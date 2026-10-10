package trial

import (
	"context"
	"errors"
	"fmt"
	"testing"

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
