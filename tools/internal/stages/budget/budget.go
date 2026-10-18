// Package budget to budżety warstw CI (§3.1 i §0.5 kontraktu, decyzja 57):
// p50 i p95 wall-clock każdej warstwy z ostatnich zielonych biegów danego
// zdarzenia, kolejka osobno, czasy zadań. Wall-clock warstwy to najpóźniejsze
// zakończenie jej zadania minus utworzenie biegu — tyle czeka autor PR-a
// na sygnał tej warstwy.
package budget

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
)

// Runs — biegi workflowu i ich zadania (adapters/github).
type Runs interface {
	RunsByEvent(ctx context.Context, workflow, event string, limit int) ([]ci.Run, error)
	Jobs(ctx context.Context, runID int64) ([]ci.Job, error)
}

// Budgets — budżet każdej warstwy z rejestru progów.
var Budgets = map[ci.Layer]time.Duration{
	ci.W1: contract.BudgetW1, ci.W2: contract.BudgetW2, ci.W3: contract.BudgetW3,
	ci.W4: contract.BudgetW4, ci.W5: contract.BudgetW5,
}

// Stat — percentyle z n pomiarów i liczba przekroczeń budżetu.
type Stat struct {
	N        int           `json:"n"`
	P50      time.Duration `json:"p50"`
	P95      time.Duration `json:"p95"`
	Budget   time.Duration `json:"budget,omitempty"`
	Exceeded int           `json:"exceeded,omitempty"`
}

// Report — budżety jednego zdarzenia.
type Report struct {
	Event  string            `json:"event"`
	Runs   int               `json:"runs"`
	Layers map[ci.Layer]Stat `json:"layers"`
	Queue  Stat              `json:"queue"`
	Jobs   map[string]Stat   `json:"jobs"`
}

// Measure liczy budżety z ostatnich limit zielonych biegów pierwszego
// podejścia workflowu ci dla zdarzenia: ponowienie i porażka nie mówią,
// ile trwa zielony bieg.
func Measure(ctx context.Context, r Runs, event string, limit int) (Report, error) {
	runs, err := r.RunsByEvent(ctx, "ci.yml", event, limit*2)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Event: event, Layers: map[ci.Layer]Stat{}, Jobs: map[string]Stat{}}
	layers := map[ci.Layer][]time.Duration{}
	jobs := map[string][]time.Duration{}
	var queue []time.Duration
	for _, run := range runs {
		if rep.Runs == limit {
			break
		}
		if run.Attempt > 1 || run.Conclusion != "success" {
			continue
		}
		created, err := time.Parse(time.RFC3339, run.CreatedAt)
		if err != nil {
			continue
		}
		list, err := r.Jobs(ctx, run.ID)
		if err != nil {
			return rep, err
		}
		rep.Runs++
		end := map[ci.Layer]time.Time{}
		var first time.Time
		for _, j := range list {
			if j.Conclusion == "skipped" {
				continue
			}
			start, err1 := time.Parse(time.RFC3339, j.StartedAt)
			done, err2 := time.Parse(time.RFC3339, j.CompletedAt)
			if err1 != nil || err2 != nil {
				continue
			}
			jobs[j.Name] = append(jobs[j.Name], done.Sub(start))
			if first.IsZero() || start.Before(first) {
				first = start
			}
			if layer, ok := ci.Layers[j.Name]; ok && done.After(end[layer]) {
				end[layer] = done
			}
		}
		if !first.IsZero() {
			queue = append(queue, first.Sub(created))
		}
		for layer, t := range end {
			layers[layer] = append(layers[layer], t.Sub(created))
		}
	}
	for layer, d := range layers {
		s := stat(d)
		s.Budget = Budgets[layer]
		for _, x := range d {
			if x > s.Budget {
				s.Exceeded++
			}
		}
		rep.Layers[layer] = s
	}
	rep.Queue = stat(queue)
	for name, d := range jobs {
		rep.Jobs[name] = stat(d)
	}
	return rep, nil
}

// stat — percentyle metodą najbliższej rangi.
func stat(d []time.Duration) Stat {
	if len(d) == 0 {
		return Stat{}
	}
	s := slices.Clone(d)
	slices.Sort(s)
	rank := func(p float64) time.Duration {
		i := int(float64(len(s))*p+0.999999) - 1
		return s[max(0, min(i, len(s)-1))]
	}
	return Stat{N: len(s), P50: rank(0.5), P95: rank(0.95)}
}

// Verdict — warstwy z przekroczeniem w co najmniej BudgetFail biegach
// (czerwone) i BudgetWarn (ostrzeżenie), w kolejności warstw.
func (r Report) Verdict() (fail, warn []string) {
	for _, layer := range slices.Sorted(maps.Keys(r.Layers)) {
		s := r.Layers[layer]
		msg := fmt.Sprintf("%s na %s: p95 %v przy budżecie %v, przekroczony w %d z %d biegów", layer, r.Event, s.P95.Round(time.Second), s.Budget, s.Exceeded, s.N)
		switch {
		case s.Exceeded >= contract.BudgetFail:
			fail = append(fail, msg)
		case s.Exceeded >= contract.BudgetWarn:
			warn = append(warn, msg)
		}
	}
	return fail, warn
}

// Markdown — raport do podsumowania biegu.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Budżety CI — %s (%d zielonych biegów)\n\n| Warstwa | p50 | p95 | Budżet | Przekroczenia |\n|---|---|---|---|---|\n", r.Event, r.Runs)
	for _, layer := range slices.Sorted(maps.Keys(r.Layers)) {
		s := r.Layers[layer]
		fmt.Fprintf(&b, "| %s | %v | %v | %v | %d z %d |\n", layer, s.P50.Round(time.Second), s.P95.Round(time.Second), s.Budget, s.Exceeded, s.N)
	}
	fmt.Fprintf(&b, "| kolejka | %v | %v | — | — |\n\n| Zadanie | p50 | p95 |\n|---|---|---|\n", r.Queue.P50.Round(time.Second), r.Queue.P95.Round(time.Second))
	for _, name := range slices.Sorted(maps.Keys(r.Jobs)) {
		s := r.Jobs[name]
		fmt.Fprintf(&b, "| %s | %v | %v |\n", name, s.P50.Round(time.Second), s.P95.Round(time.Second))
	}
	return b.String()
}
