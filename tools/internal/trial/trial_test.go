package trial

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

func runsJSON(runs ...string) string {
	return `{"workflow_runs":[` + strings.Join(runs, ",") + `]}`
}

func run(id int64, event string, attempt int) string {
	return fmt.Sprintf(`{"id":%d,"event":%q,"run_attempt":%d,"status":"completed","conclusion":"success"}`, id, event, attempt)
}

func jobs(cluster string) string {
	switch cluster {
	case "":
		return `{"jobs":[{"name":"test","conclusion":"success"},{"name":"build","conclusion":"failure"},{"name":"cluster","conclusion":"skipped"}]}`
	case "brak":
		// Bieg sprzed dodania zadania cluster do workflowu.
		return `{"jobs":[{"name":"test","conclusion":"success"},{"name":"build","conclusion":"success"}]}`
	}
	return `{"jobs":[{"name":"test","conclusion":"success"},{"name":"build","conclusion":"success"},{"name":"cluster","conclusion":"` + cluster + `"}]}`
}

const runsPath = "gh api repos/{owner}/{repo}/actions/workflows/ci.yml/runs?branch=main&status=completed&per_page=50"

func jobsPath(id int64) string {
	return fmt.Sprintf("gh api repos/{owner}/{repo}/actions/runs/%d/jobs", id)
}

func checker(responses map[string]proc.FakeResponse) Checker {
	return Checker{Runner: &proc.Fake{Responses: responses}, Workflow: "ci.yml", Job: "cluster", Branch: "main", Events: []string{"push", "schedule"}, Limit: 50}
}

func TestStreak(t *testing.T) {
	tests := []struct {
		name       string
		responses  map[string]proc.FakeResponse
		wantStreak int
		wantReason string
	}{
		{
			name: "seria przerwana porażką; PR i bieg bez zadania nie liczą się",
			responses: map[string]proc.FakeResponse{
				runsPath:    {Stdout: runsJSON(run(6, "schedule", 1), run(5, "pull_request", 1), run(4, "push", 1), run(3, "push", 1), run(2, "schedule", 1), run(1, "push", 1))},
				jobsPath(6): {Stdout: jobs("success")},
				jobsPath(4): {Stdout: jobs("")},
				jobsPath(3): {Stdout: jobs("success")},
				jobsPath(2): {Stdout: jobs("failure")},
			},
			wantStreak: 2,
			wantReason: "zadanie cluster: failure",
		},
		{
			name: "ponowienie przerywa serię, nawet zielone",
			responses: map[string]proc.FakeResponse{
				runsPath:    {Stdout: runsJSON(run(9, "schedule", 1), run(8, "push", 2))},
				jobsPath(9): {Stdout: jobs("success")},
			},
			wantStreak: 1,
			wantReason: "bieg ponowiony (podejście 2)",
		},
		{
			// Bez jobsPath(3): Fake zawiódłby przy zapytaniu o starszy bieg.
			name: "bieg sprzed dodania zadania kończy przegląd",
			responses: map[string]proc.FakeResponse{
				runsPath:    {Stdout: runsJSON(run(5, "schedule", 1), run(4, "push", 1), run(3, "push", 1))},
				jobsPath(5): {Stdout: jobs("success")},
				jobsPath(4): {Stdout: jobs("brak")},
			},
			wantStreak: 1,
		},
		{
			name: "przekroczony limit czasu to porażka",
			responses: map[string]proc.FakeResponse{
				runsPath:    {Stdout: runsJSON(run(7, "schedule", 1))},
				jobsPath(7): {Stdout: jobs("timed_out")},
			},
			wantReason: "zadanie cluster: timed_out",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := checker(tt.responses).Streak(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.Streak != tt.wantStreak || got.Reason != tt.wantReason {
				t.Errorf("seria %d (%q), oczekiwano %d (%q)", got.Streak, got.Reason, tt.wantStreak, tt.wantReason)
			}
		})
	}
}

func TestStreakUnreadableAPI(t *testing.T) {
	_, err := checker(map[string]proc.FakeResponse{runsPath: {Stdout: "<html>"}}).Streak(context.Background())
	if err == nil {
		t.Error("nieczytelna odpowiedź API przyjęta")
	}
}
