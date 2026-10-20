package plan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
)

// prRuns — biegi PR-ów w pamięci: profil planu (pusty — bez artefaktu)
// i wynik cluster.
type prRuns struct {
	runs []ci.Run
	plan map[int64]string
	// expired — artefakt po retencji.
	expired map[int64]bool
	cluster map[int64]string
}

func (r prRuns) RunsByEvent(_ context.Context, workflow, event string, _ int) ([]ci.Run, error) {
	if workflow != "ci.yml" || event != "pull_request" {
		return nil, fmt.Errorf("zapytanie %s %s", workflow, event)
	}
	return r.runs, nil
}

func (r prRuns) Jobs(_ context.Context, id int64) ([]ci.Job, error) {
	return []ci.Job{{Name: "test", Conclusion: "success"}, {Name: "cluster", Conclusion: r.cluster[id]}}, nil
}

func (r prRuns) RunArtifacts(_ context.Context, id int64) ([]artifact.EvidenceArtifact, error) {
	if r.plan[id] == "" {
		return []artifact.EvidenceArtifact{{Name: "dowody-cluster"}}, nil
	}
	return []artifact.EvidenceArtifact{{Name: Artifact, Expired: r.expired[id]}}, nil
}

func (r prRuns) DownloadArtifact(_ context.Context, id int64, name, dir string) error {
	if name != Artifact || r.expired[id] {
		return fmt.Errorf("artefakt %s", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "plan.json"), []byte(`{"profile":"`+r.plan[id]+`","files":{}}`), 0o644)
}

func TestShadow(t *testing.T) {
	r := prRuns{
		plan:    map[int64]string{1: "heavy", 2: "medium", 3: "fast", 5: "fast"},
		expired: map[int64]bool{5: true},
		cluster: map[int64]string{1: "failure", 2: "failure", 3: "success", 4: "failure", 5: "failure"},
	}
	for id := int64(1); id <= 5; id++ {
		r.runs = append(r.runs, ci.Run{ID: id, URL: fmt.Sprintf("https://x/runs/%d", id)})
	}
	res, err := Shadow(context.Background(), r, t.TempDir(), 50)
	if err != nil {
		t.Fatal(err)
	}
	// Bieg 4 bez planu i 5 z artefaktem po retencji się nie liczą; heavy
	// przy czerwonym klastrze to nie rozjazd.
	if res.Runs != 3 || res.ByProfile[ci.ProfileHeavy] != 1 || res.ByProfile[ci.ProfileFast] != 1 {
		t.Errorf("biegi: %+v", res)
	}
	if len(res.Mismatches) != 1 || !strings.Contains(res.Mismatches[0], "runs/2: profil medium, a cluster czerwony") {
		t.Errorf("rozjazdy: %q", res.Mismatches)
	}
	if res.Ready() {
		t.Error("gotowe przy rozjeździe i 3 biegach")
	}
}

func TestShadowReady(t *testing.T) {
	r := prRuns{plan: map[int64]string{}, cluster: map[int64]string{}}
	for id := int64(1); id <= 20; id++ {
		r.runs = append(r.runs, ci.Run{ID: id})
		r.plan[id], r.cluster[id] = "fast", "success"
	}
	res, err := Shadow(context.Background(), r, t.TempDir(), 50)
	if err != nil || !res.Ready() {
		t.Errorf("20 biegów bez rozjazdu: %+v, %v", res, err)
	}
}
