package gocheck

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
	"github.com/mateuszf757/Docfind/tools/internal/vulns"
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

// reachable — ustalenie govulncheck na poziomie symbolu, z poprawką.
const reachable = `{"finding":{"osv":"GO-2026-0001","fixed_version":"v0.36.6","trace":[{"module":"k8s.io/client-go","version":"v0.36.5","function":"Do"}]}}`

// TestVulncheckException: ważny wyjątek z zakresu go zdejmuje blokadę,
// przeterminowany — nie.
func TestVulncheckException(t *testing.T) {
	silence(t)
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{"go tool govulncheck -format json ./...": {Stdout: reachable}}}
	today := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	for expires, want := range map[string]int{"2026-10-20": cli.ExitOK, "2026-10-07": cli.ExitUnmet} {
		c := Checker{ModuleDir: "/m", Runner: f, Today: today, Out: io.Discard, Err: io.Discard,
			Exceptions: []vulns.Exception{{ID: "GO-2026-0001", Package: "k8s.io/client-go", Expires: expires, Reason: "aktualizacja client-go czeka na k3s 1.37", Scope: vulns.ScopeGo}}}
		if code := cli.ExitCode(c.Vulncheck(context.Background())); code != want {
			t.Errorf("wyjątek do %s: kod %d, oczekiwano %d", expires, code, want)
		}
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
		{"govulncheck: osiągalna z poprawką", proc.FakeResponse{Stdout: reachable}, Checker.Vulncheck, cli.ExitUnmet},
		{"govulncheck: brak bazy", proc.FakeResponse{Code: 1}, Checker.Vulncheck, cli.ExitFailure},
		{"govulncheck: nieczytelny wynik", proc.FakeResponse{Stdout: "nie JSON"}, Checker.Vulncheck, cli.ExitFailure},
		{"govulncheck: czysto", proc.FakeResponse{Stdout: `{"config":{}}`}, Checker.Vulncheck, cli.ExitOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &proc.Fake{Responses: map[string]proc.FakeResponse{
				"go vet ./...":                           tt.resp,
				"go tool govulncheck -format json ./...": tt.resp,
				"go tool staticcheck ./...":              tt.resp,
				"go env CC":                              {Stdout: "gcc"},
				"go test -race ./...":                    tt.resp,
				"go test ./...":                          tt.resp,
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
