package reposettings

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

const repoRoot = "../../../.."

// TestRepositoryEnvironments: środowiska GitHuba z repozytorium przechodzą
// walidację, a produkcja jest tylko z tagów wydań, z recenzentem.
func TestRepositoryEnvironments(t *testing.T) {
	envs, err := LoadEnvironments(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Environment{}
	for _, e := range envs {
		byName[e.Name] = e
	}
	prod, ok := byName["production"]
	if !ok || len(prod.Reviewers) == 0 || prod.PreventSelfReview || !slices.Equal(prod.Policies, []Policy{{Name: "v*", Type: "tag"}}) {
		t.Errorf("production: %+v — recenzent, bez prevent_self_review (jeden autor), tylko tagi v*", prod)
	}
	staging, ok := byName["staging"]
	if !ok || len(staging.Reviewers) != 0 || !slices.Equal(staging.Policies, []Policy{{Name: "main", Type: "branch"}}) {
		t.Errorf("staging: %+v — bez recenzenta, tylko main", staging)
	}
}

func TestLoadEnvironmentsRejects(t *testing.T) {
	valid := `{"name":"production","wait_timer":0,"prevent_self_review":false,"reviewers":["a"],` +
		`"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true},"policies":[{"name":"v*","type":"tag"}]}`
	tests := []struct{ name, file, content, want string }{
		{"literówka w polu", "production.json", strings.Replace(valid, `"reviewers"`, `"reviewer"`, 1), "unknown field"},
		{"nazwa inna niż plik", "prod.json", valid, "a plik nazywa się"},
		{"bez listy gałęzi", "production.json", strings.Replace(valid, `"custom_branch_policies":true`, `"custom_branch_policies":false`, 1), "z dowolnej gałęzi"},
		{"zły typ polityki", "production.json", strings.Replace(valid, `"type":"tag"`, `"type":"tagi"`, 1), "branch albo tag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, ".github", "environments")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, tt.file), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadEnvironments(root)
			if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("oczekiwano odrzucenia z %q, jest %v", tt.want, err)
			}
		})
	}
}

// TestLoadRulesets: reguły z repozytorium mają nazwy (po nich są
// aktualizowane), a plik bez nazwy jest odrzucany.
func TestLoadRulesets(t *testing.T) {
	rulesets, err := LoadRulesets(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rulesets, func(r Ruleset) bool { return r.Name != "" && strings.HasSuffix(r.File, "main.json") }) {
		t.Errorf("reguły: %+v — oczekiwano main.json z nazwą", rulesets)
	}
	root := t.TempDir()
	dir := filepath.Join(root, RulesetsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bez-nazwy.json"), []byte(`{"target":"branch"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRulesets(root); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("reguła bez nazwy: %v", err)
	}
}

// TestRequiredChecks: statusy wymagane przez reguły z repozytorium i z pliku
// z dwiema regułami, z których tylko jedna wymaga statusów.
func TestRequiredChecks(t *testing.T) {
	checks, err := RequiredChecks(repoRoot)
	if err != nil || len(checks) == 0 {
		t.Fatalf("statusy z repozytorium: %v, %v", checks, err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, RulesetsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ruleset := `{"name":"main","target":"branch","rules":[{"type":"deletion"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"fast-feedback","integration_id":15368},{"context":"integration"}]}}]}`
	if err := os.WriteFile(filepath.Join(dir, "main.json"), []byte(ruleset), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tags.json"), []byte(`{"name":"tagi","target":"tag","rules":[{"type":"update"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	checks, err = RequiredChecks(root)
	if err != nil || !slices.Equal(checks, []string{"fast-feedback", "integration"}) {
		t.Errorf("statusy: %v, %v", checks, err)
	}
}

func TestRulesDrift(t *testing.T) {
	want := []Rule{{Type: "deletion"}, {Type: "required_status_checks", Checks: []string{"fast-feedback", "integration"}}}
	for _, tt := range []struct {
		name string
		got  []Rule
		want []string
	}{
		{"zgodne, inna kolejność", []Rule{{Type: "required_status_checks", Checks: []string{"integration", "fast-feedback"}}, {Type: "deletion"}}, nil},
		{"plik niezastosowany", []Rule{{Type: "deletion"}, {Type: "required_status_checks", Checks: []string{"test", "build"}}}, []string{"wymagane statusy: w pliku [fast-feedback integration], w GitHubie [build test]"}},
		{"reguła usunięta w UI", []Rule{{Type: "required_status_checks", Checks: []string{"fast-feedback", "integration"}}}, []string{"reguła deletion z pliku nie działa w GitHubie"}},
		{"reguła dodana w UI", append([]Rule{{Type: "update"}}, want...), []string{"reguła update działa w GitHubie, a nie ma jej w .github/rulesets"}},
	} {
		got := RulesDrift(want, tt.got)
		if len(got) != len(tt.want) {
			t.Errorf("%s: %q", tt.name, got)
			continue
		}
		for i := range got {
			if !strings.HasPrefix(got[i], tt.want[i]) {
				t.Errorf("%s: %q, oczekiwano %q", tt.name, got[i], tt.want[i])
			}
		}
	}
}
