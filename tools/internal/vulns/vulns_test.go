package vulns

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
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
	if z.ID != "CVE-2026-85091" || z.Severity != SeverityHigh || strings.Join(z.Fixed, ",") != "1.3.2-r1" {
		t.Errorf("zlib: %+v — poprawka tylko z ekosystemu pakietu (Alpine:v3.24)", z)
	}
	if py.ID != "CVE-2026-11111" || py.Severity != SeverityUnknown || len(py.Fixed) != 0 {
		t.Errorf("pyyaml: %+v", py)
	}
}

func finding(id, pkg, score string, fixed ...string) Finding {
	return Finding{ID: id, Package: pkg, Score: score, Severity: severity(score), Fixed: fixed}
}

func TestEvaluate(t *testing.T) {
	today := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	findings := []Finding{
		finding("CVE-1", "zlib", "8.3", "1.3.2-r1"),   // naprawialna HIGH — blokuje
		finding("CVE-2", "openssl", "9.8"),            // bez poprawki
		finding("CVE-3", "pip", "5.0", "26.3"),        // MEDIUM
		finding("CVE-4", "musl", "", "1.2.7-r0"),      // nieznana waga, naprawialna — blokuje
		finding("CVE-5", "pyyaml", "9.1", "6.0.4"),    // wyjątek ważny
		finding("CVE-6", "uvicorn", "7.5", "0.54.0"),  // wyjątek wygasł — blokuje
		finding("CVE-7", "starlette", "7.1", "1.6.1"), // jest w obrazie bazowym
		finding("CVE-8", "fastapi", "7.1", "0.142.0"), // wyjątek dla innego pakietu
		{ID: "GHSA-a", Aliases: []string{"CVE-9"}, Package: "idna", Score: "7.0", Severity: SeverityHigh, Fixed: []string{"3.21"}}, // wyjątek po aliasie
	}
	p := Policy{
		Today: today,
		Exceptions: []Exception{
			{ID: "CVE-5", Package: "pyyaml", Expires: "2026-10-20", Reason: "poprawka wymaga zmiany API konfiguracji"},
			{ID: "CVE-6", Package: "uvicorn", Expires: "2026-10-07", Reason: "czekamy na wydanie z poprawką regresji"},
			{ID: "CVE-8", Package: "starlette", Expires: "2026-10-20", Reason: "wyjątek dla innego pakietu niż podatny"},
			{ID: "cve-9", Package: "IDNA", Expires: "2026-10-20", Reason: "dopasowanie po aliasie i bez wielkości liter"},
			{ID: "CVE-404", Package: "zlib", Expires: "2026-10-20", Reason: "podatność już naprawiona, wyjątek do usunięcia"},
		},
		Baseline: []Finding{finding("CVE-7", "starlette", "7.1", "1.6.1")},
	}
	got, unused := p.Evaluate(findings)
	blocking := map[string]bool{}
	for _, f := range got {
		blocking[f.ID] = f.Blocking
	}
	want := map[string]bool{"CVE-1": true, "CVE-2": false, "CVE-3": false, "CVE-4": true, "CVE-5": false, "CVE-6": true, "CVE-7": false, "CVE-8": true, "GHSA-a": false}
	for id, w := range want {
		if blocking[id] != w {
			t.Errorf("%s: blokuje %v, oczekiwano %v", id, blocking[id], w)
		}
	}
	if strings.Join(unused, ",") != "CVE-8 (starlette),CVE-404 (zlib)" {
		t.Errorf("nieużyte wyjątki: %v", unused)
	}
	for _, f := range got {
		if f.ID == "CVE-6" && !strings.Contains(f.Note, "wygasł 2026-10-07") {
			t.Errorf("CVE-6: %q — wyjątek wygasa z początkiem dnia z datą", f.Note)
		}
	}
}

func writeExceptions(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vuln-exceptions.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExceptions(t *testing.T) {
	today := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	ok := "exceptions:\n  - id: CVE-2026-85091\n    package: zlib\n    expires: 2026-10-21\n    reason: poprawka przyjdzie z odświeżeniem obrazu bazowego\n"
	if got, err := LoadExceptions(writeExceptions(t, ok), today); err != nil || len(got) != 1 {
		t.Fatalf("poprawny plik: %v, %v", got, err)
	}
	if got, err := LoadExceptions(writeExceptions(t, "exceptions: []\n"), today); err != nil || len(got) != 0 {
		t.Fatalf("pusta lista: %v, %v", got, err)
	}
	tests := []struct{ name, from, to, want string }{
		{"literówka w polu", "    reason:", "    reasn:", "unknown field"},
		{"bez uzasadnienia", "reason: poprawka przyjdzie z odświeżeniem obrazu bazowego", "reason: tak", "uzasadnienie"},
		{"zła data", "expires: 2026-10-21", "expires: 21.10.2026", "RRRR-MM-DD"},
		{"za daleko", "expires: 2026-10-21", "expires: 2027-10-21", "dalej niż 90 dni"},
		{"bez pakietu", "    package: zlib\n", "", "id i package wymagane"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := strings.Replace(ok, tt.from, tt.to, 1)
			if content == ok {
				t.Fatalf("podmiana %q nie zadziałała", tt.from)
			}
			_, err := LoadExceptions(writeExceptions(t, content), today)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("oczekiwano błędu z %q, jest %v", tt.want, err)
			}
		})
	}
}

// TestRepositoryExceptions: wyjątki w repozytorium przechodzą walidację
// dziś (w CI — w dniu biegu).
func TestRepositoryExceptions(t *testing.T) {
	if _, err := LoadExceptions(filepath.Join("../../..", ExceptionsFile), time.Now()); err != nil {
		t.Fatal(err)
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
			got, err := Scanner{Runner: runner, DBDir: dir}.ScanArchive(context.Background(), "obraz.tar", filepath.Join(dir, "osv.json"), true)
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

// govulncheckJSON — strumień `govulncheck -format json` (v1.8.0): ustalenia
// na poziomie modułu, pakietu i symbolu dla jednej podatności i ustalenie
// symbolu bez poprawki dla drugiej.
const govulncheckJSON = `{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scanner_version":"v1.8.0"}}
{"osv":{"id":"GO-2026-0001","aliases":["CVE-2026-22222"]}}
{"osv":{"id":"GO-2026-0002","aliases":[]}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v0.36.6","trace":[{"module":"k8s.io/client-go","version":"v0.36.5"}]}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v0.36.6","trace":[{"module":"k8s.io/client-go","version":"v0.36.5","package":"k8s.io/client-go/rest"}]}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v0.36.6","trace":[{"module":"k8s.io/client-go","version":"v0.36.5","package":"k8s.io/client-go/rest","function":"Do"},{"module":"github.com/mateuszf757/Docfind/tools","function":"main"}]}}
{"finding":{"osv":"GO-2026-0001","fixed_version":"v0.36.6","trace":[{"module":"k8s.io/client-go","version":"v0.36.5","package":"k8s.io/client-go/rest","function":"DoRaw"}]}}
{"finding":{"osv":"GO-2026-0002","trace":[{"module":"golang.org/x/net","version":"v0.50.0","package":"golang.org/x/net/http2","function":"Serve"}]}}
{"finding":{"osv":"GO-2026-0003","fixed_version":"v1.2.3","trace":[{"module":"example.com/niewolany","version":"v1.2.0"}]}}
`

func TestParseGovulncheck(t *testing.T) {
	findings, err := ParseGovulncheck(strings.NewReader(govulncheckJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("ustalenia osiągalne: %+v — tylko poziom symbolu, bez powtórzeń", findings)
	}
	net, client := findings[0], findings[1]
	if client.ID != "GO-2026-0001" || client.Package != "k8s.io/client-go" || strings.Join(client.Fixed, ",") != "v0.36.6" || strings.Join(client.Aliases, ",") != "CVE-2026-22222" {
		t.Errorf("client-go: %+v", client)
	}
	if net.ID != "GO-2026-0002" || len(net.Fixed) != 0 {
		t.Errorf("x/net: %+v", net)
	}
	got, _ := Policy{Today: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)}.Evaluate(findings)
	if !got[1].Blocking || got[0].Blocking {
		t.Errorf("osiągalna z poprawką blokuje, bez poprawki nie: %+v", got)
	}
}

func TestInScope(t *testing.T) {
	all := []Exception{{ID: "a"}, {ID: "b", Scope: ScopeGo}, {ID: "c", Scope: ScopeImage}}
	if got := InScope(all, ScopeImage); len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("obraz: %+v", got)
	}
	if got := InScope(all, ScopeGo); len(got) != 1 || got[0].ID != "b" {
		t.Errorf("go: %+v", got)
	}
}
