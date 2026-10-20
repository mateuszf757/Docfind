package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/httpclient"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/registry"
	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/core/policy"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
	"github.com/mateuszf757/Docfind/tools/internal/stages/baseimage"
	"github.com/mateuszf757/Docfind/tools/internal/stages/build"
	"github.com/mateuszf757/Docfind/tools/internal/stages/imagetest"
	"github.com/mateuszf757/Docfind/tools/internal/stages/lifecycle"
	"github.com/mateuszf757/Docfind/tools/internal/stages/reproducible"
)

func runBuild(ctx context.Context, w *workspace, args []string) error {
	svc, err := service(args, "dft build [usługa]")
	if err != nil {
		return err
	}
	opts := build.Options{Root: w.root, Service: svc, Image: imageName(svc), Out: os.Stdout, Err: os.Stderr}
	for name, target := range map[string]*bool{"PUSH": &opts.Push, "NO_CACHE": &opts.NoCache} {
		if *target, err = envFlag(name); err != nil {
			return err
		}
	}
	// Build wydania nie istnieje od decyzji 40: wydanie nadaje numer
	// obrazowi z main, który przeszedł staging, zamiast budować go od nowa.
	if release, err := envFlag("RELEASE"); err != nil {
		return err
	} else if release {
		return cli.Usage("RELEASE=1: wydanie nie buduje obrazu (decyzja 40) — ./bin/mise run release -- vX.Y.Z na digeście ze stagingu")
	}
	// ARCHIVE=<plik> — obraz zapisany do archiwum dla wdrożenia bez rejestru.
	if path := os.Getenv("ARCHIVE"); path != "" {
		if opts.Archive, err = filepath.Abs(path); err != nil {
			return err
		}
	}
	if opts.Push {
		p, err := pins.Load(w.root)
		if err != nil {
			return err
		}
		opts.SBOMGenerator = p.Get(pins.SBOMGeneratorImage)
	}
	if err := requireTools("docker"); err != nil {
		return err
	}
	res, err := w.builder().Build(ctx, opts)
	if err != nil {
		return err
	}
	// Digest konfiguracji identyfikuje sprawdzony obraz niezależnie od drogi:
	// zadanie, które wdroży archiwum, porówna go z tym, co dostało.
	outputs := ""
	if opts.Archive != "" {
		outputs += fmt.Sprintf("archive=%s\nconfig=%s\n", opts.Archive, res.Config)
	}
	if res.Publication != nil {
		// Digest, nie tag, identyfikuje opublikowany obraz — tag można
		// nadpisać. Kolejne zadania i środowiska promują ten digest zamiast
		// budować od nowa.
		outputs += fmt.Sprintf("image=%s\ndigest=%s\n", res.Repository, res.Digest)
		if opts.Archive == "" {
			outputs += fmt.Sprintf("config=%s\n", res.Config)
		}
	}
	if err := githubOutput(outputs); err != nil {
		return err
	}
	if res.Publication == nil {
		return nil
	}
	// Dwa digesty sha256 obok siebie łatwo pomylić: do wdrożenia i promocji
	// służy digest indeksu, a digest konfiguracji tylko do porównań — pod
	// nim w rejestrze nie ma manifestu (MANIFEST_UNKNOWN).
	return appendSummary(fmt.Sprintf("### Opublikowany obraz %s\n\n| Commit | Obraz do wdrożenia (digest indeksu) | Konfiguracja (tylko do porównań) |\n|---|---|---|\n| `%s` (%s) | `%s` | `%s` |\n\n"+
		"Na staging, po zielonym biegu — a po akceptacji tam wydanie tego samego digestu (decyzja 40):\n\n```bash\nMISE_ENV=staging ./bin/mise run cluster:up -- --image %s\n./bin/mise run release -- vX.Y.Z\n```\n",
		svc, artifact.ShortCommit(res.Commit), res.Version, res.Ref(), res.Config, res.Ref()))
}

// builtImage zwraca obraz:tag zbudowany z bieżącego drzewa: tag z commita,
// z -dirty dla brudnego drzewa.
func builtImage(ctx context.Context, w *workspace, svc string) (string, error) {
	repo := w.git()
	commit, err := repo.Commit(ctx)
	if err != nil {
		return "", err
	}
	dirty, err := repo.Dirty(ctx)
	if err != nil {
		return "", err
	}
	return imageName(svc) + ":" + artifact.LocalTag(commit, dirty), nil
}

func runTestImage(ctx context.Context, w *workspace, args []string) error {
	svc, err := service(args, "dft test-image [usługa]")
	if err != nil {
		return err
	}
	if err := requireTools("docker"); err != nil {
		return err
	}
	epoch, err := w.git().SourceDateEpoch(ctx)
	if err != nil {
		return err
	}
	return imagetest.Run(ctx, w.docker(), w.root, svc, epoch, os.Stderr)
}

// runCheckRuntime — warunki Etapu 1 na zbudowanym obrazie. Próg rozmiaru
// jako flaga: wariant negatywny „obraz ponad limit" sprawdza się na
// prawdziwym obrazie, a nie na łatanym kodzie.
func runCheckRuntime(ctx context.Context, w *workspace, args []string) error {
	const usage = "dft check runtime [usługa] [--max-image-bytes N]"
	svc := "api"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		svc, args = args[0], args[1:]
	}
	limits := lifecycle.DefaultLimits
	fs := flag.NewFlagSet("check runtime", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Int64Var(&limits.MaxImageUnpackedBytes, "max-image-bytes", limits.MaxImageUnpackedBytes, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || limits.MaxImageUnpackedBytes <= 0 {
		return cli.Usage("użycie: %s — N to dodatnia liczba bajtów", usage)
	}
	if err := requireTools("docker"); err != nil {
		return err
	}
	image, err := builtImage(ctx, w, svc)
	if err != nil {
		return err
	}
	commit, err := w.git().Commit(ctx)
	if err != nil {
		return err
	}
	// Ustawienia poda z charta (decyzja 72); ich zgodność z renderem
	// sprawdza `dft check charts`.
	pod := policy.APIHardening
	checker := &lifecycle.Checker{
		Containers:    w.docker(),
		HTTP:          httpclient.Client{},
		Image:         image,
		ConfigExample: filepath.Join(w.root, "deploy", "config", "app.yml.example"),
		Limits:        limits,
		Name:          fmt.Sprintf("dft-runtime-%d", os.Getpid()),
		Pod:           &pod,
		Commit:        commit,
	}
	report, err := checker.Run(ctx)
	if err == nil {
		cli.Step("wszystkie warunki zakończenia Etapu 1 spełnione")
	}
	return w.withReport("runtime-"+svc, report, err)
}

func runCheckBase(ctx context.Context, w *workspace, args []string) error {
	svc, err := service(args, "dft check base [usługa]")
	if err != nil {
		return err
	}
	if err := requireTools("docker"); err != nil {
		return err
	}
	image, err := builtImage(ctx, w, svc)
	if err != nil {
		return err
	}
	p, err := pins.Load(w.root)
	if err != nil {
		return err
	}
	pyFile, err := os.ReadFile(filepath.Join(w.root, pins.PythonVersionPath))
	if err != nil {
		return err
	}
	python, err := pins.PythonVersion(pyFile)
	if err != nil {
		return err
	}
	facts, err := baseimage.Read(ctx, w.docker(), image)
	if err != nil {
		// Obraz, którego nie da się odczytać, to awaria sprawdzenia (kod 2):
		// najczęściej nie został zbudowany.
		return fmt.Errorf("%w — czy obraz jest zbudowany (./bin/mise run build)?", err)
	}
	cli.Step("baza %s: Alpine %s, Python %s, musl %s, OpenSSL %s", image, facts.Alpine, facts.Python, facts.Musl, facts.OpenSSL)
	// Przy przeglądzie PR-a od Dependabota widać, co naprawdę się zmieniło
	// pod tym samym tagiem bazy.
	if err := appendSummary(fmt.Sprintf("### Baza obrazu %s\n\n| Alpine | Python | musl | OpenSSL |\n|---|---|---|---|\n| %s | %s | %s | %s |\n",
		svc, facts.Alpine, facts.Python, facts.Musl, facts.OpenSSL)); err != nil {
		return err
	}
	if err := baseimage.Check(facts, p.Get(pins.BaseAlpine), python); err != nil {
		return err
	}
	cli.Step("baza obrazu zgodna z zapisaną")
	return nil
}

func runCheckReproducible(ctx context.Context, w *workspace, args []string) (err error) {
	svc, err := service(args, "dft check reproducible [usługa]")
	if err != nil {
		return err
	}
	if err := requireTools("docker"); err != nil {
		return err
	}
	p, err := pins.Load(w.root)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "dft-reproducible-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
	checker := reproducible.Checker{
		Builder:        w.builder(),
		BuildKit:       w.docker(),
		Options:        build.Options{Root: w.root, Service: svc, Image: imageName(svc)},
		PinnedBuildKit: p.Get(pins.BuildKitImage),
		Out:            os.Stdout,
	}
	return checker.Run(ctx, tmp)
}

// runCheckPublished sprawdza w rejestrze dowolny opublikowany obraz:
// indeks z atestacją i konfigurację równą podanej — tę samą weryfikację,
// którą build wykonuje po publikacji.
func runCheckPublished(ctx context.Context, _ *workspace, args []string) error {
	if err := expectArgs(args, 2, 2, "dft check published <obraz@digest> <digest konfiguracji>"); err != nil {
		return err
	}
	published, err := registry.Client{}.Verify(ctx, args[0], args[1])
	if err != nil {
		return err
	}
	cli.Step("rejestr: indeks %s, obraz %s, atestacja %s, konfiguracja %s", published.Digest, published.Image, published.Attestation, published.Config)
	return nil
}

// runCheckAttestation sprawdza opublikowany obraz przed wydaniem
// (decyzje 37 i 40): w rejestrze indeks z jednym obrazem i atestacją
// BuildKitu z pochodzeniem i SBOM, a do tego atestacja GitHuba (Sigstore)
// z workflowu ci tego repozytorium, z main.
func runCheckAttestation(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 1, 1, "dft check attestation <obraz@sha256:…>"); err != nil {
		return err
	}
	if err := requireTools("gh"); err != nil {
		return err
	}
	admitted, err := w.admission().Admit(ctx, args[0], admission.Policy{Environment: "atestacja", Attested: true})
	if err != nil {
		return err
	}
	a := admitted.Publication.Artifact
	return w.writeReport("attestation-"+strings.TrimPrefix(a.Digest, "sha256:")[:12], map[string]any{
		"image": a.Ref(), "commit": a.Commit, "registry": admitted.Publication, "github": admitted.Provenance,
	})
}

// runCheckNegatives uruchamia warianty negatywne bramek artefaktu na
// prawdziwym obrazie: testy z tagiem docker, które muszą zostać odrzucone.
// Bez Dockera nie mają czego sprawdzić, więc nie są częścią `dft check go`.
func runCheckNegatives(ctx context.Context, w *workspace, args []string) error {
	svc, err := service(args, "dft check negatives [usługa]")
	if err != nil {
		return err
	}
	if err := requireTools("docker", "go"); err != nil {
		return err
	}
	image, err := builtImage(ctx, w, svc)
	if err != nil {
		return err
	}
	cli.Step("warianty negatywne na %s", image)
	err = w.gotool().TestTagged(ctx, "docker", "", "DF_TEST_IMAGE="+image)
	if cli.ExitCode(err) == cli.ExitUnmet {
		return cli.Unmet("warianty negatywne: któraś bramka przepuściła obraz, który miała odrzucić — wyżej wynik go test")
	}
	return err
}
