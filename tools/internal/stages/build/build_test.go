package build

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/docker"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/git"
	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Testy składają etap z prawdziwymi adapterami gita i Dockera na atrapach
// (repozytorium w katalogu tymczasowym, nagrane odpowiedzi docker) —
// sprawdzają też, jak żądanie etapu przekłada się na flagi buildx. Reguła
// warstw dotyczy kodu produkcyjnego; test może składać etap z adapterami.

// fakeRegistry zapamiętuje weryfikację publikacji.
type fakeRegistry struct {
	verified []string
	digest   string
}

func (r *fakeRegistry) Verify(_ context.Context, ref, config string) (artifact.Publication, error) {
	r.verified = append(r.verified, ref, config)
	return artifact.Publication{Artifact: artifact.Artifact{Repository: image, Digest: r.digest, Config: config}}, nil
}

const image = "ghcr.io/test/docfind-api"

// gitRepo zakłada repozytorium z usługą api i jednym commitem.
func gitRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, kv := range []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid", "GIT_AUTHOR_DATE=2026-10-01T12:00:00Z", "GIT_COMMITTER_DATE=2026-10-01T12:00:00Z"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "services", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "services", "api", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "start"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// fakeDocker odpowiada na polecenia docker i zapamiętuje je.
type fakeDocker struct {
	driver        string
	reportCommit  string
	pushedDigest  string
	savedConfigID string
	calls         [][]string
}

func (f *fakeDocker) run(_ context.Context, c proc.Cmd) (proc.Result, error) {
	f.calls = append(f.calls, c.Args)
	switch {
	case slices.Equal(c.Args, []string{"buildx", "inspect"}):
		return proc.Result{Stdout: []byte("Name: test\nDriver: " + f.driver + "\n\nNodes:\nName: test0\nBuildKit version: v0.33.0\n")}, nil
	case len(c.Args) > 2 && c.Args[0] == "buildx" && c.Args[1] == "build":
		if i := slices.Index(c.Args, "--metadata-file"); i >= 0 {
			return proc.Result{}, os.WriteFile(c.Args[i+1], []byte(`{"containerimage.digest":"`+f.pushedDigest+`"}`), 0o644)
		}
		return proc.Result{}, nil
	case len(c.Args) > 0 && c.Args[0] == "run":
		return proc.Result{Stdout: []byte(`{"version":"x","commit":"` + f.reportCommit + `"}` + "\n")}, nil
	case len(c.Args) > 0 && c.Args[0] == "save":
		return proc.Result{}, writeSave(c.Args[2], f.savedConfigID)
	}
	return proc.Result{}, &proc.ExitError{Cmd: c.String(), Code: 1}
}

func writeSave(path, config string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	manifest := `[{"Config":"blobs/sha256/` + strings.TrimPrefix(config, "sha256:") + `"}]`
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(manifest))}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(manifest)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func (f *fakeDocker) built() [][]string {
	var builds [][]string
	for _, call := range f.calls {
		if len(call) > 1 && call[0] == "buildx" && call[1] == "build" {
			builds = append(builds, call)
		}
	}
	return builds
}

func newBuilder(t *testing.T, root string, f *fakeDocker) Builder {
	t.Helper()
	return Builder{
		Versions: git.Repo{Dir: root, Runner: proc.Exec{}},
		Images:   docker.Client{Runner: proc.RunnerFunc(f.run)},
		Registry: &fakeRegistry{},
	}
}

func commitOf(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = &bytes.Buffer{}, &bytes.Buffer{}
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

func TestBuildLocal(t *testing.T) {
	quiet(t)
	for _, tt := range []struct{ driver, output string }{
		{"docker", "type=image,rewrite-timestamp=true,unpack=false"},
		{"docker-container", "type=docker,rewrite-timestamp=true"},
	} {
		root := gitRepo(t)
		commit := commitOf(t, root)
		f := &fakeDocker{driver: tt.driver, reportCommit: commit}
		res, err := newBuilder(t, root, f).Build(context.Background(), Options{Root: root, Service: "api", Image: image})
		if err != nil {
			t.Fatalf("%s: %v", tt.driver, err)
		}
		builds := f.built()
		if len(builds) != 1 {
			t.Fatalf("%s: buildów %d, oczekiwano 1", tt.driver, len(builds))
		}
		args := strings.Join(builds[0], " ")
		for _, want := range []string{"--provenance=false", "--output " + tt.output, "--tag " + image + ":" + commit[:12] + " ", "COMMIT=" + commit, "SOURCE_DATE_EPOCH=1790856000"} {
			if !strings.Contains(args, want) {
				t.Errorf("%s: build bez %q:\n%s", tt.driver, want, args)
			}
		}
		// Wersja zależy od tagów, a nie od commita — nie trafia do obrazu
		// (decyzja 40), a obraz ma jeden tag.
		if strings.Contains(args, "VERSION=") || strings.Count(args, "--tag ") != 1 {
			t.Errorf("%s: wersja w argumentach albo więcej tagów:\n%s", tt.driver, args)
		}
		if res.Version != "0.0.0-dev.1+"+commit[:7] || res.LocalRef() != image+":"+commit[:12] {
			t.Errorf("%s: wersja %q, obraz %s", tt.driver, res.Version, res.LocalRef())
		}
	}
}

// TestBuildDirty: obraz z brudnego drzewa nie podaje się za obraz commita.
func TestBuildDirty(t *testing.T) {
	quiet(t)
	root := gitRepo(t)
	commit := commitOf(t, root)
	if err := os.WriteFile(filepath.Join(root, "nowy"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeDocker{driver: "docker", reportCommit: commit}
	res, err := newBuilder(t, root, f).Build(context.Background(), Options{Root: root, Service: "api", Image: image})
	if err != nil {
		t.Fatal(err)
	}
	if res.LocalRef() != image+":"+commit[:12]+"-dirty" || !strings.HasSuffix(res.Version, "-dirty") {
		t.Errorf("brudne drzewo: %s, wersja %q", res.LocalRef(), res.Version)
	}
}

func TestBuildRejects(t *testing.T) {
	quiet(t)
	tests := []struct {
		name     string
		prepare  func(root string)
		opts     Options
		report   string
		wantMsg  string
		noBuilds bool
	}{
		{
			name:     "publikacja z brudnego drzewa",
			prepare:  func(root string) { _ = os.WriteFile(filepath.Join(root, "nowy"), []byte("x"), 0o644) },
			opts:     Options{Push: true, SBOMGenerator: "g"},
			wantMsg:  "drzewo robocze jest brudne",
			noBuilds: true,
		},
		{name: "obraz z cudzym commitem", report: strings.Repeat("0", 40), wantMsg: "obraz deklaruje commit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := gitRepo(t)
			if tt.prepare != nil {
				tt.prepare(root)
			}
			report := tt.report
			if report == "" {
				report = commitOf(t, root)
			}
			f := &fakeDocker{driver: "docker", reportCommit: report}
			opts := tt.opts
			opts.Root, opts.Service, opts.Image = root, "api", image
			_, err := newBuilder(t, root, f).Build(context.Background(), opts)
			if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("oczekiwano niespełnionego z %q, jest %v", tt.wantMsg, err)
			}
			if tt.noBuilds && len(f.built()) > 0 {
				t.Errorf("build uruchomiony mimo odrzucenia: %v", f.built())
			}
		})
	}
}

// TestBuildPush: publikacja z atestacją, a weryfikacja dostaje digest
// z metadanych BuildKitu i konfigurację obrazu sprawdzonego z docker save.
func TestBuildPush(t *testing.T) {
	quiet(t)
	root := gitRepo(t)
	config := "sha256:" + strings.Repeat("c", 64)
	digest := "sha256:" + strings.Repeat("d", 64)
	f := &fakeDocker{driver: "docker-container", reportCommit: commitOf(t, root), pushedDigest: digest, savedConfigID: config}
	b := newBuilder(t, root, f)
	reg := &fakeRegistry{digest: digest}
	b.Registry = reg
	generator := "docker/buildkit-syft-scanner:1.12.0@sha256:" + strings.Repeat("e", 64)
	res, err := b.Build(context.Background(), Options{Root: root, Service: "api", Image: image, Push: true, SBOMGenerator: generator})
	if err != nil {
		t.Fatal(err)
	}
	builds := f.built()
	if len(builds) != 2 {
		t.Fatalf("buildów %d, oczekiwano 2 (kopia lokalna i publikacja)", len(builds))
	}
	push := strings.Join(builds[1], " ")
	for _, want := range []string{"--provenance=mode=max", "--attest type=sbom,generator=" + generator, "--output type=image,push=true,rewrite-timestamp=true,unpack=false", "--tag " + image + ":" + commitOf(t, root)[:12]} {
		if !strings.Contains(push, want) {
			t.Errorf("publikacja bez %q:\n%s", want, push)
		}
	}
	if want := []string{image + "@" + digest, config}; !slices.Equal(reg.verified, want) {
		t.Errorf("weryfikacja %v, oczekiwano %v", reg.verified, want)
	}
	if res.Publication == nil || res.Config != config || res.Ref() != image+"@"+digest || res.Pinned() != image+":"+commitOf(t, root)[:12]+"@"+digest {
		t.Errorf("wynik %+v", res)
	}
}

// TestBuildPushRequiresSBOMGenerator: bez przypiętego generatora publikacja
// nie rusza — domyślny generator BuildKitu to przesuwalny tag.
func TestBuildPushRequiresSBOMGenerator(t *testing.T) {
	quiet(t)
	root := gitRepo(t)
	f := &fakeDocker{driver: "docker-container", reportCommit: commitOf(t, root), savedConfigID: "sha256:" + strings.Repeat("c", 64)}
	_, err := newBuilder(t, root, f).Build(context.Background(), Options{Root: root, Service: "api", Image: image, Push: true})
	if err == nil || !strings.Contains(err.Error(), "generatora SBOM") {
		t.Fatalf("publikacja bez generatora: %v", err)
	}
	if n := len(f.built()); n != 1 {
		t.Errorf("buildów %d, oczekiwano 1 (tylko kopia lokalna)", n)
	}
}
