package deploy

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/gitops"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
	"github.com/mateuszf757/Docfind/tools/internal/stages/chartpub"
	"github.com/mateuszf757/Docfind/tools/internal/stages/identity"
)

// ChartRegistry — chart aplikacji w rejestrze OCI (adapters/registry): tagi
// do rozwiązania zakresu wersji i odczyt charta.
type ChartRegistry interface {
	chartpub.Reader
	Tags(ctx context.Context, repository string) ([]string, error)
}

// deliveryTimeout — od założenia Application do wdrożonej wersji: pobranie
// charta i obrazu, rollout dwóch replik, synchronizacja ExternalSecret.
const deliveryTimeout = 10 * time.Minute

// waits — odstęp sprawdzania i limit czekania na Argo CD (zero — domyślne).
func (d Deployer) waits() (interval, timeout time.Duration) {
	interval, timeout = d.Poll, d.DeliveryTimeout
	if interval == 0 {
		interval = 5 * time.Second
	}
	if timeout == 0 {
		timeout = deliveryTimeout
	}
	return interval, timeout
}

// release — wersja charta, którą Argo CD ma wdrożyć, rozstrzygnięta przed
// zmianą na klastrze: najwyższa wersja z zakresu, obraz z jej wartości
// wpuszczony według polityki środowiska i tożsamość oczekiwana na podach.
type release struct {
	version string
	want    artifact.Expected
}

// resolveChart liczy z tagów w rejestrze wersję, którą Argo CD wybierze
// z zakresu (ta sama reguła i biblioteka co w Argo), czyta chart w tej
// wersji i wpuszcza obraz z jego wartości tą samą polityką co obraz podany
// przez --image — to on stanie na klastrze. dft nie zatrzyma Argo przed
// wdrożeniem, ale nie założy Application na klastrze, gdy najnowszy chart
// tej polityki nie przechodzi, i zgłosi to przy każdym biegu; weryfikacja
// przy przyjęciu przez klaster to Etap 9.
func (d Deployer) resolveChart(ctx context.Context) (release, error) {
	a := d.Env.Delivery.ArgoCD
	tags, err := d.ChartRegistry.Tags(ctx, a.Chart)
	if err != nil {
		return release{}, err
	}
	version, err := gitops.Latest(a.Versions, tags)
	if err != nil {
		return release{}, cli.Unmet("%s: %v", a.Chart, err)
	}
	b, err := chartpub.Read(ctx, d.ChartRegistry, a.Chart, a.Chart+":"+version)
	if err != nil {
		return release{}, err
	}
	admitted, err := d.Admission.Admit(ctx, d.Image+"@"+b.Digest, admission.For(d.Env, d.Image, ""))
	if err != nil {
		return release{}, err
	}
	commit := admitted.Publication.Artifact.Commit
	// Chart opisuje obraz tagiem z commita; obraz mówi o sobie etykietą.
	if !strings.HasPrefix(commit, b.Tag) {
		return release{}, cli.Unmet("chart %s:%s opisuje obraz tagiem %s, a obraz %s pochodzi z commita %s", a.Chart, version, b.Tag, b.Digest, artifact.ShortCommit(commit))
	}
	cli.Step("Argo CD ma wdrożyć chart %s:%s — obraz %s@%s z commita %s, wersja %s", a.Chart, version, b.Tag, b.Digest, artifact.ShortCommit(commit), b.DeployVersion)
	return release{version: version, want: artifact.Expected{Image: d.Image + ":" + b.Tag + "@" + b.Digest, Version: b.DeployVersion, Commit: commit}}, nil
}

// argoCD instaluje Argo CD (decyzja 46) w przestrzeni nazw z Pod Security
// restricted: każdy jego kontener ją spełnia, a API server odrzuci pod,
// który przestanie.
func (d Deployer) argoCD(ctx context.Context, c Cluster) error {
	labels, err := d.restricted()
	if err != nil {
		return err
	}
	if err := c.EnsureNamespace(ctx, environment.ArgoCDNamespace, labels); err != nil {
		return err
	}
	return d.installComponent(ctx, pins.ArgoCD, false)
}

// deliver zakłada projekt i Application Argo CD (chart deploy/charts/delivery),
// czeka, aż Argo wdroży wersję rel, i sprawdza ją na podach jak każde
// wdrożenie. Na końcu usuwa zapis wydania Helma aplikacji, jeśli ten klaster
// pamięta wdrożenia sprzed Argo.
func (d Deployer) deliver(ctx context.Context, c Cluster, rel release) error {
	caBundle, err := d.appSecrets(ctx, c)
	if err != nil {
		return err
	}
	a := d.Env.Delivery.ArgoCD
	cli.Step("Argo CD: Application %s — %s w zakresie %s", d.Env.App.Release, a.Chart, a.Versions)
	// Bez czekania Helma: Application jest gotowa, gdy Argo wdroży
	// aplikację, a na to dft czeka niżej z własnym warunkiem.
	if err := d.Releases.Install(ctx, d.target(), cluster.Release{
		Name: "delivery", Chart: d.path("deploy/charts/delivery"), Namespace: environment.ArgoCDNamespace,
		Set: d.Env.DeliveryValues(caBundle), Timeout: 2 * time.Minute, HooksOnly: true,
	}); err != nil {
		return err
	}
	// Argo trzyma listę tagów charta w pamięci podręcznej kilka minut —
	// wersja opublikowana przed chwilą byłaby widoczna dopiero po niej.
	if err := c.RefreshApplication(ctx, environment.ArgoCDNamespace, d.Env.App.Release); err != nil {
		return err
	}
	rel, err = d.awaitDelivery(ctx, c, rel)
	if err != nil {
		return err
	}
	if _, err := identity.Verify(ctx, c, d.Env.App.Namespace, d.Env.App.Release+"-api", "api", rel.want); err != nil {
		return err
	}
	return d.adoptRelease(ctx, c)
}

// awaitDelivery czeka, aż Application wdroży wersję rel. Gdy Argo wdraża
// wersję wyższą — CI opublikował chart w trakcie — dft rozstrzyga zakres
// jeszcze raz i wpuszcza nowy obraz tą samą polityką, zamiast czekać na
// wersję, której Argo już nie wybierze.
func (d Deployer) awaitDelivery(ctx context.Context, c Cluster, rel release) (release, error) {
	interval, timeout := d.waits()
	var last string
	err := poll.Until(ctx, timeout, interval, func(ctx context.Context) (bool, error) {
		app, err := c.Application(ctx, environment.ArgoCDNamespace, d.Env.App.Release)
		if err != nil {
			last = err.Error()
			return false, nil
		}
		if gitops.Newer(app.Revision, rel.version) {
			next, err := d.resolveChart(ctx)
			if err != nil {
				return false, err
			}
			rel = next
		}
		done, waiting := gitops.Deployed(app, rel.version)
		if waiting != last && !done {
			cli.Step("Argo CD: czekam — %s", waiting)
		}
		last = waiting
		return done, nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		return rel, cli.Unmet("Argo CD nie wdrożył wersji %s w %v: %s", rel.version, timeout, last)
	}
	if err != nil {
		return rel, err
	}
	cli.Step("Argo CD wdrożył wersję %s: zsynchronizowana i zdrowa", rel.version)
	return rel, nil
}

// adoptRelease usuwa zapis wydania Helma aplikacji (Secrety
// sh.helm.release.v1.<wydanie>.*), gdy aplikacją zarządza już Argo CD:
// zasoby zostają, znika tylko historia Helma. Bez tego `helm list`
// pokazywałby wydanie, którym nikt nie zarządza, a `helm rollback` albo
// `helm uninstall` zmieniłby zasoby Argo za jego plecami.
func (d Deployer) adoptRelease(ctx context.Context, c Cluster) error {
	ns, name := d.Env.App.Namespace, d.Env.App.Release
	records, err := c.HelmReleaseRecords(ctx, ns, name)
	if err != nil || len(records) == 0 {
		return err
	}
	cli.Step("aplikacją zarządza Argo CD: usuwam zapis wydania Helma %s/%s (%d rewizji) — zasoby zostają", ns, name, len(records))
	for _, r := range records {
		if err := c.DeleteSecret(ctx, ns, r); err != nil {
			return err
		}
	}
	return nil
}
