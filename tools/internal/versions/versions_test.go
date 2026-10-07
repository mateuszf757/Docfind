package versions

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/pins"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

const repoRoot = "../../.."

// fixture kopiuje prawdziwe pliki wersji do katalogu tymczasowego, żeby test
// mógł je psuć bez dotykania repozytorium.
func fixture(t *testing.T, edit func(path string, content string) string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range []string{pins.File, pins.Dockerfile, pins.PythonVersionPath, pins.GoMod} {
		data, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if edit != nil {
			content = edit(rel, content)
		}
		target := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func goVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, pins.GoMod))
	if err != nil {
		t.Fatal(err)
	}
	v, err := pins.GoDirective(data)
	if err != nil {
		t.Fatal(err)
	}
	return "go" + v
}

func runner(t *testing.T, kubectl, uv string) *proc.Fake {
	t.Helper()
	p, err := pins.Load(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if kubectl == "" {
		kubectl = `{"clientVersion":{"gitVersion":"v` + p.Get("DF_KUBERNETES_VERSION") + `"}}`
	}
	dockerfile, _ := os.ReadFile(filepath.Join(repoRoot, pins.Dockerfile))
	if uv == "" {
		want, err := pins.UVVersion(dockerfile)
		if err != nil {
			t.Fatal(err)
		}
		uv = "uv " + want + " (x86_64-unknown-linux-gnu)"
	}
	return &proc.Fake{Responses: map[string]proc.FakeResponse{
		"kubectl version --client -o json": {Stdout: kubectl},
		"uv --version":                     {Stdout: uv},
	}}
}

func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = &buf, &buf
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
	return &buf
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name     string
		edit     func(path, content string) string
		kubectl  string
		uv       string
		goVer    string
		wantCode int
		wantText string
	}{
		{name: "wszystko zgodne", wantCode: cli.ExitOK},
		{
			name:     "kubectl inny niż klaster",
			kubectl:  `{"clientVersion":{"gitVersion":"v1.35.0"}}`,
			wantCode: cli.ExitUnmet,
			wantText: "kubectl v1.35.0",
		},
		{
			name: "baza Pythona inna niż testy",
			edit: func(path, content string) string {
				if path == pins.PythonVersionPath {
					return "3.13\n"
				}
				return content
			},
			wantCode: cli.ExitUnmet,
			wantText: "testy biegną na Pythonie 3.13",
		},
		{
			name:     "Go z mise inny niż w go.mod",
			goVer:    "go1.99.0",
			wantCode: cli.ExitUnmet,
			wantText: "program zbudowany przez go1.99.0",
		},
		{
			name:     "inny lokalny uv to tylko ostrzeżenie",
			uv:       "uv 0.0.1 (x86_64-unknown-linux-gnu)",
			wantCode: cli.ExitOK,
			wantText: "UWAGA: lokalny uv 0.0.1",
		},
		{
			name:     "nieczytelne wyjście kubectl to awaria, nie wynik",
			kubectl:  "to nie JSON",
			wantCode: cli.ExitFailure,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := capture(t)
			root := fixture(t, tt.edit)
			gv := tt.goVer
			if gv == "" {
				gv = goVersion(t)
			}
			err := Check(context.Background(), root, runner(t, tt.kubectl, tt.uv), gv)
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)\n%s", code, tt.wantCode, err, out)
			}
			if tt.wantText != "" && !strings.Contains(out.String(), tt.wantText) {
				t.Errorf("wyjście nie zawiera %q:\n%s", tt.wantText, out)
			}
		})
	}
}
