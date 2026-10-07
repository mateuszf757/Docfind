// Package trial mówi, czy nowa bramka skończyła okres próbny: ile biegów
// z rzędu na kodzie już scalonym przeszła za pierwszym podejściem.
//
// Bramka, która migocze, uczy ignorowania czerwonego. Dlatego nowe zadanie
// CI (cluster) nie trafia od razu do wymaganych statusów reguły main, tylko
// po serii zielonych biegów: nocnych i po scaleniu, bez ponowień — ponowienie
// to porażka, którą ktoś przykrył. Biegi na PR-ach się nie liczą: czerwony
// PR częściej znaczy zły kod niż zawodną bramkę.
package trial

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// Run — bieg workflowu z API GitHuba.
type Run struct {
	ID         int64  `json:"id"`
	Event      string `json:"event"`
	Attempt    int    `json:"run_attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadSHA    string `json:"head_sha"`
	CreatedAt  string `json:"created_at"`
	URL        string `json:"html_url"`
}

// Result — seria zielonych biegów i to, co ją przerwało.
type Result struct {
	Streak  int    `json:"streak"`
	Counted []Run  `json:"counted"`
	Broken  *Run   `json:"broken,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Checker liczy serię dla zadania Job workflowu Workflow na gałęzi Branch.
type Checker struct {
	Runner   proc.Runner
	Workflow string
	Job      string
	Branch   string
	// Events — zdarzenia, których biegi się liczą (push, schedule).
	Events []string
	// Limit — ile ostatnich biegów przeglądać.
	Limit int
}

func (c Checker) gh(ctx context.Context, path string, v any) error {
	res, err := c.Runner.Run(ctx, proc.Cmd{Name: "gh", Args: []string{"api", path}})
	if err != nil {
		return fmt.Errorf("gh api %s: %w", path, err)
	}
	if err := json.Unmarshal(res.Stdout, v); err != nil {
		return fmt.Errorf("gh api %s: %w", path, err)
	}
	return nil
}

// Streak przegląda biegi od najnowszego. Bieg, w którym zadanie nie ruszyło
// (wcześniejsze zadanie zawiodło), nie liczy się i serii nie przerywa;
// ponowienie albo porażka zadania — przerywa.
func (c Checker) Streak(ctx context.Context) (Result, error) {
	var runs struct {
		Runs []Run `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/{owner}/{repo}/actions/workflows/%s/runs?branch=%s&status=completed&per_page=%d", c.Workflow, c.Branch, c.Limit)
	if err := c.gh(ctx, path, &runs); err != nil {
		return Result{}, err
	}
	var r Result
	for _, run := range runs.Runs {
		if !slices.Contains(c.Events, run.Event) {
			continue
		}
		if run.Attempt > 1 {
			r.Broken, r.Reason = &run, fmt.Sprintf("bieg ponowiony (podejście %d)", run.Attempt)
			return r, nil
		}
		var jobs struct {
			Jobs []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
			} `json:"jobs"`
		}
		if err := c.gh(ctx, fmt.Sprintf("repos/{owner}/{repo}/actions/runs/%d/jobs", run.ID), &jobs); err != nil {
			return r, err
		}
		conclusion := ""
		for _, j := range jobs.Jobs {
			if j.Name == c.Job {
				conclusion = j.Conclusion
			}
		}
		switch conclusion {
		case "success":
			r.Streak++
			r.Counted = append(r.Counted, run)
		case "", "skipped":
			// Zadanie nie ruszyło (np. czerwony build) albo bieg sprzed jego
			// istnienia — ani dowód, ani porażka bramki.
		default:
			r.Broken, r.Reason = &run, "zadanie "+c.Job+": "+conclusion
			return r, nil
		}
	}
	return r, nil
}
