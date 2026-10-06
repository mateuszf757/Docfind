// Package build buduje obraz usługi z tożsamością z gita i publikuje go do
// rejestru z atestacją pochodzenia — port ci/build.sh.
//
// Obraz jest samoopisujący się: version.json powstaje wewnątrz niego z build
// argów, więc nie da się go rozdzielić z tożsamością (Etap 0).
package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/docker"
	"github.com/mateuszf757/Docfind/tools/internal/identity"
	"github.com/mateuszf757/Docfind/tools/internal/oci"
	"github.com/mateuszf757/Docfind/tools/internal/registry"
)

// Options — parametry jednego buildu.
type Options struct {
	Root    string
	Service string
	// Image — nazwa obrazu bez tagu (identity.ImageName).
	Image string
	// Release — build wydania: czyste drzewo na commicie z tagiem vX.Y.Z.
	Release bool
	// Push — publikacja do rejestru z atestacją i weryfikacją w rejestrze.
	Push bool
	// NoCache — build od zera. Build z pamięci podręcznej trywialnie daje ten
	// sam wynik, więc sprawdzenie powtarzalności potrzebuje drugiego od zera.
	NoCache bool
	// Out i Err — wyjście BuildKitu.
	Out, Err io.Writer
}

// Result opisuje zbudowany (i ewentualnie opublikowany) obraz.
type Result struct {
	Image           string
	Tag             string
	Version         string
	Commit          string
	SourceDateEpoch int64
	// Config — digest konfiguracji obrazu sprawdzonego (przy publikacji).
	Config string
	// Published — publikacja zweryfikowana w rejestrze (przy publikacji).
	Published *registry.Published
}

// Ref zwraca obraz:tag zbudowany lokalnie.
func (r Result) Ref() string { return r.Image + ":" + r.Tag }

// Builder buduje obrazy przez docker buildx.
type Builder struct {
	Repo   identity.Repo
	Docker docker.Client
	// Verify sprawdza publikację w rejestrze; nil — registry.VerifyPublished
	// z uwierzytelnieniem z konfiguracji Dockera.
	Verify func(ctx context.Context, ref, config string) (registry.Published, error)
}

var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Build buduje obraz usługi, sprawdza jego tożsamość i — przy Push —
// publikuje go i sprawdza w rejestrze.
func (b Builder) Build(ctx context.Context, opts Options) (Result, error) {
	contextDir := filepath.Join(opts.Root, "services", opts.Service)
	if info, err := os.Stat(contextDir); err != nil || !info.IsDir() {
		return Result{}, cli.Usage("brak usługi %q w services/", opts.Service)
	}
	if opts.Release {
		if err := b.Repo.RequireClean(ctx); err != nil {
			return Result{}, err
		}
	}

	version, err := b.Repo.Version(ctx)
	if err != nil {
		return Result{}, err
	}
	commit, err := b.Repo.Commit(ctx)
	if err != nil {
		return Result{}, err
	}
	epoch, err := b.Repo.SourceDateEpoch(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{Image: opts.Image, Tag: identity.DockerTag(version), Version: version, Commit: commit, SourceDateEpoch: epoch}

	// Build wydania to build wydanej wersji: Version daje samo X.Y.Z tylko na
	// commicie z tagiem vX.Y.Z (decyzja 30). Commit bez tagu albo tag w innej
	// postaci dałby obraz wydania z wersją deweloperską.
	if opts.Release && !releaseVersion.MatchString(version) {
		return res, cli.Unmet("build wydania wymaga commita z tagiem vX.Y.Z, a wersja to %s", version)
	}
	cli.Step("usługa=%s wersja=%s tag=%s commit=%s", opts.Service, version, res.Tag, commit[:12])

	// SOURCE_DATE_EPOCH jako build arg BuildKit rozpoznaje sam: ustawia z niego
	// znaczniki czasu w konfiguracji i historii obrazu, a rewrite-timestamp
	// przycina do niego czasy plików w warstwach (decyzja 21).
	buildArgs := []string{
		"--build-arg", "VERSION=" + version,
		"--build-arg", "COMMIT=" + commit,
		"--build-arg", "SOURCE_DATE=" + identity.SourceDate(epoch),
		"--build-arg", fmt.Sprintf("SOURCE_DATE_EPOCH=%d", epoch),
	}
	// Tag z commitem tylko poza wydaniem. Commit z tagiem vX.Y.Z buduje się dwa
	// razy — z gałęzi i z tagu — z różną wersją w version.json, a wspólny tag
	// <sha12> przeskakiwałby wtedy w rejestrze z obrazu z main na obraz wydania.
	tags := []string{"--tag", res.Ref()}
	if !opts.Release {
		tags = append(tags, "--tag", opts.Image+":"+commit[:12])
	}

	// Opcje eksportu zależą od sterownika BuildKit. Sterownik docker (lokalny
	// demon z magazynem containerd) eksportuje prosto do demona i odrzuca
	// rewrite-timestamp razem z domyślnym rozpakowaniem warstw — rozpakowanie
	// nastąpi przy pierwszym uruchomieniu. Sterownik docker-container (CI) nie
	// ma dostępu do magazynu demona, więc obraz wraca jako archiwum i jest
	// ładowany — dopiero wtedy da się go uruchomić.
	builder, err := b.Docker.CurrentBuilder(ctx)
	if err != nil {
		return res, err
	}
	output := "type=docker,rewrite-timestamp=true"
	if builder.Driver == "docker" {
		output = "type=image,rewrite-timestamp=true,unpack=false"
	}

	// Kopia lokalna, na której biegną sprawdzenia, zawsze bez atestacji
	// pochodzenia: atestacja opisuje przebieg budowania, więc różni się między
	// buildami i zmienia digest indeksu — każdy build wyglądałby jak nowa
	// zawartość i wywoływał rollout na klastrze.
	args := append([]string{"buildx", "build"}, buildArgs...)
	args = append(args, "--provenance=false", "--output", output)
	args = append(args, tags...)
	if opts.NoCache {
		args = append(args, "--no-cache")
	}
	args = append(args, contextDir)
	if err := b.Docker.Stream(ctx, opts.Out, opts.Err, args...); err != nil {
		return res, fmt.Errorf("build %s: %w", res.Ref(), err)
	}
	cli.Step("zbudowano %s", res.Ref())

	// Kryterium zakończenia Etapu 0: to, co obraz mówi o sobie, zgadza się z gitem.
	if err := b.verifyIdentity(ctx, res.Ref(), commit); err != nil {
		return res, err
	}
	if !opts.Push {
		return res, nil
	}
	return b.publish(ctx, opts, res, buildArgs, tags, contextDir)
}

// verifyIdentity czyta version.json z obrazu i porównuje commit z gitem.
func (b Builder) verifyIdentity(ctx context.Context, ref, wantCommit string) error {
	raw, err := b.Docker.ReadFiles(ctx, ref, "/app/version.json")
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

// publish wysyła obraz do rejestru prosto z BuildKitu (eksporter image
// z push=true), z atestacją pochodzenia, i sprawdza w rejestrze, że jest tam
// indeks z atestacją i obraz o konfiguracji obrazu sprawdzonego.
//
// Wcześniej obraz szedł do rejestru przez `docker push` z magazynu demona
// i atestacja ginęła po drodze. Build bierze warstwy z pamięci podręcznej
// kopii lokalnej; że to ta sama zawartość, rozstrzyga rejestr, nie BuildKit.
// mode=max zapisuje pełną definicję builda — build argi nie są sekretami.
func (b Builder) publish(ctx context.Context, opts Options, res Result, buildArgs, tags []string, contextDir string) (Result, error) {
	tmp, err := os.MkdirTemp("", "dft-build-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(tmp)

	archive := filepath.Join(tmp, "image.tar")
	if err := b.Docker.Save(ctx, res.Ref(), archive); err != nil {
		return res, err
	}
	config, err := oci.ConfigDigestFromSave(archive)
	if err != nil {
		return res, err
	}
	res.Config = config

	cli.Step("publikacja do %s", opts.Image)
	metadata := filepath.Join(tmp, "push.json")
	args := append([]string{"buildx", "build"}, buildArgs...)
	args = append(args, "--provenance=mode=max", "--output", "type=image,push=true,rewrite-timestamp=true,unpack=false")
	args = append(args, tags...)
	args = append(args, "--metadata-file", metadata, contextDir)
	if err := b.Docker.Stream(ctx, opts.Out, opts.Err, args...); err != nil {
		return res, fmt.Errorf("publikacja %s: %w", res.Ref(), err)
	}
	digest, err := pushedDigest(metadata)
	if err != nil {
		return res, err
	}

	verify := b.Verify
	if verify == nil {
		verify = func(ctx context.Context, ref, config string) (registry.Published, error) {
			return registry.VerifyPublished(ref, config, registry.Options(ctx)...)
		}
	}
	published, err := verify(ctx, opts.Image+"@"+digest, config)
	if err != nil {
		return res, err
	}
	res.Published = &published
	cli.Step("rejestr: atestacja pochodzenia obecna, konfiguracja zgodna z obrazem sprawdzonym (%s)", config)
	cli.Step("opublikowano %s@%s", opts.Image, digest)
	return res, nil
}

// pushedDigest czyta digest publikacji z --metadata-file BuildKitu.
func pushedDigest(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("metadane publikacji: %w", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("metadane publikacji: %w", err)
	}
	digest, ok := meta["containerimage.digest"].(string)
	if !ok || digest == "" {
		return "", errors.New("metadane publikacji: brak containerimage.digest")
	}
	return digest, nil
}
