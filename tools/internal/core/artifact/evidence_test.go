package artifact

import (
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
)

// evidence — pakiet commita wydania z zielonym biegiem main.
func evidence() Evidence {
	r := release()
	var jobs []ci.Job
	for _, name := range append([]string{"build", "vulns"}, MainJobs...) {
		jobs = append(jobs, ci.Job{Name: name, Conclusion: "success"})
	}
	jobs[0].Conclusion = "skipped" // build biegnie tylko na PR-ach i nocą
	return Evidence{
		Release: r,
		Main: []EvidenceRun{{
			Run:       ci.Run{ID: 7, Event: "push", Attempt: 1, Conclusion: "success", HeadSHA: r.Commit, URL: "https://github.com/o/r/actions/runs/7"},
			Jobs:      jobs,
			Artifacts: []EvidenceArtifact{{Name: "dowody-cluster", Digest: "sha256:" + strings.Repeat("d", 64), Size: 1024}},
		}},
		Tools: EvidenceTools{MiseLockSHA256: strings.Repeat("e", 64), DftRevision: strings.Repeat("a", 40), GoVersion: "go1.27.2"},
	}
}

func TestEvidenceCheck(t *testing.T) {
	if found := evidence().Check(); len(found) > 0 {
		t.Fatalf("pakiet z zielonym biegiem main odrzucony: %v", found)
	}
	for _, tt := range []struct {
		name   string
		change func(*Evidence)
		want   string
	}{
		{"bez biegu main", func(e *Evidence) { e.Main = nil }, "brak biegu ci po push na main"},
		{"bieg innego commita", func(e *Evidence) { e.Main[0].HeadSHA = strings.Repeat("f", 40) }, "dotyczy commita ffffffffffff"},
		{"czerwony bieg", func(e *Evidence) { e.Main[0].Conclusion = "failure" }, "runs/7: failure"},
		{"zielony bieg z pominiętym klastrem", func(e *Evidence) {
			for i := range e.Main[0].Jobs {
				if e.Main[0].Jobs[i].Name == "cluster" {
					e.Main[0].Jobs[i].Conclusion = "skipped"
				}
			}
		}, "zadanie cluster — skipped"},
		{"bieg bez zadania chart", func(e *Evidence) {
			e.Main[0].Jobs = e.Main[0].Jobs[:len(e.Main[0].Jobs)-1]
		}, "bez zadania chart"},
		{"wydanie bez commita kodu bramek", func(e *Evidence) { e.Release.GatesCommit = "" }, "commit kodu bramek"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := evidence()
			tt.change(&e)
			found := e.Check()
			if len(found) != 1 || !strings.Contains(found[0], tt.want) {
				t.Errorf("oczekiwano jednego naruszenia z %q, jest: %q", tt.want, found)
			}
		})
	}
}
