package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/mateuszf757/Docfind/tools/internal/basecheck"
	"github.com/mateuszf757/Docfind/tools/internal/build"
	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/docker"
	"github.com/mateuszf757/Docfind/tools/internal/identity"
	"github.com/mateuszf757/Docfind/tools/internal/imagetest"
	"github.com/mateuszf757/Docfind/tools/internal/pins"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
	"github.com/mateuszf757/Docfind/tools/internal/registry"
	"github.com/mateuszf757/Docfind/tools/internal/reproducible"
	"github.com/mateuszf757/Docfind/tools/internal/runtimecheck"
)

// service zwraca usługę z argumentów; domyślnie api, jak w zadaniach mise.
func service(args []string, usage string) (string, error) {
	if err := expectArgs(args, 0, 1, usage); err != nil {
		return "", err
	}
	if len(args) == 1 {
		return args[0], nil
	}
	return "api", nil
}

func dockerClient(env *environment) docker.Client {
	return docker.Client{Runner: env.runner}
}

func builder(env *environment) build.Builder {
	return build.Builder{Repo: repoOf(env), Docker: dockerClient(env)}
}

// flag czyta zmienną środowiskową w postaci 0/1 — ten sam interfejs co
// dawny ci/build.sh (RELEASE=1, PUSH=1, NO_CACHE=1), z którego korzysta workflow.
func flag(name string) (bool, error) {
	switch os.Getenv(name) {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, cli.Usage("%s=%q — dozwolone 0 albo 1", name, os.Getenv(name))
	}
}

func runBuild(ctx context.Context, env *environment, args []string) error {
	svc, err := service(args, "dft build [usługa]")
	if err != nil {
		return err
	}
	opts := build.Options{Root: env.root, Service: svc, Image: imageName(svc), Out: os.Stdout, Err: os.Stderr}
	for name, target := range map[string]*bool{"RELEASE": &opts.Release, "PUSH": &opts.Push, "NO_CACHE": &opts.NoCache} {
		if *target, err = flag(name); err != nil {
			return err
		}
	}
	// ARCHIVE=<plik> — obraz zapisany do archiwum dla wdrożenia bez rejestru.
	if path := os.Getenv("ARCHIVE"); path != "" {
		if opts.Archive, err = filepath.Abs(path); err != nil {
			return err
		}
	}
	if opts.Push {
		p, err := pins.Load(env.root)
		if err != nil {
			return err
		}
		opts.SBOMGenerator = p.Get("DF_SBOM_GENERATOR_IMAGE")
	}
	if err := requireTools("docker"); err != nil {
		return err
	}
	res, err := builder(env).Build(ctx, opts)
	if err != nil {
		return err
	}
	// Digest konfiguracji identyfikuje sprawdzony obraz niezależnie od drogi:
	// zadanie, które wdroży archiwum, porówna go z tym, co dostało.
	outputs := ""
	if opts.Archive != "" {
		outputs += fmt.Sprintf("archive=%s\nconfig=%s\n", opts.Archive, res.Config)
	}
	if res.Published != nil {
		// Digest, nie tag, identyfikuje opublikowany obraz — tag można
		// nadpisać. Kolejne zadania i środowiska promują ten digest zamiast
		// budować od nowa.
		outputs += fmt.Sprintf("image=%s\ndigest=%s\n", res.Image, res.Published.Index)
		if opts.Archive == "" {
			outputs += fmt.Sprintf("config=%s\n", res.Config)
		}
	}
	if path := os.Getenv("GITHUB_OUTPUT"); path != "" && outputs != "" {
		if err := appendFile(path, outputs); err != nil {
			return fmt.Errorf("GITHUB_OUTPUT: %w", err)
		}
	}
	if res.Published == nil {
		return nil
	}
	digest := res.Published.Index
	// Dwa digesty sha256 obok siebie łatwo pomylić: do wdrożenia i promocji
	// służy digest indeksu, a digest konfiguracji tylko do porównań — pod
	// nim w rejestrze nie ma manifestu (MANIFEST_UNKNOWN).
	return appendSummary(fmt.Sprintf("### Opublikowany obraz %s\n\n| Wersja | Obraz do wdrożenia (digest indeksu) | Konfiguracja (tylko do porównań) |\n|---|---|---|\n| %s | `%s@%s` | `%s` |\n\n"+
		"Na staging, po zielonym biegu:\n\n```bash\nMISE_ENV=staging ./bin/mise run cluster:up -- --image %s@%s\n```\n",
		svc, res.Version, res.Image, digest, res.Config, res.Image, digest))
}

// builtImage zwraca obraz:tag zbudowany z bieżącego drzewa.
func builtImage(ctx context.Context, env *environment, svc string) (string, error) {
	version, err := repoOf(env).Version(ctx)
	if err != nil {
		return "", err
	}
	return imageName(svc) + ":" + identity.DockerTag(version), nil
}

func runCheckRuntime(ctx context.Context, env *environment, args []string) (err error) {
	const usage = "dft check runtime [usługa] [--max-image-bytes N]"
	limits := runtimecheck.DefaultLimits
	// Próg jako parametr — wariant negatywny „obraz ponad limit" sprawdza się
	// na prawdziwym obrazie, a nie na łatanym kodzie.
	if len(args) >= 2 && args[len(args)-2] == "--max-image-bytes" {
		n, convErr := strconv.ParseInt(args[len(args)-1], 10, 64)
		if convErr != nil || n <= 0 {
			return cli.Usage("--max-image-bytes: oczekiwano dodatniej liczby bajtów")
		}
		limits.MaxImageUnpackedBytes = n
		args = args[:len(args)-2]
	}
	svc, err := service(args, usage)
	if err != nil {
		return err
	}
	if err := requireTools("docker"); err != nil {
		return err
	}
	image, err := builtImage(ctx, env, svc)
	if err != nil {
		return err
	}
	checker := &runtimecheck.Checker{
		Docker:        dockerClient(env),
		Image:         image,
		ConfigExample: filepath.Join(env.root, "deploy", "config", "app.yml.example"),
		Limits:        limits,
		Name:          fmt.Sprintf("dft-runtime-%d", os.Getpid()),
	}
	report, err := checker.Run(ctx)
	if reportErr := writeReport(env, "runtime-"+svc, report); reportErr != nil {
		return errors.Join(err, reportErr)
	}
	if err == nil {
		cli.Step("wszystkie warunki zakończenia Etapu 1 spełnione")
	}
	return err
}

func runCheckBase(ctx context.Context, env *environment, args []string) error {
	svc, err := service(args, "dft check base [usługa]")
	if err != nil {
		return err
	}
	if err := requireTools("docker"); err != nil {
		return err
	}
	image, err := builtImage(ctx, env, svc)
	if err != nil {
		return err
	}
	p, err := pins.Load(env.root)
	if err != nil {
		return err
	}
	pyFile, err := os.ReadFile(filepath.Join(env.root, pins.PythonVersionPath))
	if err != nil {
		return err
	}
	python, err := pins.PythonVersion(pyFile)
	if err != nil {
		return err
	}
	facts, err := basecheck.Read(ctx, dockerClient(env), image)
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
	if err := basecheck.Check(facts, p.Get("DF_BASE_ALPINE"), python); err != nil {
		return err
	}
	cli.Step("baza obrazu zgodna z zapisaną")
	return nil
}

func runTestImage(ctx context.Context, env *environment, args []string) error {
	svc, err := service(args, "dft test-image [usługa]")
	if err != nil {
		return err
	}
	if err := requireTools("docker"); err != nil {
		return err
	}
	epoch, err := repoOf(env).SourceDateEpoch(ctx)
	if err != nil {
		return err
	}
	return imagetest.Run(ctx, dockerClient(env), env.root, svc, epoch, os.Stderr)
}

func runCheckReproducible(ctx context.Context, env *environment, args []string) (err error) {
	svc, err := service(args, "dft check reproducible [usługa]")
	if err != nil {
		return err
	}
	if err := requireTools("docker"); err != nil {
		return err
	}
	p, err := pins.Load(env.root)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "dft-reproducible-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
	checker := reproducible.Checker{
		Builder:        builder(env),
		Options:        build.Options{Root: env.root, Service: svc, Image: imageName(svc)},
		PinnedBuildKit: p.Get("DF_BUILDKIT_IMAGE"),
		Out:            os.Stdout,
	}
	return checker.Run(ctx, tmp)
}

// writeReport zapisuje wynik bramki w JSON do DF_REPORT_DIR albo
// .cache/reports — dowód zostaje po biegu, a w CI trafia do artefaktów.
// reportDir — katalog raportów bramek: DF_REPORT_DIR albo .cache/reports.
func reportDir(env *environment) string {
	if dir := os.Getenv("DF_REPORT_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(env.root, ".cache", "reports")
}

func writeReport(env *environment, name string, v any) error {
	dir := reportDir(env)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	cli.Step("raport: %s", path)
	return nil
}

// runCheckPublished sprawdza w rejestrze dowolny opublikowany obraz:
// indeks z atestacją i konfigurację równą podanej — tę samą weryfikację,
// którą build wykonuje po publikacji.
func runCheckPublished(ctx context.Context, env *environment, args []string) error {
	if err := expectArgs(args, 2, 2, "dft check published <obraz@digest> <digest konfiguracji>"); err != nil {
		return err
	}
	published, err := registry.VerifyPublished(args[0], args[1], registry.Options(ctx)...)
	if err != nil {
		return err
	}
	cli.Step("rejestr: indeks %s, obraz %s, atestacja %s, konfiguracja %s", published.Index, published.Image, published.Attestation, published.Config)
	return nil
}

// runCheckAttestation sprawdza opublikowany obraz przed promocją
// (decyzja 37): w rejestrze indeks z jednym obrazem i atestacją BuildKitu
// z pochodzeniem i SBOM, a do tego atestacja GitHuba (Sigstore) z workflowu
// ci tego repozytorium. Wydanie (wersja X.Y.Z w etykiecie) musi pochodzić
// z tagu swojej wersji.
func runCheckAttestation(ctx context.Context, env *environment, args []string) error {
	if err := expectArgs(args, 1, 1, "dft check attestation <obraz@sha256:…>"); err != nil {
		return err
	}
	if err := requireTools("gh"); err != nil {
		return err
	}
	ref, err := name.NewDigest(args[0])
	if err != nil {
		return cli.Usage("obraz %q: oczekiwano repozytorium@sha256:…", args[0])
	}
	plain := ref.Context().Name() + "@" + ref.DigestStr()
	inspected, err := registry.Inspect(plain, registry.Options(ctx)...)
	if err != nil {
		return err
	}
	if err := registry.RequireAttestations(inspected.Published); err != nil {
		return err
	}
	version, commit := inspected.Labels[registry.VersionLabel], inspected.Labels[registry.RevisionLabel]
	cli.Step("rejestr: wersja %s, commit %.12s, atestacja %v", version, commit, inspected.Predicates)
	sourceRef := ""
	if identity.IsRelease(version) {
		sourceRef = "refs/tags/v" + version
	}
	verified, err := attestVerifier(env).Verify(ctx, plain, sourceRef)
	if err != nil {
		return err
	}
	if verified.SourceCommit != commit {
		return cli.Unmet("atestacja GitHuba mówi o commicie %s, a obraz deklaruje %s", verified.SourceCommit, commit)
	}
	cli.Step("atestacja GitHuba: %s z %s (%.12s), %s, bieg %s", verified.PredicateType, verified.SourceRef, verified.SourceCommit, verified.Runner, verified.Run)
	return writeReport(env, "attestation-"+strings.TrimPrefix(ref.DigestStr(), "sha256:")[:12], map[string]any{
		"image": plain, "version": version, "commit": commit, "registry": inspected.Published, "github": verified,
	})
}

// runCheckClusterNegatives uruchamia warianty negatywne na żywym klastrze
// środowiska (go test -tags cluster): to, co API server ma odrzucić.
func runCheckClusterNegatives(ctx context.Context, env *environment, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check cluster-negatives"); err != nil {
		return err
	}
	if err := requireTools("go"); err != nil {
		return err
	}
	_, err := env.runner.Run(ctx, proc.Cmd{
		Name: "go", Args: []string{"test", "-tags", "cluster", "-count=1", "-run", "Rejected$", "./..."},
		Dir: filepath.Join(env.root, "tools"), Stdout: os.Stdout, Stderr: os.Stderr,
	})
	if proc.ExitCode(err) > 0 {
		return cli.Unmet("warianty negatywne na klastrze: API server przyjął coś, co miał odrzucić — wyżej wynik go test")
	}
	return err
}

// runCheckNegatives uruchamia warianty negatywne bramek artefaktu na
// prawdziwym obrazie: testy z tagiem docker (go test -tags docker), które
// muszą zostać odrzucone. Bez Dockera nie mają czego sprawdzić, więc nie są
// częścią `dft check go`.
func runCheckNegatives(ctx context.Context, env *environment, args []string) error {
	svc, err := service(args, "dft check negatives [usługa]")
	if err != nil {
		return err
	}
	if err := requireTools("docker", "go"); err != nil {
		return err
	}
	image, err := builtImage(ctx, env, svc)
	if err != nil {
		return err
	}
	cli.Step("warianty negatywne na %s", image)
	_, err = env.runner.Run(ctx, proc.Cmd{
		Name:   "go",
		Args:   []string{"test", "-tags", "docker", "-count=1", "./..."},
		Dir:    filepath.Join(env.root, "tools"),
		Env:    []string{"DF_TEST_IMAGE=" + image},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if proc.ExitCode(err) > 0 {
		return cli.Unmet("warianty negatywne: któraś bramka przepuściła obraz, który miała odrzucić — wyżej wynik go test")
	}
	return err
}
