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
	"fmt"
	"slices"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
)

// Runs — biegi workflowów (adapters/github).
type Runs interface {
	WorkflowRuns(ctx context.Context, workflow, branch string, limit int) ([]ci.Run, error)
	Jobs(ctx context.Context, runID int64) ([]ci.Job, error)
}

// Result — seria zielonych biegów i to, co ją przerwało.
type Result struct {
	Streak  int      `json:"streak"`
	Counted []ci.Run `json:"counted"`
	Broken  *ci.Run  `json:"broken,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

// Checker liczy serię dla zadania Job workflowu Workflow na gałęzi Branch.
type Checker struct {
	Runs     Runs
	Workflow string
	Job      string
	Branch   string
	// Events — zdarzenia, których biegi się liczą (push, schedule).
	Events []string
	// Limit — ile ostatnich biegów przeglądać.
	Limit int
}

// Streak przegląda biegi od najnowszego. Bieg, w którym zadanie nie ruszyło
// (wcześniejsze zadanie zawiodło), nie liczy się i serii nie przerywa;
// ponowienie albo porażka zadania — przerywa; bieg sprzed dodania zadania
// kończy przegląd.
func (c Checker) Streak(ctx context.Context) (Result, error) {
	runs, err := c.Runs.WorkflowRuns(ctx, c.Workflow, c.Branch, c.Limit)
	if err != nil {
		return Result{}, err
	}
	var r Result
	for _, run := range runs {
		if !slices.Contains(c.Events, run.Event) {
			continue
		}
		if run.Attempt > 1 {
			r.Broken, r.Reason = &run, fmt.Sprintf("bieg ponowiony (podejście %d)", run.Attempt)
			return r, nil
		}
		jobs, err := c.Runs.Jobs(ctx, run.ID)
		if err != nil {
			return r, err
		}
		i := slices.IndexFunc(jobs, func(j ci.Job) bool { return j.Name == c.Job })
		if i < 0 {
			// Workflow tego biegu nie miał jeszcze zadania — starsze biegi też
			// go nie mają, a każdy to kolejne zapytanie do API.
			return r, nil
		}
		switch jobs[i].Conclusion {
		case "success":
			r.Streak++
			r.Counted = append(r.Counted, run)
		case "skipped":
			// Zadanie nie ruszyło (np. czerwony build) — ani dowód, ani
			// porażka bramki.
		default:
			r.Broken, r.Reason = &run, "zadanie "+c.Job+": "+jobs[i].Conclusion
			return r, nil
		}
	}
	return r, nil
}
