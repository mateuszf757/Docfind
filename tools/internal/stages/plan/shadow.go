package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
)

// Artifact — nazwa artefaktu z raportem planu w biegu PR-a.
const Artifact = "plan-zmiany"

// Runs — biegi PR-ów, ich zadania i artefakty (adapters/github).
type Runs interface {
	RunsByEvent(ctx context.Context, workflow, event string, limit int) ([]ci.Run, error)
	Jobs(ctx context.Context, runID int64) ([]ci.Job, error)
	RunArtifacts(ctx context.Context, runID int64) ([]artifact.EvidenceArtifact, error)
	DownloadArtifact(ctx context.Context, runID int64, name, dir string) error
}

// ShadowResult — tryb cienia planu (decyzja 77): profile policzone na PR-ach
// i rozjazdy — profil fast albo medium przy czerwonym cluster, czyli błąd,
// który klaster złapał, a plan by przepuścił.
type ShadowResult struct {
	Runs       int                `json:"runs"`
	ByProfile  map[ci.Profile]int `json:"by_profile"`
	Mismatches []string           `json:"mismatches"`
}

// Ready — plan można włączyć (krok 9): dość biegów z planem i żadnego
// rozjazdu.
func (r ShadowResult) Ready() bool {
	return r.Runs >= contract.PlanShadowRuns && len(r.Mismatches) == 0
}

// Shadow przegląda ostatnie biegi ci na PR-ach (limit): profil z artefaktu
// planu, wynik zadania cluster z API. Bieg bez artefaktu (sprzed planu albo
// po retencji) się nie liczy.
func Shadow(ctx context.Context, r Runs, tmp string, limit int) (ShadowResult, error) {
	res := ShadowResult{ByProfile: map[ci.Profile]int{}, Mismatches: []string{}}
	runs, err := r.RunsByEvent(ctx, "ci.yml", "pull_request", limit)
	if err != nil {
		return res, err
	}
	for _, run := range runs {
		arts, err := r.RunArtifacts(ctx, run.ID)
		if err != nil {
			return res, err
		}
		if !slices.ContainsFunc(arts, func(a artifact.EvidenceArtifact) bool { return a.Name == Artifact && !a.Expired }) {
			continue
		}
		dir := filepath.Join(tmp, strconv.FormatInt(run.ID, 10))
		if err := r.DownloadArtifact(ctx, run.ID, Artifact, dir); err != nil {
			return res, err
		}
		raw, err := os.ReadFile(filepath.Join(dir, "plan.json"))
		if err != nil {
			return res, fmt.Errorf("bieg %d: %w", run.ID, err)
		}
		var p ci.Plan
		if err := json.Unmarshal(raw, &p); err != nil {
			return res, fmt.Errorf("bieg %d: plan.json: %w", run.ID, err)
		}
		jobs, err := r.Jobs(ctx, run.ID)
		if err != nil {
			return res, err
		}
		res.Runs++
		res.ByProfile[p.Profile]++
		i := slices.IndexFunc(jobs, func(j ci.Job) bool { return j.Name == "cluster" })
		if p.Profile != ci.ProfileHeavy && i >= 0 && jobs[i].Conclusion == "failure" {
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("%s: profil %s, a cluster czerwony", run.URL, p.Profile))
		}
	}
	return res, nil
}
