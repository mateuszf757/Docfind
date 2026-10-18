package trial

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
)

// runs — biegi i zadania w pamięci; zadanie o nieopisanym biegu to błąd,
// więc test wykrywa zbędne zapytania o starsze biegi.
type runs struct {
	runs []ci.Run
	jobs map[int64]string
	err  error
}

func (r runs) WorkflowRuns(_ context.Context, workflow, branch string, limit int) ([]ci.Run, error) {
	if workflow != "ci.yml" || branch != "main" || limit != 50 {
		return nil, fmt.Errorf("zapytanie %s %s %d", workflow, branch, limit)
	}
	return r.runs, r.err
}

func (r runs) Jobs(_ context.Context, id int64) ([]ci.Job, error) {
	cluster, ok := r.jobs[id]
	if !ok {
		return nil, fmt.Errorf("nieoczekiwane zapytanie o bieg %d", id)
	}
	switch cluster {
	case "":
		return []ci.Job{{Name: "test", Conclusion: "success"}, {Name: "build", Conclusion: "failure"}, {Name: "cluster", Conclusion: "skipped"}}, nil
	case "brak":
		// Bieg sprzed dodania zadania cluster do workflowu.
		return []ci.Job{{Name: "test", Conclusion: "success"}, {Name: "build", Conclusion: "success"}}, nil
	}
	return []ci.Job{{Name: "test", Conclusion: "success"}, {Name: "cluster", Conclusion: cluster}}, nil
}

func run(id int64, event string, attempt int) ci.Run {
	return ci.Run{ID: id, Event: event, Attempt: attempt, Status: "completed", Conclusion: "success"}
}

func checker(r runs) Checker {
	return Checker{Runs: r, Workflow: "ci.yml", Job: "cluster", Branch: "main", Events: []string{"push", "schedule"}, Limit: 50}
}

func TestStreak(t *testing.T) {
	tests := []struct {
		name       string
		runs       runs
		wantStreak int
		wantReason string
	}{
		{
			name: "seria przerwana porażką; PR i bieg bez zadania nie liczą się",
			runs: runs{
				runs: []ci.Run{run(6, "schedule", 1), run(5, "pull_request", 1), run(4, "push", 1), run(3, "push", 1), run(2, "schedule", 1), run(1, "push", 1)},
				jobs: map[int64]string{6: "success", 4: "", 3: "success", 2: "failure"},
			},
			wantStreak: 2,
			wantReason: "zadanie cluster: failure",
		},
		{
			name:       "ponowienie przerywa serię, nawet zielone",
			runs:       runs{runs: []ci.Run{run(9, "schedule", 1), run(8, "push", 2)}, jobs: map[int64]string{9: "success"}},
			wantStreak: 1,
			wantReason: "bieg ponowiony (podejście 2)",
		},
		{
			name:       "bieg sprzed dodania zadania kończy przegląd",
			runs:       runs{runs: []ci.Run{run(5, "schedule", 1), run(4, "push", 1), run(3, "push", 1)}, jobs: map[int64]string{5: "success", 4: "brak"}},
			wantStreak: 1,
		},
		{
			name:       "przekroczony limit czasu to porażka",
			runs:       runs{runs: []ci.Run{run(7, "schedule", 1)}, jobs: map[int64]string{7: "timed_out"}},
			wantReason: "zadanie cluster: timed_out",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checker(tt.runs).Streak(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.Streak != tt.wantStreak || got.Reason != tt.wantReason {
				t.Errorf("seria %d (%q), oczekiwano %d (%q)", got.Streak, got.Reason, tt.wantStreak, tt.wantReason)
			}
		})
	}
	if _, err := checker(runs{err: errors.New("API")}).Streak(context.Background()); err == nil {
		t.Error("awaria API przyjęta")
	}
}

// pulls — scalone PR-y i biegi ich ostatniego commita w pamięci.
type pulls struct {
	merged []ci.PullRequest
	runs   map[string][]ci.Run
}

func (p pulls) MergedPullRequests(_ context.Context, base string, limit int) ([]ci.PullRequest, error) {
	if base != "main" || limit != 50 {
		return nil, fmt.Errorf("zapytanie %s %d", base, limit)
	}
	return p.merged, nil
}

func (p pulls) RunsForCommit(_ context.Context, workflow, sha, event string) ([]ci.Run, error) {
	if workflow != "ci.yml" || event != "pull_request" {
		return nil, fmt.Errorf("zapytanie %s %s", workflow, event)
	}
	return p.runs[sha], nil
}

func TestPullRequestStreak(t *testing.T) {
	pr := func(n int) ci.PullRequest { return ci.PullRequest{Number: n, HeadSHA: fmt.Sprintf("c%d", n)} }
	tests := []struct {
		name       string
		pulls      pulls
		jobs       map[int64]string
		wantStreak int
		wantReason string
	}{
		{
			name: "czerwone zadanie na scalonym PR-ze przerywa serię; PR bez biegu się nie liczy",
			pulls: pulls{
				merged: []ci.PullRequest{pr(9), pr(8), pr(7), pr(6), pr(5)},
				runs:   map[string][]ci.Run{"c9": {run(19, "pull_request", 1)}, "c7": {run(17, "pull_request", 1)}, "c6": {run(16, "pull_request", 1)}, "c5": {run(15, "pull_request", 1)}},
			},
			jobs:       map[int64]string{19: "success", 17: "", 16: "success", 15: "failure"},
			wantStreak: 2,
			wantReason: "PR #5 scalony z zadaniem cluster: failure",
		},
		{
			name:       "liczy się ostatni bieg commita; ponowiony przerywa",
			pulls:      pulls{merged: []ci.PullRequest{pr(4)}, runs: map[string][]ci.Run{"c4": {run(24, "pull_request", 2), run(23, "pull_request", 1)}}},
			jobs:       map[int64]string{},
			wantReason: "PR #4: bieg ponowiony (podejście 2)",
		},
		{
			name:       "PR sprzed dodania zadania kończy przegląd",
			pulls:      pulls{merged: []ci.PullRequest{pr(3), pr(2), pr(1)}, runs: map[string][]ci.Run{"c3": {run(13, "pull_request", 1)}, "c2": {run(12, "pull_request", 1)}}},
			jobs:       map[int64]string{13: "success", 12: "brak"},
			wantStreak: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checker(runs{jobs: tt.jobs}).PullRequestStreak(context.Background(), tt.pulls)
			if err != nil {
				t.Fatal(err)
			}
			if got.Streak != tt.wantStreak || got.Reason != tt.wantReason {
				t.Errorf("seria %d (%q), oczekiwano %d (%q)", got.Streak, got.Reason, tt.wantStreak, tt.wantReason)
			}
		})
	}
}

// TestStreakNightlyGap: dwie noce bez biegu przerywają serię; tak samo noc,
// która od dwóch dni nie przyszła.
func TestStreakNightlyGap(t *testing.T) {
	at := func(id int64, event, created string) ci.Run {
		r := run(id, event, 1)
		r.CreatedAt = created
		return r
	}
	now := func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) }
	c := checker(runs{
		runs: []ci.Run{at(5, "schedule", "2026-10-20T03:17:00Z"), at(4, "push", "2026-10-19T12:00:00Z"), at(3, "schedule", "2026-10-17T03:17:00Z"), at(2, "schedule", "2026-10-16T03:17:00Z")},
		jobs: map[int64]string{5: "success", 4: "success", 3: "success", 2: "success"},
	})
	c.Now = now
	got, err := c.Streak(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Streak != 2 || !strings.HasPrefix(got.Reason, "przerwa w nocach: 72h0m0s bez biegu nocnego po 2026-10-17") {
		t.Errorf("seria %d (%q)", got.Streak, got.Reason)
	}

	c = checker(runs{runs: []ci.Run{at(9, "schedule", "2026-10-17T03:17:00Z")}, jobs: map[int64]string{9: "success"}})
	c.Now = now
	got, err = c.Streak(context.Background())
	if err != nil || got.Streak != 0 || !strings.HasPrefix(got.Reason, "przerwa w nocach") {
		t.Errorf("noc sprzed trzech dni: seria %d (%q), %v", got.Streak, got.Reason, err)
	}
}
