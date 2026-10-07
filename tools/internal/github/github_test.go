package github

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

func TestApply(t *testing.T) {
	var out bytes.Buffer
	oldOut := cli.Out
	cli.Out = &out
	defer func() { cli.Out = oldOut }()

	root := t.TempDir()
	dir := filepath.Join(root, ".github", "rulesets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"main.json": `{"name":"main"}`, "tags.json": `{"name":"tagi wydań"}`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo := "o/r"
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"gh auth status": {},
		"gh repo view --json nameWithOwner --jq .nameWithOwner":                                                                          {Stdout: repo + "\n"},
		"gh api --method PATCH repos/o/r -F allow_auto_merge=true --silent":                                                              {},
		"gh api repos/o/r/actions/permissions":                                                                                           {Stdout: `{"enabled":true,"allowed_actions":"all"}`},
		"gh api --method PUT repos/o/r/actions/permissions -F enabled=true -f allowed_actions=all -F sha_pinning_required=true --silent": {},
		"gh api repos/o/r/rulesets":                                                                                                      {Stdout: `[{"id":7,"name":"main"}]`},
		"gh api --method PUT repos/o/r/rulesets/7 --input " + filepath.Join(dir, "main.json") + " --silent":                              {},
		"gh api --method POST repos/o/r/rulesets --input " + filepath.Join(dir, "tags.json") + " --silent":                               {},
	}}
	if err := (Settings{Root: root, Runner: f}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reguła 'main' zaktualizowana (id 7)", "reguła 'tagi wydań' utworzona", "allowed_actions=all"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("brak %q w:\n%s", want, out.String())
		}
	}

	// Tryb próbny: żadnej zmiany, tylko odczyty.
	f.Calls = nil
	if err := (Settings{Root: root, Runner: f, DryRun: true}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls {
		if strings.Contains(c.String(), "--method") {
			t.Errorf("tryb próbny wykonał zmianę: %s", c.String())
		}
	}
}

// TestRepositoryEnvironments: środowiska GitHuba z repozytorium przechodzą
// walidację, a produkcja jest tylko z tagów wydań, z recenzentem.
func TestRepositoryEnvironments(t *testing.T) {
	envs, err := LoadEnvironments("../../..")
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

// TestApplyEnvironments: recenzent po identyfikatorze z API, ciało PUT
// z pliku, polityki doprowadzone do listy z pliku (brakująca dodana,
// nadmiarowa usunięta).
func TestApplyEnvironments(t *testing.T) {
	var out bytes.Buffer
	oldOut := cli.Out
	cli.Out = &out
	defer func() { cli.Out = oldOut }()

	root := t.TempDir()
	dir := filepath.Join(root, ".github", "environments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	prod := `{"name":"production","wait_timer":0,"prevent_self_review":false,"reviewers":["autor"],` +
		`"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true},"policies":[{"name":"v*","type":"tag"}]}`
	if err := os.WriteFile(filepath.Join(dir, "production.json"), []byte(prod), 0o644); err != nil {
		t.Fatal(err)
	}
	var calls []string
	var putBody string
	runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
		cmd := c.String()
		calls = append(calls, cmd)
		switch {
		case cmd == "gh api users/autor --jq .id":
			return proc.Result{Stdout: []byte("4242\n")}, nil
		case strings.HasPrefix(cmd, "gh api --method PUT repos/o/r/environments/production --input "):
			raw, err := os.ReadFile(c.Args[5])
			putBody = string(raw)
			return proc.Result{}, err
		case cmd == "gh api repos/o/r/environments/production/deployment-branch-policies":
			return proc.Result{Stdout: []byte(`{"total_count":1,"branch_policies":[{"id":3,"name":"release/*","type":"branch"}]}`)}, nil
		}
		return proc.Result{}, nil
	})
	if err := (Settings{Root: root, Runner: runner}).environments(context.Background(), "o/r"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(putBody, `"reviewers":[{"type":"User","id":4242}]`) || !strings.Contains(putBody, `"prevent_self_review":false`) {
		t.Errorf("ciało PUT: %s", putBody)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"gh api --method POST repos/o/r/environments/production/deployment-branch-policies -f name=v* -f type=tag --silent",
		"gh api --method DELETE repos/o/r/environments/production/deployment-branch-policies/3 --silent",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("brak wywołania %q w:\n%s", want, joined)
		}
	}
}

func TestRequireApproval(t *testing.T) {
	const cmd = "gh api repos/o/r/environments/production"
	tests := []struct {
		name, body string
		wantCode   int
	}{
		{"zgoda wymagana", `{"name":"production","protection_rules":[{"type":"branch_policy"},{"type":"required_reviewers","prevent_self_review":false,"reviewers":[{"type":"User","reviewer":{"login":"autor"}}]}]}`, cli.ExitOK},
		// Tak wygląda środowisko utworzone przez GitHuba przy pierwszym użyciu.
		{"środowisko bez reguł", `{"name":"production","protection_rules":[]}`, cli.ExitUnmet},
		{"reguła bez recenzentów", `{"protection_rules":[{"type":"required_reviewers","reviewers":[]}]}`, cli.ExitUnmet},
		{"nieczytelna odpowiedź", `<html>`, cli.ExitFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &proc.Fake{Responses: map[string]proc.FakeResponse{cmd: {Stdout: tt.body}}}
			if code := cli.ExitCode(RequireApproval(context.Background(), f, "o/r", "production")); code != tt.wantCode {
				t.Errorf("kod %d, oczekiwano %d", code, tt.wantCode)
			}
		})
	}
}
