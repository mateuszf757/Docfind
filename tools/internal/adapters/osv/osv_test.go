package osv

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// osvJSON — wynik OSV-Scannera 2.6.0 w postaci z prawdziwego skanu obrazu
// API (zlib z Alpine 3.24), skrócony, plus pakiet PyPI bez poprawki i bez
// oceny CVSS.
const osvJSON = `{"results":[{"source":{"path":"/lib/apk/db/installed","type":"os"},"packages":[
 {"package":{"name":"zlib","version":"1.3.2-r0","ecosystem":"Alpine:v3.24"},
  "vulnerabilities":[{"id":"ALPINE-CVE-2026-85091","affected":[
    {"package":{"ecosystem":"Alpine:v3.23","name":"zlib"},"ranges":[{"events":[{"introduced":"0"},{"fixed":"1.3.2-r9"}],"type":"ECOSYSTEM"}]},
    {"package":{"ecosystem":"Alpine:v3.24","name":"zlib"},"ranges":[{"events":[{"introduced":"0"},{"fixed":"1.3.2-r1"}],"type":"ECOSYSTEM"}]}]}],
  "groups":[{"ids":["ALPINE-CVE-2026-85091"],"aliases":["ALPINE-CVE-2026-85091","CVE-2026-85091"],"max_severity":"8.3"}]},
 {"package":{"name":"pyyaml","version":"6.0.3","ecosystem":"PyPI"},
  "vulnerabilities":[{"id":"GHSA-xxxx-yyyy-zzzz","aliases":["CVE-2026-11111"],"affected":[
    {"package":{"ecosystem":"PyPI","name":"pyyaml"},"ranges":[{"events":[{"introduced":"0"}],"type":"ECOSYSTEM"}]}]}],
  "groups":[{"ids":["GHSA-xxxx-yyyy-zzzz"],"aliases":["CVE-2026-11111","GHSA-xxxx-yyyy-zzzz"],"max_severity":""}]}]}]}`

func TestParse(t *testing.T) {
	findings, err := Parse([]byte(osvJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("podatności: %+v", findings)
	}
	py, z := findings[0], findings[1]
	if z.ID != "CVE-2026-85091" || z.Severity != vuln.SeverityHigh || strings.Join(z.Fixed, ",") != "1.3.2-r1" {
		t.Errorf("zlib: %+v — poprawka tylko z ekosystemu pakietu (Alpine:v3.24)", z)
	}
	if py.ID != "CVE-2026-11111" || py.Severity != vuln.SeverityUnknown || len(py.Fixed) != 0 {
		t.Errorf("pyyaml: %+v", py)
	}
}

func TestDatabases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "osv-scalibr", "PyPI", "all.zip")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for name, modified := range map[string]string{"A.json": "2026-10-06T15:46:46Z", "B.json": "2026-09-01T00:00:00Z"} {
		e, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = e.Write([]byte(`{"id":"` + name + `","modified":"` + modified + `"}`))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	dbs, err := Databases(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dbs) != 1 || dbs[0].Ecosystem != "PyPI" || dbs[0].Entries != 2 || dbs[0].Newest != "2026-10-06T15:46:46Z" || len(dbs[0].SHA256) != 64 {
		t.Errorf("bazy: %+v", dbs)
	}
}

func TestScanArchive(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		wantErr  bool
		wantSeen int
	}{
		{"podatności znalezione (kod 1) to wynik", 1, false, 2},
		{"brak pakietów (kod 128) to awaria", 128, true, 0},
		{"inny kod to awaria", 127, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var env []string
			runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
				env = c.Env
				out := c.Args[slices.Index(c.Args, "--output-file")+1]
				if err := os.WriteFile(out, []byte(osvJSON), 0o644); err != nil {
					return proc.Result{}, err
				}
				if tt.code != 0 {
					return proc.Result{}, &proc.ExitError{Cmd: c.String(), Code: tt.code}
				}
				return proc.Result{}, nil
			})
			dir := t.TempDir()
			got, err := Scanner{Runner: runner, DBDir: dir}.Scan(context.Background(), "obraz.tar", true)
			if (err != nil) != tt.wantErr || len(got) != tt.wantSeen {
				t.Fatalf("wynik %d, błąd %v", len(got), err)
			}
			if err == nil && (len(env) != 1 || env[0] != "OSV_SCANNER_LOCAL_DB_CACHE_DIRECTORY="+dir) {
				t.Errorf("środowisko skanera: %v", env)
			}
			if err != nil && cli.ExitCode(err) != cli.ExitFailure {
				t.Errorf("awaria skanera z kodem %d", cli.ExitCode(err))
			}
		})
	}
}
