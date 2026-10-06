package gocheck

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

func silence(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = &bytes.Buffer{}, &bytes.Buffer{}
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

func TestGofmt(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("dobry.go", "package x\n\nfunc A() {}\n")
	// testdata i katalogi ukryte nie są kodem modułu.
	write("testdata/zly.go", "package x\nfunc  B(){}\n")
	write(".cache/zly.go", "package x\nfunc  B(){}\n")

	c := Checker{ModuleDir: dir}
	if err := c.Gofmt(); err != nil {
		t.Fatalf("sformatowany moduł odrzucony: %v", err)
	}

	write("pakiet/zly.go", "package pakiet\nfunc  B(){}\n")
	err := c.Gofmt()
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), filepath.Join("pakiet", "zly.go")) {
		t.Errorf("oczekiwano niespełnionego z nazwą pliku, jest %v", err)
	}
}

func TestGoToolExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		resp     proc.FakeResponse
		run      func(Checker, context.Context) error
		wantCode int
	}{
		{"vet bez uwag", proc.FakeResponse{}, Checker.Vet, cli.ExitOK},
		{"vet z uwagami", proc.FakeResponse{Code: 1}, Checker.Vet, cli.ExitUnmet},
		{"go nie uruchamia się", proc.FakeResponse{Err: errors.New("brak go")}, Checker.Vet, cli.ExitFailure},
		{"govulncheck: podatności", proc.FakeResponse{Code: 3}, Checker.Vulncheck, cli.ExitUnmet},
		{"govulncheck: brak bazy", proc.FakeResponse{Code: 1}, Checker.Vulncheck, cli.ExitFailure},
		{"govulncheck: czysto", proc.FakeResponse{}, Checker.Vulncheck, cli.ExitOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &proc.Fake{Responses: map[string]proc.FakeResponse{
				"go vet ./...":              tt.resp,
				"go tool govulncheck ./...": tt.resp,
				"go tool staticcheck ./...": tt.resp,
				"go env CC":                 {Stdout: "gcc"},
				"go test -race ./...":       tt.resp,
				"go test ./...":             tt.resp,
			}}
			err := tt.run(Checker{ModuleDir: "/m", Runner: f}, context.Background())
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Errorf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
		})
	}
}

// TestRace: testy z -race tam, gdzie jest kompilator C; w CI jego brak to
// awaria, a lokalnie ostrzeżenie i testy bez -race.
func TestRace(t *testing.T) {
	silence(t)
	noCompiler := func(string) (string, error) { return "", errors.New("brak") }
	compiler := func(string) (string, error) { return "/usr/bin/gcc", nil }
	tests := []struct {
		name        string
		lookPath    func(string) (string, error)
		requireRace bool
		wantCmd     string
		wantCode    int
	}{
		{"kompilator jest", compiler, true, "go test -race ./...", cli.ExitOK},
		{"brak kompilatora lokalnie", noCompiler, false, "go test ./...", cli.ExitOK},
		{"brak kompilatora w CI", noCompiler, true, "", cli.ExitFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &proc.Fake{Responses: map[string]proc.FakeResponse{
				"go env CC":           {Stdout: "gcc\n"},
				"go test -race ./...": {},
				"go test ./...":       {},
			}}
			c := Checker{ModuleDir: "/m", Runner: f, RequireRace: tt.requireRace, LookPath: tt.lookPath}
			err := c.Test(context.Background())
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			last := f.Calls[len(f.Calls)-1]
			if tt.wantCmd != "" && last.String() != tt.wantCmd {
				t.Errorf("ostatnie polecenie %q, oczekiwano %q", last.String(), tt.wantCmd)
			}
			if tt.wantCmd == "go test -race ./..." && !strings.Contains(strings.Join(last.Env, " "), "CGO_ENABLED=1") {
				t.Errorf("-race bez CGO_ENABLED=1: %v", last.Env)
			}
		})
	}
}

// nondeterministicBuild udaje `go build`, którego wynik zależy od stanu
// pamięci podręcznej — tak wyglądałby program z czasem budowania w binarce.
type nondeterministicBuild struct{}

func (nondeterministicBuild) Run(_ context.Context, c proc.Cmd) (proc.Result, error) {
	out := c.Args[len(c.Args)-2]
	content := "z-pamieci"
	for _, e := range c.Env {
		if strings.HasPrefix(e, "GOCACHE=") {
			content = "od-zera"
		}
	}
	return proc.Result{}, os.WriteFile(filepath.Join(out, "dft"), []byte(content), 0o755)
}

func TestReproducibleDetectsDifference(t *testing.T) {
	silence(t)
	c := Checker{ModuleDir: "/m", Runner: nondeterministicBuild{}}
	binaries, err := c.Reproducible(context.Background(), t.TempDir())
	if cli.ExitCode(err) != cli.ExitUnmet {
		t.Fatalf("kod %d, oczekiwano %d (%v)", cli.ExitCode(err), cli.ExitUnmet, err)
	}
	if len(binaries) != 1 || binaries[0].Name != "dft" {
		t.Errorf("binarki: %+v", binaries)
	}
}
