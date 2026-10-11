// Package release to wydanie bez ponownego buildu (decyzja 40): numer
// vX.Y.Z nadany digestowi obrazu z main, który przeszedł akceptację na
// stagingu.
//
// Accept biegnie u autora, bo tylko tam jest klaster stagingu: bierze digest,
// który na nim stoi, sprawdza jego pochodzenie z main, uruchamia bramki
// akceptacji na tym właśnie wdrożeniu i zapisuje wynik w tagu z adnotacją na
// commicie obrazu. Publish biegnie w CI na wypchniętym tagu: czyta zapis
// z tagu, sprawdza obraz jeszcze raz i dopisuje w rejestrze tag z numerem
// wydania — ten sam digest, bez builda. Atestację wydania wystawia potem
// workflow, a promocja na prod wymaga jej (stages/admission).
package release

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
	"github.com/mateuszf757/Docfind/tools/internal/stages/drain"
	"github.com/mateuszf757/Docfind/tools/internal/stages/identity"
	"github.com/mateuszf757/Docfind/tools/internal/stages/ingress"
)

// Admission — wpuszczanie obrazu z rejestru (stages/admission).
type Admission interface {
	Admit(ctx context.Context, ref string, p admission.Policy) (admission.Admitted, error)
}

// policy — wydaniem może być tylko obraz aplikacji z pochodzeniem z main
// i SBOM, niezależnie od tego, czego wymaga definicja stagingu.
func policy(env, repository string) admission.Policy {
	return admission.Policy{Environment: env, Repository: repository, Attested: true}
}

// Git — repozytorium lokalne (adapters/git).
type Git interface {
	TagExists(ctx context.Context, name string) (bool, error)
	LatestRelease(ctx context.Context, commit string) (string, error)
	CreateTag(ctx context.Context, name, commit, message string) error
}

// Staging — wdrożenie na klastrze akceptacji (adapters/kube).
type Staging interface {
	DeploymentImage(ctx context.Context, namespace, name, container string) (string, error)
}

// Gates — bramki akceptacji na wdrożeniu, które stoi na stagingu; składa je
// cmd z etapów identity, drain i ingress.
type Gates interface {
	Identity(ctx context.Context) ([]identity.Pod, error)
	Drain(ctx context.Context) (drain.Report, error)
	Ingress(ctx context.Context) (ingress.Report, error)
}

// Acceptor — akceptacja na stagingu i tag wydania.
type Acceptor struct {
	// Staging — definicja środowiska akceptacji.
	Staging    environment.Definition
	Repository string
	Cluster    Staging
	Admission  Admission
	Gates      Gates
	Git        Git
	// Now — zegar akceptacji; nil oznacza time.Now.
	Now func() time.Time
}

// Accept akceptuje na stagingu obraz, który tam stoi, i zakłada tag wydania
// tag (vX.Y.Z) na jego commicie. title — pierwsza linia tagu (co zawiera
// wydanie); pusty — „Wydanie X.Y.Z". Tagu nie wypycha — to robi autor.
func (a Acceptor) Accept(ctx context.Context, tag, title string) (artifact.Release, error) {
	version, ok := artifact.ReleaseOfTag(tag)
	if !ok {
		return artifact.Release{}, cli.Usage("wydanie %q: oczekiwano tagu vX.Y.Z (decyzja 30)", tag)
	}
	if exists, err := a.Git.TagExists(ctx, tag); err != nil {
		return artifact.Release{}, err
	} else if exists {
		return artifact.Release{}, cli.Unmet("tag %s już istnieje — tagów wydań się nie przesuwa, pomyłkę poprawia kolejny numer", tag)
	}
	if err := a.Staging.RequireCluster(); err != nil {
		return artifact.Release{}, err
	}

	deployment := a.Staging.App.Release + "-api"
	image, err := a.Cluster.DeploymentImage(ctx, a.Staging.App.Namespace, deployment, "api")
	if err != nil {
		return artifact.Release{}, err
	}
	if !strings.Contains(image, "@sha256:") {
		return artifact.Release{}, cli.Unmet("na %s stoi %s — wydaniem może być tylko obraz z rejestru po digeście: %s -- --image <obraz>@sha256:…",
			a.Staging.Name, image, a.Staging.Task("cluster:up"))
	}
	cli.Step("na %s stoi %s", a.Staging.Name, image)
	admitted, err := a.Admission.Admit(ctx, image, policy(a.Staging.Name, a.Repository))
	if err != nil {
		return artifact.Release{}, err
	}
	img := admitted.Publication.Artifact
	latest, err := a.Git.LatestRelease(ctx, img.Commit)
	if err != nil {
		return artifact.Release{}, err
	}
	if latest != "" && artifact.CompareReleases(version, latest) <= 0 {
		return artifact.Release{}, cli.Unmet("wydanie %s nie jest późniejsze niż %s, z którego wyrósł commit %s", version, latest, artifact.ShortCommit(img.Commit))
	}

	summary, err := a.accept(ctx)
	if err != nil {
		return artifact.Release{}, fmt.Errorf("akceptacja %s na %s: %w", img.Ref(), a.Staging.Name, err)
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	r := artifact.Release{
		Version: version, Title: title, Image: img.Ref(), Commit: img.Commit,
		AcceptedAt: now().UTC().Format(time.RFC3339), Staging: summary,
	}
	if err := r.Validate(); err != nil {
		return artifact.Release{}, err
	}
	if err := a.Git.CreateTag(ctx, tag, img.Commit, r.TagMessage()); err != nil {
		return artifact.Release{}, err
	}
	cli.Step("tag %s na %s: %s", tag, artifact.ShortCommit(img.Commit), summary)
	return r, nil
}

// accept uruchamia bramki akceptacji na wdrożeniu stagingu: tożsamość,
// drain każdego węzła pod ruchem, odnowienie certyfikatu pod ruchem
// i tożsamość jeszcze raz — po drainach stoją nowe pody. Zwraca
// podsumowanie do tagu.
func (a Acceptor) accept(ctx context.Context) (string, error) {
	pods, err := a.Gates.Identity(ctx)
	if err != nil {
		return "", err
	}
	drained, err := a.Gates.Drain(ctx)
	if err != nil {
		return "", err
	}
	tls, err := a.Gates.Ingress(ctx)
	if err != nil {
		return "", err
	}
	if _, err := a.Gates.Identity(ctx); err != nil {
		return "", err
	}
	requests := 0
	for _, d := range drained.Drains {
		requests += d.Summary.Total
	}
	parts := []string{
		fmt.Sprintf("tożsamość na %d podach przed drainami i po nich", len(pods)),
		fmt.Sprintf("drain %d węzłów, %d żądań, 0 nieudanych", len(drained.Drains), requests),
	}
	if tls.RenewalSkipped != "" {
		parts = append(parts, fmt.Sprintf("TLS %s bez odnowienia (%s)", tls.Hostname, tls.RenewalSkipped))
	} else {
		parts = append(parts, fmt.Sprintf("odnowienie certyfikatu %s (rewizja %d → %d), %d żądań, 0 nieudanych", tls.Hostname, tls.RevisionBefore, tls.RevisionAfter, tls.Requests))
	}
	return strings.Join(parts, "; "), nil
}

// Tags — tagi wydań w repozytorium na GitHubie (adapters/github).
type Tags interface {
	ReleaseTag(ctx context.Context, name string) (commit, message string, err error)
}

// Registry — tagi w rejestrze (adapters/registry).
type Registry interface {
	Tag(ctx context.Context, ref, tag string) error
}

// Publisher — wydanie w CI na wypchniętym tagu.
type Publisher struct {
	Repository string
	Tags       Tags
	Admission  Admission
	Registry   Registry
}

// Publish czyta wydanie z tagu (zapis z Accept), sprawdza, że tag wskazuje
// na commit obrazu, a obraz pochodzi z main z tego commita, i dopisuje
// w rejestrze tag z numerem wydania. head — commit, na którym biegnie
// workflow (GITHUB_SHA).
func (p Publisher) Publish(ctx context.Context, tag, head string) (artifact.Release, error) {
	commit, message, err := p.Tags.ReleaseTag(ctx, tag)
	if err != nil {
		return artifact.Release{}, err
	}
	r, err := artifact.ParseReleaseTag(tag, message)
	if err != nil {
		return artifact.Release{}, cli.Unmet("%v", err)
	}
	switch {
	case r.Commit != commit:
		return artifact.Release{}, cli.Unmet("tag %s wskazuje na %s, a zapisane wydanie na %s", tag, commit, r.Commit)
	case head != commit:
		return artifact.Release{}, cli.Unmet("workflow biegnie na %s, a tag %s wskazuje na %s", head, tag, commit)
	}
	admitted, err := p.Admission.Admit(ctx, r.Image, policy("wydanie", p.Repository))
	if err != nil {
		return artifact.Release{}, err
	}
	if admitted.Publication.Commit != r.Commit {
		return artifact.Release{}, cli.Unmet("obraz %s zbudowano z %s, a wydanie %s mówi o %s", r.Image, admitted.Publication.Commit, tag, r.Commit)
	}
	if err := p.Registry.Tag(ctx, r.Image, r.Version); err != nil {
		return artifact.Release{}, err
	}
	cli.Step("wydanie %s: %s:%s → %s (bez builda), akceptacja %s: %s", r.Version, p.Repository, r.Version, r.Image, r.AcceptedAt, r.Staging)
	return r, nil
}
