// Package build to etap budowy: obraz usługi z tożsamością z gita,
// sprawdzenie version.json i — przy publikacji — atestacje i weryfikacja
// w rejestrze (dawniej ci/build.sh). Git, Docker i rejestr przychodzą jako
// porty; flagi buildx zna adapter Dockera.
//
// Obraz jest samoopisujący się: version.json powstaje wewnątrz niego z build
// argów, więc nie da się go rozdzielić z tożsamością (Etap 0).
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
	// Release — build wydania: czyste drzewo na commicie z tagiem vX.Y.Z.
	Release bool
	// Push — publikacja do rejestru z atestacją i weryfikacją w rejestrze.
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
	SourceDateEpoch int64
	// Publication — publikacja zweryfikowana w rejestrze (przy Push).
	Publication *artifact.Publication
}

// LocalRef — obraz:tag zbudowany lokalnie, na którym biegną sprawdzenia.
func (r Result) LocalRef() string { return r.Repository + ":" + r.Tag() }

// Versions — tożsamość z systemu kontroli wersji (adapters/git).
type Versions interface {
	Version(ctx context.Context) (string, error)
	Commit(ctx context.Context) (string, error)
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
	if opts.Release {
		if err := b.Versions.RequireClean(ctx); err != nil {
			return Result{}, err
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
	epoch, err := b.Versions.SourceDateEpoch(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{Artifact: artifact.Artifact{Repository: opts.Image, Version: version, Commit: commit}, SourceDateEpoch: epoch}

	// Build wydania to build wydanej wersji: Version daje samo X.Y.Z tylko na
	// commicie z tagiem vX.Y.Z (decyzja 30). Commit bez tagu albo tag w innej
	// postaci dałby obraz wydania z wersją deweloperską.
	if opts.Release && !artifact.IsRelease(version) {
		return res, cli.Unmet("build wydania wymaga commita z tagiem vX.Y.Z, a wersja to %s", version)
	}
	cli.Step("usługa=%s wersja=%s tag=%s commit=%s", opts.Service, version, res.Tag(), artifact.ShortCommit(commit))

	// SOURCE_DATE_EPOCH jako build arg BuildKit rozpoznaje sam: ustawia z niego
	// znaczniki czasu w konfiguracji i historii obrazu, a rewrite-timestamp
	// przycina do niego czasy plików w warstwach (decyzja 21).
	req := artifact.BuildRequest{
		ContextDir: contextDir,
		Args: []artifact.BuildArg{
			{Name: "VERSION", Value: version},
			{Name: "COMMIT", Value: commit},
			{Name: "SOURCE_DATE", Value: artifact.SourceDate(epoch)},
			{Name: "SOURCE_DATE_EPOCH", Value: strconv.FormatInt(epoch, 10)},
		},
		// Tag z commitem tylko poza wydaniem. Commit z tagiem vX.Y.Z buduje się
		// dwa razy — z gałęzi i z tagu — z różną wersją w version.json,
		// a wspólny tag <sha12> przeskakiwałby wtedy w rejestrze z obrazu
		// z main na obraz wydania.
		Tags:    []string{res.LocalRef()},
		NoCache: opts.NoCache,
		Out:     opts.Out,
		Err:     opts.Err,
	}
	if !opts.Release {
		req.Tags = append(req.Tags, opts.Image+":"+artifact.ShortCommit(commit))
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
