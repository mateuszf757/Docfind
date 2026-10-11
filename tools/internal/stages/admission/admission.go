// Package admission to wpuszczanie obrazu z rejestru na środowisko —
// jedna polityka dla wdrożenia na klaster, promocji na środowisko bez
// klastra i bramki atestacji: repozytorium aplikacji, publikacja
// w rejestrze (indeks z atestacją, wersja i commit w etykietach), wydanie
// tam, gdzie definicja przyjmuje tylko wydania, i atestacja GitHuba
// z commitem obrazu.
package admission

import (
	"context"
	"errors"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Registry — publikacja w rejestrze (adapters/registry).
type Registry interface {
	Inspect(ctx context.Context, ref string) (artifact.Publication, error)
}

// Attestations — atestacje GitHuba (adapters/github).
type Attestations interface {
	Verify(ctx context.Context, ref, sourceRef string) (artifact.Provenance, error)
}

// Policy — czego środowisko wymaga od obrazu z rejestru.
type Policy struct {
	// Environment — nazwa środowiska, do komunikatów.
	Environment string
	// Repository — jedyne repozytorium, z którego obrazy wchodzą; puste —
	// dowolne (bramka atestacji sprawdza wskazany obraz).
	Repository string
	// ReleasesOnly — tylko wydania X.Y.Z.
	ReleasesOnly bool
	// Attested — pochodzenie i SBOM w atestacji BuildKitu oraz atestacja
	// GitHuba (decyzja 37).
	Attested bool
}

// For zwraca politykę środowiska dla obrazu aplikacji repository.
func For(e environment.Definition, repository string) Policy {
	return Policy{Environment: e.Name, Repository: repository, ReleasesOnly: e.Artifact.ReleasesOnly, Attested: e.Artifact.Attested}
}

// Admitted — obraz przyjęty na środowisko.
type Admitted struct {
	// Publication — publikacja w rejestrze z wersją i commitem z etykiet.
	Publication artifact.Publication `json:"registry"`
	// Provenance — zweryfikowana atestacja GitHuba; nil, gdy środowisko jej
	// nie wymaga.
	Provenance *artifact.Provenance `json:"github,omitempty"`
}

// Gate sprawdza obrazy.
type Gate struct {
	Registry Registry
	// Attestations — wymagane, gdy polityka wymaga atestacji.
	Attestations Attestations
}

// Admit sprawdza obraz ref (repozytorium[:tag]@sha256:…) według polityki,
// bez zmiany czegokolwiek. Tag w referencji niczego nie wybiera — liczy się
// digest: tag w rejestrze da się nadpisać, digest nie.
func (g Gate) Admit(ctx context.Context, ref string, p Policy) (Admitted, error) {
	repository, digest, err := artifact.ParseRef(ref)
	if err != nil {
		return Admitted{}, cli.Usage("obraz %q: oczekiwano repozytorium@sha256:…", ref)
	}
	if p.Repository != "" && repository != p.Repository {
		return Admitted{}, cli.Unmet("obraz z repozytorium %s, a aplikacja to %s — inne obrazy na środowisko %s nie trafiają", repository, p.Repository, p.Environment)
	}
	plain := repository + "@" + digest
	published, err := g.Registry.Inspect(ctx, plain)
	if err != nil {
		return Admitted{}, err
	}
	version, commit := published.Version, published.Commit
	if version == "" || commit == "" {
		return Admitted{}, cli.Unmet("%s: obraz bez etykiet wersji i commita", plain)
	}
	if p.ReleasesOnly && !artifact.IsRelease(version) {
		return Admitted{}, cli.Unmet("środowisko %s przyjmuje tylko wydania, a %s to wersja %s", p.Environment, plain, version)
	}
	admitted := Admitted{Publication: published}
	if p.Attested {
		if err := published.RequireAttestations(); err != nil {
			return Admitted{}, err
		}
		if g.Attestations == nil {
			return Admitted{}, errors.New("polityka wymaga atestacji, a bramka nie ma weryfikatora")
		}
		// Wydanie musi pochodzić z tagu swojej wersji — obraz z main o tej
		// samej treści ma inny digest (decyzja 30), ale tag można by też
		// przesunąć na inny commit; podpis mówi, z którego zbudowano.
		sourceRef := ""
		if artifact.IsRelease(version) {
			sourceRef = "refs/tags/v" + version
		}
		verified, err := g.Attestations.Verify(ctx, plain, sourceRef)
		if err != nil {
			return Admitted{}, err
		}
		if verified.SourceCommit != commit {
			return Admitted{}, cli.Unmet("atestacja GitHuba mówi o commicie %s, a obraz deklaruje %s", verified.SourceCommit, commit)
		}
		admitted.Provenance = &verified
		cli.Step("atestacja GitHuba: %s z %s (%s), %s, bieg %s", verified.PredicateType, verified.SourceRef, artifact.ShortCommit(verified.SourceCommit), verified.Runner, verified.Run)
	}
	cli.Step("rejestr: %s, atestacja %v, wersja %s, commit %s", published.Digest, published.Predicates, version, artifact.ShortCommit(commit))
	return admitted, nil
}
