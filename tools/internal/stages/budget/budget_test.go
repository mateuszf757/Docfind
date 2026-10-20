package budget

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
)

// runs — biegi w pamięci: zadania każdego biegu jako przesunięcia od
// utworzenia biegu.
type runs struct {
	list []ci.Run
	jobs map[int64][]ci.JobSteps
}

func (r runs) RunsByEvent(_ context.Context, workflow, event string, limit int) ([]ci.Run, error) {
	if workflow != "ci.yml" || event != "pull_request" || limit != 40 {
		return nil, fmt.Errorf("zapytanie %s %s %d", workflow, event, limit)
	}
	return r.list, nil
}

func (r runs) JobSteps(_ context.Context, id int64) ([]ci.JobSteps, error) { return r.jobs[id], nil }

var base = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

func job(name string, start, end time.Duration, steps ...ci.Step) ci.JobSteps {
	return ci.JobSteps{Job: ci.Job{Name: name, Conclusion: "success", StartedAt: base.Add(start).Format(time.RFC3339), CompletedAt: base.Add(end).Format(time.RFC3339)}, Steps: steps}
}

func step(name string, start, end time.Duration) ci.Step {
	return ci.Step{Name: name, Conclusion: "success", StartedAt: base.Add(start).Format(time.RFC3339), CompletedAt: base.Add(end).Format(time.RFC3339)}
}

func TestMeasure(t *testing.T) {
	r := runs{jobs: map[int64][]ci.JobSteps{}}
	// 20 zielonych biegów: W1 kończy się po 5 min, a w pięciu po 8 min; W2
	// po 18 min. Do tego bieg ponowiony i czerwony — poza pomiarem.
	for i := int64(1); i <= 22; i++ {
		run := ci.Run{ID: i, Event: "pull_request", Attempt: 1, Conclusion: "success", CreatedAt: base.Format(time.RFC3339)}
		switch i {
		case 21:
			run.Attempt = 2
		case 22:
			run.Conclusion = "failure"
		}
		r.list = append(r.list, run)
		w1 := 5 * time.Minute
		if i <= 5 {
			w1 = 8 * time.Minute
		}
		r.jobs[i] = []ci.JobSteps{
			// Podatek test: 2 + 3 + 4 + 40 s; „przypięte wersje” to już praca.
			job("test", 30*time.Second, 3*time.Minute,
				step("Set up job", 30*time.Second, 32*time.Second),
				step("Run actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", 32*time.Second, 35*time.Second),
				step("narzędzia (mise)", 35*time.Second, 39*time.Second),
				step("dft", 39*time.Second, 79*time.Second),
				step("przypięte wersje", 79*time.Second, 84*time.Second)),
			job("build", 30*time.Second, w1, step("Go (mise)", 30*time.Second, 40*time.Second)),
			job("cluster", w1, 18*time.Minute, step("dft", w1, w1+45*time.Second)),
			{Job: ci.Job{Name: "build-publish", Conclusion: "skipped", StartedAt: base.Format(time.RFC3339), CompletedAt: base.Format(time.RFC3339)}},
		}
	}
	rep, err := Measure(context.Background(), r, "pull_request", 20)
	if err != nil {
		t.Fatal(err)
	}
	w1, w2 := rep.Layers[ci.W1], rep.Layers[ci.W2]
	if rep.Runs != 20 || w1.N != 20 || w1.P50 != 5*time.Minute || w1.P95 != 8*time.Minute || w1.Exceeded != 5 || w1.Budget != 6*time.Minute {
		t.Errorf("W1: %+v (biegów %d)", w1, rep.Runs)
	}
	if w2.P95 != 18*time.Minute || w2.Exceeded != 0 {
		t.Errorf("W2: %+v", w2)
	}
	if rep.Queue.P50 != 30*time.Second {
		t.Errorf("kolejka: %+v", rep.Queue)
	}
	if _, ok := rep.Jobs["build-publish"]; ok {
		t.Error("pominięte zadanie w czasach zadań")
	}
	if s := rep.Setup["test"]; s.P50 != 49*time.Second || s.N != 20 {
		t.Errorf("podatek test: %+v", s)
	}
	if s := rep.Setup["build"]; s.P50 != 10*time.Second {
		t.Errorf("podatek build: %+v", s)
	}
	// Suma W1 w biegu: test 49 s + build 10 s; cluster (W2) się nie liczy.
	if rep.SetupW1.P50 != 59*time.Second {
		t.Errorf("podatek W1 na bieg: %+v", rep.SetupW1)
	}
	fail, warn := rep.Verdict()
	if len(fail) != 1 || !strings.Contains(fail[0], "W1 na pull_request: p95 8m0s przy budżecie 6m0s, przekroczony w 5 z 20") {
		t.Errorf("werdykt: %q", fail)
	}
	// Ostrzeżenie tylko dla zadania W1 ponad 30 s — cluster (45 s) jest w W2.
	if len(warn) != 1 || !strings.Contains(warn[0], "podatek startowy test na pull_request: p50 49s ponad 30s") {
		t.Errorf("ostrzeżenia: %q", warn)
	}
	md := rep.Markdown()
	for _, want := range []string{"| W1 | 5m0s | 8m0s | 6m0s | 5 z 20 |", "| kolejka | 30s |", "| podatek startowy W1 na bieg | 59s |", "| test | 2m30s | 2m30s | 49s |"} {
		if !strings.Contains(md, want) {
			t.Errorf("raport bez %q:\n%s", want, md)
		}
	}
}

func TestStat(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 20; i++ {
		d = append(d, time.Duration(i)*time.Minute)
	}
	if s := stat(d); s.P50 != 10*time.Minute || s.P95 != 19*time.Minute || s.N != 20 {
		t.Errorf("stat: %+v", s)
	}
	if s := stat(nil); s.N != 0 {
		t.Errorf("pusty: %+v", s)
	}
}

func TestSetupWarningsPerRun(t *testing.T) {
	rep := Report{Event: "push", Setup: map[string]Stat{"test": {N: 20, P50: 20 * time.Second}}, SetupW1: Stat{N: 20, P50: 4 * time.Minute}}
	warn := rep.SetupWarnings()
	if len(warn) != 1 || !strings.Contains(warn[0], "podatek startowy zadań W1 na push: p50 4m0s na bieg ponad 3m0s") {
		t.Errorf("ostrzeżenia: %q", warn)
	}
	if warn := (Report{Event: "push"}).SetupWarnings(); len(warn) != 0 {
		t.Errorf("bez biegów: %q", warn)
	}
}
