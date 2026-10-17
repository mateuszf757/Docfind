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
	jobs map[int64][]ci.Job
}

func (r runs) RunsByEvent(_ context.Context, workflow, event string, limit int) ([]ci.Run, error) {
	if workflow != "ci.yml" || event != "pull_request" || limit != 40 {
		return nil, fmt.Errorf("zapytanie %s %s %d", workflow, event, limit)
	}
	return r.list, nil
}

func (r runs) Jobs(_ context.Context, id int64) ([]ci.Job, error) { return r.jobs[id], nil }

var base = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

func job(name string, start, end time.Duration) ci.Job {
	return ci.Job{Name: name, Conclusion: "success", StartedAt: base.Add(start).Format(time.RFC3339), CompletedAt: base.Add(end).Format(time.RFC3339)}
}

func TestMeasure(t *testing.T) {
	r := runs{jobs: map[int64][]ci.Job{}}
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
		r.jobs[i] = []ci.Job{
			job("test", 30*time.Second, 3*time.Minute),
			job("build", 30*time.Second, w1),
			job("cluster", w1, 18*time.Minute),
			{Name: "build-publish", Conclusion: "skipped", StartedAt: base.Format(time.RFC3339), CompletedAt: base.Format(time.RFC3339)},
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
	fail, warn := rep.Verdict()
	if len(fail) != 1 || !strings.Contains(fail[0], "W1 na pull_request: p95 8m0s przy budżecie 6m0s, przekroczony w 5 z 20") || len(warn) != 0 {
		t.Errorf("werdykt: %q / %q", fail, warn)
	}
	if md := rep.Markdown(); !strings.Contains(md, "| W1 | 5m0s | 8m0s | 6m0s | 5 z 20 |") || !strings.Contains(md, "| kolejka | 30s |") {
		t.Errorf("raport:\n%s", md)
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
