// Package release to wydanie bez ponownego buildu (decyzja 40): numer
// vX.Y.Z nadany digestowi obrazu z main, który przeszedł akceptację na
// stagingu.
//
// Accept biegnie u autora, bo tylko tam jest klaster stagingu: bierze digest,
// który na nim stoi, sprawdza jego pochodzenie z main, uruchamia bramki
// akceptacji na tym właśnie wdrożeniu i zapisuje wynik w tagu z adnotacją na
// commicie obrazu. Publish biegnie w CI na wypchniętym tagu: czyta zapis
// z tagu, sprawdza obraz jeszcze raz i dopisuje w rejestrze tag z numerem
// wydania — ten sam digest, bez builda — razem z pakietem dowodów (decyzja
// 53). Atestacje wydania i pakietu wystawia potem workflow, a promocja na
// prod wymaga obu (stages/admission).
package release

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
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
	IsAncestor(ctx context.Context, commit, ref string) (bool, error)
}

// Staging — wdrożenie na klastrze akceptacji (adapters/kube).
type Staging interface {
	DeploymentImage(ctx context.Context, namespace, name, container string) (string, error)
	Application(ctx context.Context, namespace, name string) (cluster.Application, error)
}

// Gates — bramki akceptacji na wdrożeniu, które stoi na stagingu; składa je
// cmd z etapów identity, drain i ingress. Tożsamość porównuje pody z obrazem,
// który zostaje wydaniem, a nie z tym, który akurat stoi w Deploymencie.
type Gates interface {
	Identity(ctx context.Context, image string) ([]identity.Pod, error)
	Drain(ctx context.Context) (drain.Report, error)
	Ingress(ctx context.Context) (ingress.Report, error)
}

// MainRef — ref, w którym musi być commit kodu bramek: zdalny main w klonie
// autora. Kod z niescalonej gałęzi może mieć poluzowane progi, a tag
// zapisałby jego wynik tak samo jak każdy inny.
const MainRef = "origin/main"

// GatesCode — kod bramek, którym biegnie akceptacja: commit i stan drzewa
// z informacji o buildzie programu (vcs.revision, vcs.modified). Sonda drainu
// powstaje z tego samego drzewa.
type GatesCode struct {
	Commit   string
	Modified bool
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
	// Code — kod bramek z informacji o buildzie dft.
	Code GatesCode
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
	if err := a.checkCode(ctx); err != nil {
		return artifact.Release{}, err
	}

	start, err := a.target(ctx)
	if err != nil {
		return artifact.Release{}, err
	}
	image := start.image
	if !strings.Contains(image, "@sha256:") {
		return artifact.Release{}, cli.Unmet("na %s stoi %s — wydaniem może być tylko obraz z rejestru po digeście: %s -- --image <obraz>@sha256:…",
			a.Staging.Name, image, a.Staging.Task("cluster:up"))
	}
	cli.Step("na %s stoi %s", a.Staging.Name, image)
	if start.chart != "" {
		cli.Step("Argo CD: wersja charta %s — akceptacja odmówi, jeśli zmieni się w trakcie", start.chart)
	}
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

	summary, err := a.accept(ctx, start)
	if err != nil {
		return artifact.Release{}, fmt.Errorf("akceptacja %s na %s: %w", img.Ref(), a.Staging.Name, err)
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	r := artifact.Release{
		Version: version, Title: title, Image: img.Ref(), Commit: img.Commit,
		AcceptedAt: now().UTC().Format(time.RFC3339), Staging: summary, GatesCommit: a.Code.Commit,
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
// i tożsamość jeszcze raz — po drainach stoją nowe pody. Po każdej bramce
// sprawdza, że cel się nie ruszył. Zwraca podsumowanie do tagu.
func (a Acceptor) accept(ctx context.Context, start target) (string, error) {
	pods, err := a.Gates.Identity(ctx, start.image)
	if err != nil {
		return "", err
	}
	if err := a.unchanged(ctx, start, "tożsamości"); err != nil {
		return "", err
	}
	drained, err := a.Gates.Drain(ctx)
	if err != nil {
		return "", err
	}
	if err := a.unchanged(ctx, start, "drainu"); err != nil {
		return "", err
	}
	tls, err := a.Gates.Ingress(ctx)
	if err != nil {
		return "", err
	}
	if err := a.unchanged(ctx, start, "odnowienia certyfikatu"); err != nil {
		return "", err
	}
	if _, err := a.Gates.Identity(ctx, start.image); err != nil {
		return "", err
	}
	if err := a.unchanged(ctx, start, "tożsamości po drainach"); err != nil {
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
	if start.chart != "" {
		parts = append(parts, fmt.Sprintf("Argo CD na wersji charta %s przez całą akceptację", start.chart))
	}
	return strings.Join(parts, "; "), nil
}

// checkCode — akceptacja biegnie kodem bramek z zatwierdzonego commita ze
// scalonego main. Zmieniony lokalnie próg albo eksperymentalna gałąź dałyby
// zieloną akceptację, którą tag zapisałby tak samo jak każdą inną.
func (a Acceptor) checkCode(ctx context.Context) error {
	switch {
	case len(a.Code.Commit) != 40:
		return cli.Unmet("kod bramek bez commita w informacji o buildzie (vcs.revision=%q) — dft musi być zbudowany w repozytorium gita, przez ci/dft", a.Code.Commit)
	case a.Code.Modified:
		return cli.Unmet("kod bramek z niezatwierdzonymi zmianami (vcs.modified, commit %s) — akceptacja tylko kodem z zatwierdzonego commita", artifact.ShortCommit(a.Code.Commit))
	}
	merged, err := a.Git.IsAncestor(ctx, a.Code.Commit, MainRef)
	if err != nil {
		return err
	}
	if !merged {
		return cli.Unmet("kod bramek z %s, którego nie ma w %s — akceptacja tylko kodem ze scalonego main (git switch main && git pull)", artifact.ShortCommit(a.Code.Commit), MainRef)
	}
	cli.Step("kod bramek: %s z %s, bez niezatwierdzonych zmian", artifact.ShortCommit(a.Code.Commit), MainRef)
	return nil
}

// target — co stoi na stagingu: obraz Deploymentu i wersja charta, którą
// wdrożył Argo CD (pusta, gdy staging nie ma Argo).
type target struct {
	image, chart string
}

func (a Acceptor) target(ctx context.Context) (target, error) {
	image, err := a.Cluster.DeploymentImage(ctx, a.Staging.App.Namespace, a.Staging.App.Release+"-api", "api")
	if err != nil {
		return target{}, err
	}
	t := target{image: image}
	if a.Staging.GitOps() {
		app, err := a.Cluster.Application(ctx, environment.ArgoCDNamespace, a.Staging.App.Release)
		if err != nil {
			return target{}, err
		}
		t.chart = app.Revision
	}
	return t, nil
}

// unchanged — cel akceptacji się nie ruszył. Argo CD z automatyczną
// synchronizacją wdraża nowszą wersję kilka minut po publikacji, także
// w trakcie bramek; wtedy część bramek przeszłaby na innym digeście niż ten,
// który zostaje wydaniem, a tag by tego nie pokazał.
func (a Acceptor) unchanged(ctx context.Context, want target, after string) error {
	got, err := a.target(ctx)
	if err != nil {
		return err
	}
	if got != want {
		return cli.Unmet("cel akceptacji zmienił się po bramce %s: obraz %s → %s, wersja charta %q → %q — Argo CD wdrożył nowszą wersję; powtórz wydanie po synchronizacji", after, want.image, got.image, want.chart, got.chart)
	}
	return nil
}

// Tags — tagi wydań w repozytorium na GitHubie (adapters/github).
type Tags interface {
	ReleaseTag(ctx context.Context, name string) (commit, message string, err error)
}

// Registry — tagi w rejestrze (adapters/registry).
type Registry interface {
	Tag(ctx context.Context, ref, tag string) error
}

// CI — biegi i artefakty workflowu ci (adapters/github).
type CI interface {
	RunsForCommit(ctx context.Context, workflow, sha, event string) ([]ci.Run, error)
	Jobs(ctx context.Context, runID int64) ([]ci.Job, error)
	RunArtifacts(ctx context.Context, runID int64) ([]artifact.EvidenceArtifact, error)
}

// Publisher — wydanie w CI na wypchniętym tagu.
type Publisher struct {
	Repository string
	Tags       Tags
	Admission  Admission
	Registry   Registry
	// CI i Tools — źródła pakietu dowodów wydania (decyzja 53).
	CI    CI
	Tools artifact.EvidenceTools
}

// Publish czyta wydanie z tagu (zapis z Accept), sprawdza, że tag wskazuje
// na commit obrazu, a obraz pochodzi z main z tego commita, składa pakiet
// dowodów i dopisuje w rejestrze tag z numerem wydania. head — commit, na
// którym biegnie workflow (GITHUB_SHA). Pakiet przed tagiem w rejestrze:
// wydanie, którego bieg main nie przeszedł, nie dostaje numeru nigdzie.
func (p Publisher) Publish(ctx context.Context, tag, head string) (artifact.Release, artifact.Evidence, error) {
	commit, message, err := p.Tags.ReleaseTag(ctx, tag)
	if err != nil {
		return artifact.Release{}, artifact.Evidence{}, err
	}
	r, err := artifact.ParseReleaseTag(tag, message)
	if err != nil {
		return artifact.Release{}, artifact.Evidence{}, cli.Unmet("%v", err)
	}
	switch {
	case r.Commit != commit:
		return artifact.Release{}, artifact.Evidence{}, cli.Unmet("tag %s wskazuje na %s, a zapisane wydanie na %s", tag, commit, r.Commit)
	case head != commit:
		return artifact.Release{}, artifact.Evidence{}, cli.Unmet("workflow biegnie na %s, a tag %s wskazuje na %s", head, tag, commit)
	}
	admitted, err := p.Admission.Admit(ctx, r.Image, policy("wydanie", p.Repository))
	if err != nil {
		return artifact.Release{}, artifact.Evidence{}, err
	}
	if admitted.Publication.Commit != r.Commit {
		return artifact.Release{}, artifact.Evidence{}, cli.Unmet("obraz %s zbudowano z %s, a wydanie %s mówi o %s", r.Image, admitted.Publication.Commit, tag, r.Commit)
	}
	e, err := p.evidence(ctx, r)
	if err != nil {
		return artifact.Release{}, artifact.Evidence{}, err
	}
	if err := p.Registry.Tag(ctx, r.Image, r.Version); err != nil {
		return artifact.Release{}, artifact.Evidence{}, err
	}
	cli.Step("wydanie %s: %s:%s → %s (bez builda), akceptacja %s: %s", r.Version, p.Repository, r.Version, r.Image, r.AcceptedAt, r.Staging)
	return r, e, nil
}

// evidence składa pakiet dowodów wydania r (decyzja 53): biegi ci po push
// na main dla commita wydania — wyniki zadań i artefakty ze skrótami —
// i wersje narzędzi. Pakiet, który nie przechodzi Evidence.Check (ostatni
// bieg main tego commita nie był zielony z klastrem i chartem), to odmowa:
// wydanie nazywa digest, który przeszedł W1–W3 na main.
func (p Publisher) evidence(ctx context.Context, r artifact.Release) (artifact.Evidence, error) {
	e := artifact.Evidence{Release: r, Tools: p.Tools}
	runs, err := p.CI.RunsForCommit(ctx, "ci.yml", r.Commit, "push")
	if err != nil {
		return e, err
	}
	for _, run := range runs {
		jobs, err := p.CI.Jobs(ctx, run.ID)
		if err != nil {
			return e, err
		}
		artifacts, err := p.CI.RunArtifacts(ctx, run.ID)
		if err != nil {
			return e, err
		}
		e.Main = append(e.Main, artifact.EvidenceRun{Run: run, Jobs: jobs, Artifacts: artifacts})
	}
	if found := e.Check(); len(found) > 0 {
		return e, cli.Unmet("pakiet dowodów wydania %s: %s", r.Version, strings.Join(found, "; "))
	}
	cli.Step("pakiet dowodów %s: bieg main %s (%s), %d artefaktów, mise.lock %.12s", r.Version, e.Main[0].URL, e.Main[0].Conclusion, len(e.Main[0].Artifacts), p.Tools.MiseLockSHA256)
	return e, nil
}
