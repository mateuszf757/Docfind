package ci

import (
	"slices"
	"testing"
)

var clusterDirs = map[string]bool{"tools/internal/stages/drain": true, "tools/internal/core/contract": true}

func TestClassify(t *testing.T) {
	for file, want := range map[string]Profile{
		"docs/DECYZJE.md":                              ProfileFast,
		"README.md":                                    ProfileFast,
		".github/rulesets/main.json":                   ProfileFast,
		"ci/vuln-exceptions.yaml":                      ProfileFast,
		"ci/images/Dockerfile":                         ProfileFast,
		"services/api/uv.lock":                         ProfileMedium,
		"services/api/Dockerfile":                      ProfileMedium,
		"deploy/config/app.yml.example":                ProfileMedium,
		"deploy/charts/docfind/values.yaml":            ProfileHeavy,
		"deploy/platform/openbao/values.yaml":          ProfileHeavy,
		".github/workflows/ci.yml":                     ProfileHeavy,
		".github/actions/build-image/action.yml":       ProfileHeavy,
		"ci/pins.env":                                  ProfileHeavy,
		"mise.toml":                                    ProfileHeavy,
		"mise.staging.toml":                            ProfileHeavy,
		"tools/go.sum":                                 ProfileHeavy,
		"tools/cmd/drain-probe/main.go":                ProfileHeavy,
		"tools/cmd/dft/cluster.go":                     ProfileHeavy,
		"tools/cmd/dft/quality.go":                     ProfileHeavy,
		"tools/cmd/dft/release.go":                     ProfileFast,
		"tools/cmd/dft/main_test.go":                   ProfileFast,
		"tools/internal/stages/drain/drain.go":         ProfileHeavy,
		"tools/internal/stages/drain/drain_test.go":    ProfileFast,
		"tools/internal/core/contract/thresholds.go":   ProfileHeavy,
		"tools/internal/stages/budget/budget.go":       ProfileFast,
		"tools/internal/stages/charts/testdata/x.yaml": ProfileFast,
		// Nieznane ścieżki — heavy.
		"Makefile":                    ProfileHeavy,
		"tools/README":                ProfileHeavy,
		"tools/internal/nowa_warstwa": ProfileHeavy,
	} {
		if got := Classify(file, clusterDirs); got != want {
			t.Errorf("Classify(%q) = %s, oczekiwano %s", file, got, want)
		}
	}
}

func TestPlanFor(t *testing.T) {
	p := PlanFor([]string{"docs/RUNBOOK.md", "services/api/uv.lock"}, clusterDirs)
	if p.Profile != ProfileMedium || !slices.Equal(p.Files[ProfileFast], []string{"docs/RUNBOOK.md"}) {
		t.Errorf("zależność aplikacji: %+v", p)
	}
	if p := PlanFor([]string{"services/api/uv.lock", "deploy/charts/docfind/values.yaml", "docs/x.md"}, clusterDirs); p.Profile != ProfileHeavy {
		t.Errorf("chart: %+v", p)
	}
	if p := PlanFor(nil, clusterDirs); p.Profile != ProfileFast {
		t.Errorf("pusta zmiana: %+v", p)
	}
}
