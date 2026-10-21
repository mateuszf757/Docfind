package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/delivery"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/gitops"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/core/platform"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/manifest"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
	"github.com/mateuszf757/Docfind/tools/internal/stages/chartpub"
	"github.com/mateuszf757/Docfind/tools/internal/stages/identity"
)

// ChartRegistry — pakiet dostawy i chart aplikacji w rejestrze OCI
// (adapters/registry): tagi do rozwiązania zakresu wersji i odczyt chartów.
type ChartRegistry interface {
	chartpub.Reader
	Tags(ctx context.Context, repository string) ([]string, error)
}

// deliveryTimeout — od założenia głównej Application do wdrożonego pakietu.
// Na nowym klastrze to cała platforma falami (CRD, webhooki, OpenBao,
// certyfikat Let's Encrypt przez DNS-01) i aplikacja.
const deliveryTimeout = 20 * time.Minute

// maxResyncs — ile razy w jednym `cluster up` dft synchronizuje jeszcze raz
// Application, którą Argo zostawił z nieudaną operacją (decyzja 82). Skok
// zegara trafia w pojedynczą synchronizację; błąd, który wraca dwa razy
// z rzędu, nie jest przejściowy — wtedy odmowa od razu, bez czekania do
// limitu.
const maxResyncs = 2

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

// release — pakiet dostawy, który Argo CD ma wdrożyć, rozstrzygnięty przed
// zmianą na klastrze: najwyższa wersja z zakresu i digest jej manifestu (do
// niego Argo rozwiązuje zakres), komponenty z pakietu, obraz aplikacji
// wpuszczony według polityki środowiska i tożsamość oczekiwana na podach.
type release struct {
	version, digest string
	bundle          delivery.Bundle
	want            artifact.Expected
}

// resolveBundle liczy z tagów w rejestrze wersję pakietu, którą Argo CD
// wybierze z zakresu (ta sama reguła i biblioteka co w Argo), czyta pakiet
// i chart aplikacji z niego i wpuszcza obraz aplikacji tą samą polityką co
// obraz podany przez --image — to on stanie na klastrze. dft nie zatrzyma
// Argo przed wdrożeniem, ale nie założy głównej Application na klastrze,
// gdy najnowszy pakiet tej polityki nie przechodzi, i zgłosi to przy każdym
// biegu; weryfikacja przy przyjęciu przez klaster to Etap 9.
func (d Deployer) resolveBundle(ctx context.Context) (release, error) {
	a := d.Env.Delivery.ArgoCD
	tags, err := d.ChartRegistry.Tags(ctx, a.Bundle)
	if err != nil {
		return release{}, err
	}
	version, err := gitops.Latest(a.Versions, tags)
	if err != nil {
		return release{}, cli.Unmet("%s: %v", a.Bundle, err)
	}
	archive, digest, err := chartpub.Archive(ctx, d.ChartRegistry, a.Bundle, a.Bundle+":"+version)
	if err != nil {
		return release{}, err
	}
	values, err := chart.ValuesOf(archive)
	if err != nil {
		return release{}, fmt.Errorf("pakiet %s:%s: %w", a.Bundle, version, err)
	}
	b, err := delivery.FromValues(values)
	if err != nil {
		return release{}, fmt.Errorf("pakiet %s:%s: %w", a.Bundle, version, err)
	}
	if _, ok := b.Environments[d.Env.Name]; !ok {
		return release{}, cli.Unmet("pakiet %s:%s nie ma wartości środowiska %s — definicja z delivery.argocd po publikacji pakietu?", a.Bundle, version, d.Env.Name)
	}
	app, ok := b.App()
	if !ok {
		return release{}, cli.Unmet("pakiet %s:%s bez aplikacji", a.Bundle, version)
	}
	appRepo := strings.TrimPrefix(app.RepoURL, "oci://")
	build, err := chartpub.Read(ctx, d.ChartRegistry, appRepo, appRepo+"@"+app.Digest)
	if err != nil {
		return release{}, err
	}
	admitted, err := d.Admission.Admit(ctx, d.Image+"@"+build.Digest, admission.For(d.Env, d.Image, ""))
	if err != nil {
		return release{}, err
	}
	commit := admitted.Publication.Artifact.Commit
	// Chart opisuje obraz tagiem z commita; obraz mówi o sobie etykietą.
	if !strings.HasPrefix(commit, build.Tag) {
		return release{}, cli.Unmet("chart aplikacji z pakietu %s:%s opisuje obraz tagiem %s, a obraz %s pochodzi z commita %s", a.Bundle, version, build.Tag, build.Digest, artifact.ShortCommit(commit))
	}
	cli.Step("Argo CD ma wdrożyć pakiet %s:%s (%s) — obraz %s@%s z commita %s, wersja %s", a.Bundle, version, digest, build.Tag, build.Digest, artifact.ShortCommit(commit), build.DeployVersion)
	return release{version: version, digest: digest, bundle: b, want: artifact.Expected{Image: d.Image + ":" + build.Tag + "@" + build.Digest, Version: build.DeployVersion, Commit: commit}}, nil
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

// gitops stawia środowisko, którym zarządza Argo CD (decyzja 81): dft
// instaluje tylko to, bez czego Argo nie ruszy (CoreDNS, sam Argo CD),
// przygotowuje to, czego Argo zrobić nie może, i zakłada główną Application.
// Resztę — platformę falami i aplikację — wdraża Argo z pakietu rel; dft
// prowadzi kroki startowe między falami i sprawdza wynik.
func (d Deployer) gitops(ctx context.Context, c Cluster, rel release) error {
	if err := d.installComponent(ctx, pins.CoreDNS, false); err != nil {
		return err
	}
	if err := d.argoCD(ctx, c); err != nil {
		return err
	}
	// Klucz odpieczętowania OpenBao musi być przed jego podami, a przestrzeń
	// aplikacji — z etykietami, które są zgodą platformy (PSA, trasy).
	ss, err := d.SecretStores(d.target())
	if err != nil {
		return err
	}
	if err := ss.Prepare(ctx); err != nil {
		return err
	}
	if err := d.namespace(ctx, c); err != nil {
		return err
	}
	a := d.Env.Delivery.ArgoCD
	cli.Step("Argo CD: główna Application %s — pakiet %s w zakresie %s", environment.RootApplication, a.Bundle, a.Versions)
	// Bez czekania Helma: korzeń jest gotowy, gdy Argo wdroży pakiet, a na
	// to dft czeka niżej z własnym warunkiem.
	if err := d.Releases.Install(ctx, d.target(), cluster.Release{
		Name: "delivery-root", Chart: d.path("deploy/charts/delivery-root"), Namespace: environment.ArgoCDNamespace,
		Set: d.Env.RootValues(d.Env.Gateway.ACMEEmail), Timeout: 2 * time.Minute, HooksOnly: true,
	}); err != nil {
		return err
	}
	// Argo trzyma listę tagów w pamięci podręcznej kilka minut — pakiet
	// opublikowany przed chwilą byłby widoczny dopiero po niej.
	if err := c.RefreshApplication(ctx, environment.ArgoCDNamespace, environment.RootApplication); err != nil {
		return err
	}
	if err := d.startup(ctx, c, ss); err != nil {
		return err
	}
	rel, err = d.awaitDelivery(ctx, c, rel)
	if err != nil {
		return err
	}
	if err := d.systemImages(ctx, c); err != nil {
		return err
	}
	if _, err := identity.Verify(ctx, c, d.Env.App.Namespace, d.Env.App.Release+"-api", "api", rel.want); err != nil {
		return err
	}
	return d.adopt(ctx, c)
}

// startup prowadzi kroki, których Argo zrobić nie może, w miejscu fal,
// w którym są potrzebne: inicjalizację i konfigurację OpenBao, gdy Argo
// założył jego zasoby (bez niej fala OpenBao nie będzie zdrowa, a za nią
// stoją platforma i aplikacja), token Cloudflare i sekrety aplikacji
// w OpenBao, a na końcu CA platformy w przestrzeni aplikacji dla jej
// SecretStore.
func (d Deployer) startup(ctx context.Context, c Cluster, ss SecretStore) error {
	interval, timeout := d.waits()
	if err := d.waitApplication(ctx, c, pins.OpenBao, interval, timeout, func(app cluster.Application) (bool, string) {
		return app.Sync == "Synced", "synchronizacja " + orUnknown(app.Sync)
	}); err != nil {
		return err
	}
	if _, err := ss.Bootstrap(ctx); err != nil {
		return err
	}
	if strings.HasPrefix(d.Env.Gateway.Issuer, "letsencrypt") {
		tokens, err := d.DNSTokens(d.target())
		if err != nil {
			return err
		}
		if err := tokens.Ensure(ctx, d.Env.Task("dns-token")); err != nil {
			return err
		}
	}
	if err := ss.EnsureAppSecrets(ctx); err != nil {
		return err
	}
	var ca []byte
	if err := waitReady(ctx, "Secret cert-manager/docfind-root-ca (CA platformy)", timeout, func(ctx context.Context) (bool, error) {
		data, err := c.SecretData(ctx, "cert-manager", "docfind-root-ca")
		ca = data["ca.crt"]
		return err == nil && len(ca) > 0, nil
	}); err != nil {
		return err
	}
	cli.Step("CA platformy w %s/%s dla SecretStore aplikacji", d.Env.App.Namespace, environment.PlatformCAConfigMap)
	return c.EnsureConfigMap(ctx, d.Env.App.Namespace, environment.PlatformCAConfigMap, map[string]string{"ca.crt": string(ca)})
}

func orUnknown(s string) string {
	if s == "" {
		return "nieznana"
	}
	return s
}

// waitApplication czeka, aż Application name spełni warunek ready (ready
// zwraca też, na co czeka — do komunikatu).
func (d Deployer) waitApplication(ctx context.Context, c Cluster, name string, interval, timeout time.Duration, ready func(cluster.Application) (bool, string)) error {
	var last string
	err := poll.Until(ctx, timeout, interval, func(ctx context.Context) (bool, error) {
		app, err := c.Application(ctx, environment.ArgoCDNamespace, name)
		if err != nil {
			last = err.Error()
			return false, nil
		}
		done, waiting := ready(app)
		if !done && waiting != last {
			cli.Step("Argo CD: %s — czekam: %s", name, waiting)
		}
		last = waiting
		return done, nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		return cli.Unmet("Argo CD: %s po %v — %s", name, timeout, last)
	}
	return err
}

// awaitDelivery czeka, aż główna Application wdroży pakiet rel, a każda
// Application komponentu — swój chart z pakietu: zsynchronizowana z tym
// digestem i zdrowa. Zdrowie korzenia to zdrowie Applications komponentów,
// ale komponent zdrowy na poprzednim digeście też jest „zdrowy", więc każdy
// sprawdzany jest osobno. Gdy Argo wdraża inny pakiet — CI opublikował
// nowszy w trakcie — dft rozstrzyga zakres jeszcze raz i wpuszcza nowy obraz
// tą samą polityką, zamiast czekać na pakiet, którego Argo już nie wybierze.
// Application zsynchronizowaną z nieudaną operacją, której Argo nie ponowi
// (gitops.Stalled), dft synchronizuje jeszcze raz — najwyżej maxResyncs razy.
func (d Deployer) awaitDelivery(ctx context.Context, c Cluster, rel release) (release, error) {
	interval, timeout := d.waits()
	var last string
	resynced := map[string]int{}
	// check — na co czeka Application name, która ma stać na want; pusty
	// napis — wdrożona.
	check := func(ctx context.Context, name string, app cluster.Application, want string) (string, error) {
		if gitops.Stalled(app, want) {
			if resynced[name] >= maxResyncs {
				return "", cli.Unmet("Argo CD: %s — operacja %s na %s także po %d ponownych synchronizacjach: %s", name, app.Operation.Phase, want, maxResyncs, app.Operation.Message)
			}
			if err := c.SyncApplication(ctx, environment.ArgoCDNamespace, name, want); err != nil {
				// Konflikt z zapisem Argo albo operacja zlecona w międzyczasie
				// — następna runda zobaczy nowy stan.
				return name + ": ponowna synchronizacja nie zlecona: " + err.Error(), nil
			}
			resynced[name]++
			cli.Warn("Argo CD: %s — operacja %s na %s: %s; Application jest zsynchronizowana, więc Argo sam jej nie ponowi — zlecam ponowną synchronizację (%d z %d, decyzja 82)", name, app.Operation.Phase, want, app.Operation.Message, resynced[name], maxResyncs)
			return name + ": ponowna synchronizacja zlecona", nil
		}
		if done, why := gitops.Deployed(app, want); !done {
			return name + ": " + why, nil
		}
		return "", nil
	}
	err := poll.Until(ctx, timeout, interval, func(ctx context.Context) (bool, error) {
		root, err := c.Application(ctx, environment.ArgoCDNamespace, environment.RootApplication)
		if err != nil {
			last = err.Error()
			return false, nil
		}
		if root.Revision != "" && root.Revision != rel.digest && strings.HasPrefix(root.Revision, "sha256:") {
			next, err := d.resolveBundle(ctx)
			if err != nil {
				return false, err
			}
			if next.digest == root.Revision {
				rel = next
			}
		}
		waiting := []string{}
		why, err := check(ctx, environment.RootApplication, root, rel.digest)
		if err != nil {
			return false, err
		}
		if why != "" {
			waiting = append(waiting, why)
		}
		for _, comp := range rel.bundle.Components {
			app, err := c.Application(ctx, environment.ArgoCDNamespace, comp.Name)
			if err != nil {
				waiting = append(waiting, comp.Name+": "+err.Error())
				continue
			}
			why, err := check(ctx, comp.Name, app, comp.Digest)
			if err != nil {
				return false, err
			}
			if why != "" {
				waiting = append(waiting, why)
			}
		}
		state := strings.Join(waiting, "; ")
		if len(waiting) > 0 && state != last {
			cli.Step("Argo CD: czekam — %s", state)
		}
		last = state
		return len(waiting) == 0, nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		return rel, cli.Unmet("Argo CD nie wdrożył pakietu %s (%s) w %v: %s", rel.version, rel.digest, timeout, last)
	}
	if err != nil {
		return rel, err
	}
	cli.Step("Argo CD wdrożył pakiet %s: %d komponentów zsynchronizowanych i zdrowych", rel.version, len(rel.bundle.Components))
	return rel, nil
}

// adopted — wydania Helma, które przejmuje Argo CD w środowisku z GitOps:
// komponenty lustra, chart platformy, aplikacja i chart dostawy z kroku 2
// (decyzja 46), który zakładał projekt i Application aplikacji.
func (d Deployer) adopted() [][2]string {
	var out [][2]string
	for _, pc := range platform.Mirrored() {
		out = append(out, [2]string{pc.Namespace, pc.Pin})
	}
	return append(out,
		[2]string{d.Env.Gateway.Namespace, "platform"},
		[2]string{d.Env.App.Namespace, d.Env.App.Release},
		[2]string{environment.ArgoCDNamespace, "delivery"},
	)
}

// adopt kończy przejęcie wydań Helma przez Argo CD: z każdego zasobu wydania
// znika wpis menedżera „helm" w managedFields (jeśli zasobem zarządza już
// Argo), a potem zapis wydania (Secrety sh.helm.release.v1.<wydanie>.*).
// Zasoby zostają. Bez pierwszego pola, które Helm zastosował przez
// Server-Side Apply, zostałyby jego własnością i nie znikałyby, gdy znikną
// z charta stosowanego przez Argo; bez drugiego `helm list` pokazywałby
// wydanie, którym nikt nie zarządza, a `helm rollback` albo `helm uninstall`
// zmieniłby zasoby Argo za jego plecami.
func (d Deployer) adopt(ctx context.Context, c Cluster) error {
	for _, nr := range d.adopted() {
		ns, name := nr[0], nr[1]
		records, err := c.HelmReleaseRecords(ctx, ns, name)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			continue
		}
		raw, err := d.Releases.Manifest(ctx, d.target(), ns, name)
		if err != nil {
			return err
		}
		docs, err := manifest.Parse(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("manifest wydania %s/%s: %w", ns, name, err)
		}
		stripped := 0
		for _, doc := range docs {
			meta := manifest.Map(doc, "metadata")
			objNS := manifest.String(meta, "namespace")
			if objNS == "" {
				objNS = ns
			}
			changed, err := c.RemoveFieldManager(ctx, manifest.String(doc, "apiVersion"), manifest.String(doc, "kind"), objNS, manifest.String(meta, "name"), "helm", gitops.ArgoManager)
			if err != nil {
				return err
			}
			if changed {
				stripped++
			}
		}
		cli.Step("Argo CD przejął wydanie Helma %s/%s: menedżer helm zdjęty z %d z %d zasobów, zapis wydania (%d rewizji) usunięty — zasoby zostają", ns, name, stripped, len(docs), len(records))
		for _, r := range records {
			if err := c.DeleteSecret(ctx, ns, r); err != nil {
				return err
			}
		}
	}
	return nil
}
