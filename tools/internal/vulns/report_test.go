package vulns

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

const listCmd = "gh issue list --state open --search " + IssueTitle + " in:title --json number,title"

func TestSyncIssue(t *testing.T) {
	blocking := Report{Target: "obraz.tar", Blocking: 1, ScannedAt: time.Date(2026, 10, 8, 3, 30, 0, 0, time.UTC),
		Findings: []Finding{{ID: "CVE-1", Package: "zlib", Severity: SeverityHigh, Fixed: []string{"1.3.2-r1"}, Blocking: true}}}
	clean := Report{Target: "obraz.tar", ScannedAt: blocking.ScannedAt}
	open := `[{"number":7,"title":"` + IssueTitle + `"},{"number":9,"title":"` + IssueTitle + ` — stare"}]`
	tests := []struct {
		name     string
		rep      Report
		list     string
		wantCall string
	}{
		{"blokujące, brak issue — otwiera", blocking, "[]", "gh issue create --title " + IssueTitle},
		{"blokujące, issue otwarte — aktualizuje dokładnie to o tym tytule", blocking, open, "gh issue edit 7 --body"},
		{"czysto, issue otwarte — zamyka", clean, open, "gh issue close 7 --comment Skan z 2026-10-08: brak blokujących podatności."},
		{"czysto, brak issue — nic", clean, "[]", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
				calls = append(calls, c.String())
				if c.String() == listCmd {
					return proc.Result{Stdout: []byte(tt.list)}, nil
				}
				return proc.Result{}, nil
			})
			if err := SyncIssue(context.Background(), runner, tt.rep); err != nil {
				t.Fatal(err)
			}
			if tt.wantCall == "" {
				if len(calls) != 1 {
					t.Errorf("wywołania: %v", calls)
				}
				return
			}
			if len(calls) != 2 || !strings.HasPrefix(calls[1], tt.wantCall) {
				t.Errorf("wywołania: %v, oczekiwano %q", calls, tt.wantCall)
			}
			if strings.Contains(tt.wantCall, "--title") || strings.Contains(tt.wantCall, "--body") {
				if !strings.Contains(calls[1], "| HIGH | CVE-1 | zlib") {
					t.Errorf("treść bez tabeli podatności: %s", calls[1])
				}
			}
		})
	}
}
