package vulns

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

const repo = "ghcr.io/mateuszf757/docfind-api"

// scanner — podatności po nazwie archiwum: obraz.tar (skanowany) albo
// baseline.tar (obraz bazowy).
type scanner struct{ downloads []bool }

func (s *scanner) Scan(_ context.Context, archive string, download bool) ([]vuln.Finding, error) {
	s.downloads = append(s.downloads, download)
	zlib := vuln.Finding{ID: "CVE-1", Package: "zlib", Severity: vuln.SeverityHigh, Fixed: []string{"1.3.2-r1"}}
	if filepath.Base(archive) == "baseline.tar" {
		return []vuln.Finding{zlib}, nil
	}
	return []vuln.Finding{zlib, {ID: "CVE-2", Package: "pip", Severity: vuln.SeverityCritical, Fixed: []string{"26.3"}}}, nil
}

func (s *scanner) Databases() ([]vuln.Database, error) {
	return []vuln.Database{{Ecosystem: "Alpine", Entries: 3, SHA256: strings.Repeat("a", 64)}}, nil
}

// store — rejestr i demon: zapisuje archiwum, a nieznany obraz to
// artifact.ErrNotFound.
type store map[string]bool

func (s store) Save(_ context.Context, ref, path string) error {
	if !s[ref] {
		return fmt.Errorf("%s: %w", ref, artifact.ErrNotFound)
	}
	return os.WriteFile(path, []byte(ref), 0o644)
}

func TestRun(t *testing.T) {
	quiet(t)
	base := repo + ":" + strings.Repeat("b", 12)
	newScan := func(sc *scanner) Scan {
		return Scan{Scanner: sc, Registry: store{base: true, repo + ":pr": true}, Images: store{repo + ":lokalny": true},
			Today: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), Repository: repo}
	}

	sc := &scanner{}
	rep, err := newScan(sc).Run(context.Background(), Target{Image: repo + ":pr"}, strings.Repeat("b", 40), false)
	if cli.ExitCode(err) != cli.ExitUnmet || rep.Blocking != 1 || rep.Baseline != base {
		t.Fatalf("PR względem main: %v, %+v", err, rep)
	}
	// Bazy pobierane raz na bieg: obraz bazowy oceniony tą samą migawką.
	if !slices.Equal(sc.downloads, []bool{true, false}) {
		t.Errorf("pobrania baz: %v", sc.downloads)
	}

	// Bez obrazu bazowego blokuje każda naprawialna HIGH i CRITICAL; tylko
	// raport — bez blokowania.
	rep, err = newScan(&scanner{}).Run(context.Background(), Target{Local: repo + ":lokalny"}, strings.Repeat("c", 40), true)
	if err != nil || rep.Blocking != 2 || rep.Baseline != "" || !rep.ReportOnly {
		t.Errorf("bez obrazu bazowego, tylko raport: %v, %+v", err, rep)
	}
	if _, err := newScan(&scanner{}).Run(context.Background(), Target{Local: repo + ":lokalny"}, "krótki", false); cli.ExitCode(err) != cli.ExitFailure {
		t.Errorf("skrócony commit bazowy: %v", err)
	}
}

type issues struct {
	open  int
	calls []string
}

func (i *issues) FindIssue(context.Context, string) (int, error) { return i.open, nil }
func (i *issues) CreateIssue(_ context.Context, title, body string) error {
	i.calls = append(i.calls, "create "+body)
	return nil
}
func (i *issues) EditIssue(_ context.Context, n int, body string) error {
	i.calls = append(i.calls, fmt.Sprintf("edit %d %s", n, body))
	return nil
}
func (i *issues) CloseIssue(_ context.Context, n int, comment string) error {
	i.calls = append(i.calls, fmt.Sprintf("close %d %s", n, comment))
	return nil
}

func TestSyncIssue(t *testing.T) {
	quiet(t)
	scanned := time.Date(2026, 10, 8, 3, 30, 0, 0, time.UTC)
	blocking := vuln.Report{Subject: "obrazu api", Target: "obraz.tar", Blocking: 1, ScannedAt: scanned,
		Findings: []vuln.Finding{{ID: "CVE-1", Package: "zlib", Severity: vuln.SeverityHigh, Fixed: []string{"1.3.2-r1"}, Blocking: true}}}
	clean := vuln.Report{Subject: "obrazu api", Target: "obraz.tar", ScannedAt: scanned}
	cleanGo := vuln.Report{Subject: "kodu narzędzi Go", Target: "tools/", ScannedAt: scanned}
	tests := []struct {
		name     string
		reports  []vuln.Report
		missing  []string
		open     int
		wantCall string
		wantCode int
	}{
		{"blokujące, brak issue — otwiera", []vuln.Report{blocking, cleanGo}, nil, 0, "create ### Podatności obrazu api", cli.ExitOK},
		{"blokujące, issue otwarte — aktualizuje", []vuln.Report{blocking, cleanGo}, nil, 7, "edit 7 ### Podatności obrazu api", cli.ExitOK},
		{"czysto, issue otwarte — zamyka", []vuln.Report{clean, cleanGo}, nil, 7, "close 7 Skan z 2026-10-08: brak blokujących podatności.", cli.ExitOK},
		{"czysto, brak issue — nic", []vuln.Report{clean, cleanGo}, nil, 0, "", cli.ExitOK},
		// Brak raportu to brak wiedzy: issue nie jest zamykane, a bieg nie
		// wygląda na kompletny.
		{"czysto, ale bez raportu Go — nie zamyka", []vuln.Report{clean}, []string{"vulns-go.json"}, 7, "", cli.ExitUnmet},
		{"blokujące, bez raportu Go — aktualizuje z adnotacją", []vuln.Report{blocking}, []string{"vulns-go.json"}, 7, "edit 7 ### Podatności obrazu api", cli.ExitUnmet},
		{"żadnego raportu — nic", nil, []string{"vulns-api.json", "vulns-go.json"}, 7, "", cli.ExitUnmet},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := &issues{open: tt.open}
			err := SyncIssue(context.Background(), i, tt.reports, tt.missing)
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if tt.wantCall == "" {
				if len(i.calls) != 0 {
					t.Errorf("wywołania: %v", i.calls)
				}
				return
			}
			if len(i.calls) != 1 || !strings.HasPrefix(i.calls[0], tt.wantCall) {
				t.Fatalf("wywołania: %v, oczekiwano %q", i.calls, tt.wantCall)
			}
			if strings.HasPrefix(tt.wantCall, "close") {
				return
			}
			if !strings.Contains(i.calls[0], "| HIGH | CVE-1 | zlib") {
				t.Errorf("treść bez tabeli podatności: %s", i.calls[0])
			}
			for _, m := range tt.missing {
				if !strings.Contains(i.calls[0], m) {
					t.Errorf("treść bez skanu bez raportu %s: %s", m, i.calls[0])
				}
			}
		})
	}
}
