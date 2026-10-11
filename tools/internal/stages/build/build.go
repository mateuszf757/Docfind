// Package build to etap budowy: obraz usługi z commitem z gita,
// sprawdzenie version.json i — przy publikacji — atestacje i weryfikacja
// w rejestrze (dawniej ci/build.sh). Git, Docker i rejestr przychodzą jako
// porty; flagi buildx zna adapter Dockera.
//
// Obraz niesie tylko to, co wynika z commita: commit i jego czas
// w version.json i etykietach. Wersji w nim nie ma — ten sam commit to ten sam
// obraz, a wydanie nadaje numer digestowi, który już przeszedł bramki,
// zamiast budować go od nowa (decyzja 40).
package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Options — parametry jednego buildu.
type Options struct {
	Root    string
	Service string
	// Image — nazwa obrazu bez tagu (artifact.ImageName).
	Image string
	// Push — publikacja do rejestru z atestacją i weryfikacją w rejestrze;
	// tylko z czystego drzewa.
	Push bool
	// NoCache — build od zera. Build z pamięci podręcznej trywialnie daje ten
	// sam wynik, więc sprawdzenie powtarzalności potrzebuje drugiego od zera.
	NoCache bool
	// Archive — ścieżka, pod którą zapisać obraz (`docker save`) dla zadania,
	// które wdroży go na klaster bez rejestru (PR: obrazu tam nie ma).
	Archive string
	// SBOMGenerator — obraz skanera, z którego BuildKit robi SBOM do
	// atestacji przy publikacji (DF_SBOM_GENERATOR_IMAGE z ci/pins.env).
	SBOMGenerator string
	// Out i Err — wyjście BuildKitu.
	Out, Err io.Writer
}

// Result — zbudowany (i ewentualnie opublikowany) artefakt.
type Result struct {
	artifact.Artifact
	// Version — wersja drzewa z gita (decyzja 30): do komunikatów i dla
	// wdrożenia, które podaje ją aplikacji; obraz jej nie niesie.
	Version string
	// Dirty — obraz z brudnego drzewa.
	Dirty           bool
	SourceDateEpoch int64
	// Publication — publikacja zweryfikowana w rejestrze (przy Push).
	Publication *artifact.Publication
}

// LocalRef — obraz:tag zbudowany lokalnie, na którym biegną sprawdzenia.
func (r Result) LocalRef() string {
	return r.Repository + ":" + artifact.LocalTag(r.Commit, r.Dirty)
}

// Versions — tożsamość z systemu kontroli wersji (adapters/git).
type Versions interface {
	Version(ctx context.Context) (string, error)
	Commit(ctx context.Context) (string, error)
	Dirty(ctx context.Context) (bool, error)
	SourceDateEpoch(ctx context.Context) (int64, error)
	RequireClean(ctx context.Context) error
}

// Images — budowanie i odczyt obrazów (adapters/docker).
type Images interface {
	Build(ctx context.Context, req artifact.BuildRequest) (string, error)
	ReadFiles(ctx context.Context, ref string, paths ...string) ([]byte, error)
	SaveArchive(ctx context.Context, ref, path string) (string, error)
}

// Registry — publikacja w rejestrze (adapters/registry).
type Registry interface {
	Verify(ctx context.Context, ref, config string) (artifact.Publication, error)
}

// Builder buduje obraz usługi.
type Builder struct {
	Versions Versions
	Images   Images
	Registry Registry
}

// Build buduje obraz usługi, sprawdza jego tożsamość i — przy Push —
// publikuje go i sprawdza w rejestrze.
func (b Builder) Build(ctx context.Context, opts Options) (Result, error) {
	contextDir := filepath.Join(opts.Root, "services", opts.Service)
	if info, err := os.Stat(contextDir); err != nil || !info.IsDir() {
		return Result{}, cli.Usage("brak usługi %q w services/", opts.Service)
	}
	// Publikowany obraz musi dać się odtworzyć z commita, który podaje.
	if opts.Push {
		if err := b.Versions.RequireClean(ctx); err != nil {
			return Result{}, fmt.Errorf("publikacja tylko z czystego drzewa: %w", err)
		}
	}

	version, err := b.Versions.Version(ctx)
	if err != nil {
		return Result{}, err
	}
	commit, err := b.Versions.Commit(ctx)
	if err != nil {
		return Result{}, err
	}
	dirty, err := b.Versions.Dirty(ctx)
	if err != nil {
		return Result{}, err
	}
	epoch, err := b.Versions.SourceDateEpoch(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{Artifact: artifact.Artifact{Repository: opts.Image, Commit: commit}, Version: version, Dirty: dirty, SourceDateEpoch: epoch}
	cli.Step("usługa=%s commit=%s tag=%s (wersja drzewa %s)", opts.Service, artifact.ShortCommit(commit), artifact.LocalTag(commit, dirty), version)

	// SOURCE_DATE_EPOCH jako build arg BuildKit rozpoznaje sam: ustawia z niego
	// znaczniki czasu w konfiguracji i historii obrazu, a rewrite-timestamp
	// przycina do niego czasy plików w warstwach (decyzja 21). Wersji nie ma
	// wśród argumentów: zależy od tagów, a nie od commita (decyzja 40).
	req := artifact.BuildRequest{
		ContextDir: contextDir,
		Args: []artifact.BuildArg{
			{Name: "COMMIT", Value: commit},
			{Name: "SOURCE_DATE", Value: artifact.SourceDate(epoch)},
			{Name: "SOURCE_DATE_EPOCH", Value: strconv.FormatInt(epoch, 10)},
		},
		// Jeden tag: obraz commita (z -dirty dla brudnego drzewa). Ten sam
		// commit to ten sam obraz, więc tag nigdy nie przeskakuje na inną
		// treść; numer wydania dopisuje w rejestrze wydanie.
		Tags:    []string{res.LocalRef()},
		NoCache: opts.NoCache,
		Out:     opts.Out,
		Err:     opts.Err,
	}
	if _, err := b.Images.Build(ctx, req); err != nil {
		return res, fmt.Errorf("build %s: %w", res.LocalRef(), err)
	}
	cli.Step("zbudowano %s", res.LocalRef())

	// Kryterium zakończenia Etapu 0: to, co obraz mówi o sobie, zgadza się z gitem.
	if err := b.verifyIdentity(ctx, res.LocalRef(), commit); err != nil {
		return res, err
	}
	if opts.Archive != "" {
		if res.Config, err = b.Images.SaveArchive(ctx, res.LocalRef(), opts.Archive); err != nil {
			return res, err
		}
		cli.Step("archiwum %s, konfiguracja %s", opts.Archive, res.Config)
	}
	if !opts.Push {
		return res, nil
	}
	return b.publish(ctx, opts, res, req)
}

// verifyIdentity czyta version.json z obrazu i porównuje commit z gitem.
func (b Builder) verifyIdentity(ctx context.Context, ref, wantCommit string) error {
	raw, err := b.Images.ReadFiles(ctx, ref, "/app/version.json")
	if err != nil {
		return err
	}
	fmt.Fprint(cli.Out, string(raw))
	var reported struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(raw, &reported); err != nil {
		return fmt.Errorf("version.json w obrazie jest nieczytelny: %w", err)
	}
	if reported.Commit != wantCommit {
		return cli.Unmet("obraz deklaruje commit %q, oczekiwano %s", reported.Commit, wantCommit)
	}
	cli.Step("version.json zgodny z %s", wantCommit)
	return nil
}

// publish wysyła obraz do rejestru z atestacją pochodzenia i SBOM i sprawdza
// w rejestrze, że jest tam indeks z atestacją i obraz o konfiguracji obrazu
// sprawdzonego. Build bierze warstwy z pamięci podręcznej kopii lokalnej; że
// to ta sama zawartość, rozstrzyga rejestr, nie BuildKit.
func (b Builder) publish(ctx context.Context, opts Options, res Result, req artifact.BuildRequest) (Result, error) {
	tmp, err := os.MkdirTemp("", "dft-build-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)
	if res.Config, err = b.Images.SaveArchive(ctx, res.LocalRef(), filepath.Join(tmp, "image.tar")); err != nil {
		return res, err
	}

	cli.Step("publikacja do %s", opts.Image)
	req.Publish = &artifact.PublishRequest{SBOMGenerator: opts.SBOMGenerator}
	digest, err := b.Images.Build(ctx, req)
	if err != nil {
		return res, fmt.Errorf("publikacja %s: %w", res.LocalRef(), err)
	}
	published, err := b.Registry.Verify(ctx, opts.Image+"@"+digest, res.Config)
	if err != nil {
		return res, err
	}
	res.Digest = published.Digest
	res.Publication = &published
	cli.Step("rejestr: atestacja z pochodzeniem i SBOM (%v), konfiguracja zgodna z obrazem sprawdzonym (%s)", published.Predicates, res.Config)
	cli.Step("opublikowano %s", res.Ref())
	return res, nil
}
