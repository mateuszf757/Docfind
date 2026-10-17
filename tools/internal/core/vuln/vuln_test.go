package vuln

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func finding(id, pkg, score string, fixed ...string) Finding {
	return Finding{ID: id, Package: pkg, Score: score, Severity: SeverityFromCVSS(score), Fixed: fixed}
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
	if _, err := LoadExceptions(filepath.Join("../../../..", ExceptionsFile), time.Now()); err != nil {
		t.Fatal(err)
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

func TestSeverityFromCVSS(t *testing.T) {
	for score, want := range map[string]string{"9.8": SeverityCritical, "9.0": SeverityCritical, "7.0": SeverityHigh, "4.0": SeverityMedium, "3.9": SeverityLow, "": SeverityUnknown, "0": SeverityUnknown, "x": SeverityUnknown} {
		if got := SeverityFromCVSS(score); got != want {
			t.Errorf("%q: %s, oczekiwano %s", score, got, want)
		}
	}
}

func TestIssueBody(t *testing.T) {
	image := Report{Subject: "obrazu api", Target: "obraz.tar", Scanner: "osv-scanner", Blocking: 1,
		Findings:  []Finding{{ID: "CVE-1", Package: "zlib", Severity: SeverityHigh, Fixed: []string{"1.3.2-r1"}, Blocking: true, Note: "a|b"}},
		Databases: []Database{{Ecosystem: "Alpine", Newest: "2026-10-06", SHA256: strings.Repeat("a", 64)}}}
	code := Report{Subject: "kodu narzędzi Go", Target: "tools/", Scanner: "govulncheck", Baseline: "tools/ z abc",
		Databases: []Database{{Ecosystem: "Go (https://vuln.go.dev)", Newest: "2026-10-08T22:31:09Z"}}}
	body := IssueBody([]Report{image, code}, []string{"vulns-go.json"})
	for _, want := range []string{
		"### Podatności obrazu api", "| HIGH | CVE-1 | zlib  | 1.3.2-r1 | tak | a/b |", "osv-scanner, bazy: Alpine do 2026-10-06 (`aaaaaaaaaaaa`)",
		"### Podatności kodu narzędzi Go", "punkt odniesienia: tools/ z abc", "govulncheck, bazy: Go (https://vuln.go.dev) do 2026-10-08T22:31:09Z\n",
		"**Bez raportu:** vulns-go.json", "dft vulns-issue",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("treść bez %q:\n%s", want, body)
		}
	}
}

func TestExpiring(t *testing.T) {
	today := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	exceptions := []Exception{
		{ID: "CVE-1", Package: "zlib", Expires: "2026-10-28"},
		{ID: "CVE-2", Package: "pip", Expires: "2026-10-29"},
		{ID: "CVE-3", Package: "musl", Expires: "2026-10-21"},
		{ID: "CVE-4", Package: "x", Expires: "2026-10-20"},
		{ID: "CVE-5", Package: "y", Expires: "jutro"},
	}
	got := Expiring(exceptions, today, 7*24*time.Hour)
	// Za tydzień — tak; za osiem dni — jeszcze nie; dziś już nie działa,
	// więc to nie ostrzeżenie, tylko blokada.
	if want := "CVE-1 (zlib) wygasa 2026-10-28"; len(got) != 1 || got[0] != want {
		t.Errorf("Expiring = %q; oczekiwano [%q]", got, want)
	}
}
