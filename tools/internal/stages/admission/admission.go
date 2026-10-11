// Package admission to wpuszczanie obrazu z rejestru na środowisko —
// jedna polityka dla wdrożenia na klaster, wydania, promocji i bramki
// atestacji: repozytorium aplikacji, publikacja w rejestrze (indeks
// z atestacją, commit w etykiecie), pochodzenie z main podpisane przez
// GitHuba, a tam, gdzie definicja przyjmuje tylko wydania, atestacja
// wydania o podanym numerze (decyzja 40).
package admission

import (
	"context"
	"errors"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// MainRef — jedyne źródło obrazów z atestacją: obraz buduje się raz, na
// main, a wydanie nadaje mu numer bez ponownego buildu (decyzja 40).
const MainRef = "refs/heads/main"

// Registry — publikacja w rejestrze (adapters/registry).
type Registry interface {
	Inspect(ctx context.Context, ref string) (artifact.Publication, error)
}

// Attestations — atestacje GitHuba (adapters/github).
type Attestations interface {
	Verify(ctx context.Context, ref, sourceRef string) (artifact.Provenance, error)
	VerifyRelease(ctx context.Context, ref, version string) (artifact.Release, artifact.Provenance, error)
}

// Policy — czego środowisko wymaga od obrazu z rejestru.
type Policy struct {
	// Environment — nazwa środowiska, do komunikatów.
	Environment string
	// Repository — jedyne repozytorium, z którego obrazy wchodzą; puste —
	// dowolne (bramka atestacji sprawdza wskazany obraz).
	Repository string
	// Attested — pochodzenie i SBOM w atestacji BuildKitu oraz atestacja
	// GitHuba z main (decyzje 37 i 40).
	Attested bool
	// ReleasesOnly — tylko wydanie Release z atestacją wydania.
	ReleasesOnly bool
	// Release — numer wydania X.Y.Z, gdy ReleasesOnly.
	Release string
}

// For zwraca politykę środowiska dla obrazu aplikacji repository; release —
// numer wydania dla środowiska, które przyjmuje tylko wydania.
func For(e environment.Definition, repository, release string) Policy {
	return Policy{Environment: e.Name, Repository: repository, Attested: e.Artifact.Attested, ReleasesOnly: e.Artifact.ReleasesOnly, Release: release}
}

// Admitted — obraz przyjęty na środowisko.
type Admitted struct {
	// Publication — publikacja w rejestrze z commitem z etykiety; Release
	// ustawione, gdy obraz przyjęto jako wydanie.
	Publication artifact.Publication `json:"registry"`
	// Provenance — zweryfikowane pochodzenie z main; nil, gdy środowisko go
	// nie wymaga.
	Provenance *artifact.Provenance `json:"github,omitempty"`
	// Release — zweryfikowana atestacja wydania; nil poza wydaniami.
	Release *artifact.Release `json:"release,omitempty"`
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
	if p.ReleasesOnly && !artifact.IsRelease(p.Release) {
		return Admitted{}, cli.Usage("środowisko %s przyjmuje tylko wydania — podaj numer wydania X.Y.Z (jest %q)", p.Environment, p.Release)
	}
	plain := repository + "@" + digest
	published, err := g.Registry.Inspect(ctx, plain)
	if err != nil {
		return Admitted{}, err
	}
	commit := published.Commit
	if commit == "" {
		return Admitted{}, cli.Unmet("%s: obraz bez etykiety commita", plain)
	}
	admitted := Admitted{Publication: published}
	if p.Attested || p.ReleasesOnly {
		if err := published.RequireAttestations(); err != nil {
			return Admitted{}, err
		}
		if g.Attestations == nil {
			return Admitted{}, errors.New("polityka wymaga atestacji, a bramka nie ma weryfikatora")
		}
		// Pochodzenie z main: obraz buduje się tylko tam, a podpis mówi,
		// z którego commita — tag można by przesunąć, podpisu nie.
		verified, err := g.Attestations.Verify(ctx, plain, MainRef)
		if err != nil {
			return Admitted{}, err
		}
		if verified.SourceCommit != commit {
			return Admitted{}, cli.Unmet("atestacja GitHuba mówi o commicie %s, a obraz deklaruje %s", verified.SourceCommit, commit)
		}
		admitted.Provenance = &verified
		cli.Step("atestacja GitHuba: %s z %s (%s), %s, bieg %s", verified.PredicateType, verified.SourceRef, artifact.ShortCommit(verified.SourceCommit), verified.Runner, verified.Run)
	}
	if p.ReleasesOnly {
		release, signed, err := g.Attestations.VerifyRelease(ctx, plain, p.Release)
		if err != nil {
			return Admitted{}, err
		}
		_, releasedDigest, _ := artifact.ParseRef(release.Image)
		switch {
		case release.Version != p.Release:
			return Admitted{}, cli.Unmet("atestacja wydania mówi o %s, a promowane jest %s", release.Version, p.Release)
		case releasedDigest != digest:
			return Admitted{}, cli.Unmet("atestacja wydania %s dotyczy %s, a obraz to %s", p.Release, release.Image, plain)
		case release.Commit != commit || signed.SourceCommit != commit:
			return Admitted{}, cli.Unmet("wydanie %s z commita %s (podpis z %s), a obraz zbudowano z %s", p.Release, release.Commit, signed.SourceCommit, commit)
		}
		admitted.Release = &release
		admitted.Publication.Release = p.Release
		cli.Step("atestacja wydania %s: tag %s (%s), akceptacja na stagingu %s", p.Release, signed.SourceRef, artifact.ShortCommit(signed.SourceCommit), release.AcceptedAt)
	}
	cli.Step("rejestr: %s, atestacja %v, commit %s", published.Digest, published.Predicates, artifact.ShortCommit(commit))
	return admitted, nil
}
