package mainhealth

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

type runs []ci.Run

func (r runs) WorkflowRuns(_ context.Context, workflow, branch string, limit int) ([]ci.Run, error) {
	if workflow != "ci.yml" || branch != "main" || limit != 50 {
		return nil, fmt.Errorf("zapytanie %s %s %d", workflow, branch, limit)
	}
	return r, nil
}

type issues struct {
	open  int
	calls []string
}

func (i *issues) FindIssue(context.Context, string) (int, error) { return i.open, nil }
func (i *issues) CreateIssue(_ context.Context, _, body string) error {
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

func run(event, conclusion, created string) ci.Run {
	return ci.Run{Event: event, Conclusion: conclusion, CreatedAt: created, URL: "u-" + created}
}

func TestSync(t *testing.T) {
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
	now := func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) }
	tests := []struct {
		name  string
		runs  runs
		open  int
		want  string
		parts []string
	}{
		{"zielony, noc dziś — nic", runs{run("schedule", "success", "2026-10-20T03:17:00Z")}, 0, "", nil},
		{"czerwony od godziny — jeszcze bez issue", runs{run("push", "failure", "2026-10-20T08:00:00Z"), run("schedule", "success", "2026-10-20T03:17:00Z")}, 0, "", nil},
		{"czerwony od dwóch dni — otwiera", runs{
			run("push", "failure", "2026-10-20T08:00:00Z"),
			run("pull_request", "success", "2026-10-20T07:00:00Z"),
			run("schedule", "failure", "2026-10-20T03:17:00Z"),
			run("push", "cancelled", "2026-10-19T03:00:00Z"),
			run("push", "failure", "2026-10-18T07:00:00Z"),
			run("push", "success", "2026-10-17T07:00:00Z"),
		}, 0, "create ", []string{"main czerwony od 2026-10-18 07:00 UTC (50h0m0s)", "ostatni bieg u-2026-10-20T08:00:00Z"}},
		{"ostatni zielony — starsza czerwień się nie liczy", runs{run("push", "success", "2026-10-20T08:00:00Z"), run("push", "failure", "2026-10-15T08:00:00Z"), run("schedule", "success", "2026-10-20T03:17:00Z")}, 0, "", nil},
		{"brak nocy od trzech dni — aktualizuje", runs{run("push", "success", "2026-10-20T08:00:00Z"), run("schedule", "success", "2026-10-17T03:17:00Z")}, 5, "edit 5 ", []string{"ostatni bieg nocny 2026-10-17 03:17 UTC, 78h0m0s temu"}},
		{"żadnej nocy w historii", runs{run("push", "success", "2026-10-20T08:00:00Z")}, 0, "create ", []string{"brak biegu nocnego wśród 1"}},
		{"wszystko dobrze, issue otwarte — zamyka", runs{run("schedule", "success", "2026-10-20T03:17:00Z")}, 5, "close 5 ", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := &issues{open: tt.open}
			if err := (Checker{Runs: tt.runs, Issues: i, Now: now}).Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(i.calls) != 0 {
					t.Errorf("wywołania: %v", i.calls)
				}
				return
			}
			if len(i.calls) != 1 || !strings.HasPrefix(i.calls[0], tt.want) {
				t.Fatalf("wywołania %v, oczekiwano %q", i.calls, tt.want)
			}
			for _, p := range tt.parts {
				if !strings.Contains(i.calls[0], p) {
					t.Errorf("treść bez %q: %s", p, i.calls[0])
				}
			}
		})
	}
}
