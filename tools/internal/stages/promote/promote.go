// Package promote to promocja digestu wydania na środowisko (decyzja 38):
// ten sam obraz, który przeszedł sprawdzenia, po digeście — nigdy build od
// nowa dla kolejnego środowiska.
//
// W CI promocja biegnie w zadaniu z `environment: production`, więc rusza
// dopiero po zgodzie autora; etap najpierw sprawdza, że ta zgoda w ogóle
// jest wymagana. Potem wpuszcza obraz tak, jak przed wdrożeniem na to
// środowisko (stages/admission). Do Etapu 10 prod nie ma klastra: promocja
// kończy się zapisem i instalacją, jaką wykonałby klient.
package promote

import (
	"context"
	"fmt"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
)

// Approvals — reguły środowisk GitHuba (adapters/github).
type Approvals interface {
	RequiresApproval(ctx context.Context, repo, env string) (bool, error)
}

// Admission — wpuszczanie obrazu z rejestru (stages/admission).
type Admission interface {
	Admit(ctx context.Context, ref string, p admission.Policy) (admission.Admitted, error)
}

// Release — promowany obraz.
type Release struct {
	// Image — repozytorium:tag@digest, jak w charcie.
	Image   string `json:"image"`
	Tag     string `json:"tag"`
	Digest  string `json:"digest"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Promotion — raport promocji.
type Promotion struct {
	Environment string  `json:"environment"`
	Release     Release `json:"release"`
	// Install — instalacja u klienta: chart z tagu wydania, obraz po
	// digeście i wartości z definicji środowiska.
	Install string `json:"install"`
	// Run — bieg workflowu, który promował.
	Run string `json:"run,omitempty"`
}

// Promoter promuje obrazy aplikacji Repository na środowisko Env.
type Promoter struct {
	Env        environment.Definition
	Repository string
	Admission  Admission
	// Approvals i GitHubRepo — sprawdzenie, że środowisko GitHuba o nazwie
	// GitHubEnvironment wymaga zgody; nil poza CI, gdzie żadne zadanie nie
	// czeka na zgodę.
	Approvals         Approvals
	GitHubRepo        string
	GitHubEnvironment string
	// Run — link do biegu workflowu (do raportu).
	Run string
}

// Promote promuje obraz ref (repozytorium@sha256:…).
func (p Promoter) Promote(ctx context.Context, ref string) (Promotion, error) {
	cli.Step("promocja na %s (%s)", p.Env.Name, p.Env.Stage)
	if p.Approvals != nil {
		// GitHub tworzy środowisko bez żadnych reguł przy pierwszym zadaniu,
		// które go użyje — zanim autor zastosuje .github/environments/,
		// zadanie z `environment: production` przeszłoby bez nikogo.
		required, err := p.Approvals.RequiresApproval(ctx, p.GitHubRepo, p.GitHubEnvironment)
		if err != nil {
			return Promotion{}, err
		}
		if !required {
			return Promotion{}, cli.Unmet("środowisko GitHuba %s nie wymaga zgody — zastosuj .github/environments/ (./bin/mise run repo:settings, konto admina)", p.GitHubEnvironment)
		}
		cli.Step("środowisko GitHuba %s wymaga zgody — zadanie ruszyło po niej", p.GitHubEnvironment)
	}
	if !p.Env.AllowsSource(environment.SourceRegistry) {
		return Promotion{}, cli.Unmet("środowisko %s nie przyjmuje obrazów z rejestru (artifact.sources: %v)", p.Env.Name, p.Env.Artifact.Sources)
	}
	admitted, err := p.Admission.Admit(ctx, ref, admission.For(p.Env, p.Repository))
	if err != nil {
		return Promotion{}, err
	}
	a := admitted.Publication.Artifact
	release := Release{Image: a.Pinned(), Tag: a.Tag(), Digest: a.Digest, Version: a.Version, Commit: a.Commit}

	// Chart OCI zastąpi ścieżkę w Etapie 10.
	values := append(p.Env.AppValues(), "api.image.tag="+release.Tag, "api.image.digest="+release.Digest)
	var install strings.Builder
	fmt.Fprintf(&install, "git checkout v%s\nhelm upgrade --install %s deploy/charts/docfind --namespace %s", release.Version, p.Env.App.Release, p.Env.App.Namespace)
	for _, v := range values {
		fmt.Fprintf(&install, " \\\n  --set %s", v)
	}
	if p.Env.Cluster.Provider == environment.ProviderNone {
		cli.Step("środowisko %s nie ma jeszcze klastra (Etap 10): zapis promocji i instalacja dla klienta", p.Env.Name)
	}
	cli.Step("promowany %s (wersja %s, commit %.12s)", release.Image, release.Version, release.Commit)
	return Promotion{Environment: p.Env.Name, Release: release, Install: install.String(), Run: p.Run}, nil
}

// Summary — promocja jako Markdown do podsumowania biegu.
func (p Promotion) Summary() string {
	return fmt.Sprintf("### Promocja na %s\n\n| Wersja | Obraz | Commit |\n|---|---|---|\n| %s | `%s` | `%s` |\n\nInstalacja u klienta:\n\n```bash\n%s\n```\n",
		p.Environment, p.Release.Version, p.Release.Image, p.Release.Commit, p.Install)
}
