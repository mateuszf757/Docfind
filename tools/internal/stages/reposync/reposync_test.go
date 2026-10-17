package reposync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/reposettings"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// github — repozytorium w pamięci; zapisuje zmiany.
type github struct {
	changes  []string
	policies map[reposettings.Policy]int64
	// noEnvironment — środowiska jeszcze nie ma (odczyt polityk zawodzi).
	noEnvironment bool
	// rules, approval — stan w GitHubie dla Check.
	rules    []reposettings.Rule
	approval bool
}

func (g *github) BranchRules(context.Context, string, string) ([]reposettings.Rule, error) {
	return g.rules, nil
}
func (g *github) RequiresApproval(context.Context, string, string) (bool, error) {
	return g.approval, nil
}

func (g *github) change(format string, args ...any) error {
	g.changes = append(g.changes, fmt.Sprintf(format, args...))
	return nil
}

func (g *github) Repo(context.Context) (string, error)          { return "o/r", nil }
func (g *github) EnableAutoMerge(context.Context, string) error { return g.change("auto-merge") }
func (g *github) EnableSecretScanning(context.Context, string) error {
	return g.change("secret scanning")
}
func (g *github) ActionsPermissions(context.Context, string) (bool, string, error) {
	return true, "all", nil
}
func (g *github) RequireSHAPinning(_ context.Context, _ string, enabled bool, allowed string) error {
	return g.change("sha %v %s", enabled, allowed)
}
func (g *github) Rulesets(context.Context, string) (map[string]int64, error) {
	return map[string]int64{"main": 7}, nil
}
func (g *github) CreateRuleset(_ context.Context, _, file string) error {
	return g.change("create %s", filepath.Base(file))
}
func (g *github) UpdateRuleset(_ context.Context, _ string, id int64, file string) error {
	return g.change("update %d %s", id, filepath.Base(file))
}
func (g *github) UserID(_ context.Context, login string) (int64, error) {
	if login != "autor" {
		return 0, errors.New("nieznany użytkownik")
	}
	return 4242, nil
}
func (g *github) PutEnvironment(_ context.Context, _ string, e reposettings.Environment, ids []int64) error {
	return g.change("environment %s %v", e.Name, ids)
}
func (g *github) BranchPolicies(context.Context, string, string) (map[reposettings.Policy]int64, error) {
	if g.noEnvironment {
		return nil, errors.New("404")
	}
	return g.policies, nil
}
func (g *github) AddBranchPolicy(_ context.Context, _, env string, p reposettings.Policy) error {
	return g.change("add %s %s %s", env, p.Type, p.Name)
}
func (g *github) DeleteBranchPolicy(_ context.Context, _, env string, id int64) error {
	return g.change("delete %s %d", env, id)
}

func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		".github/rulesets/main.json": `{"name":"main","target":"branch","rules":[{"type":"deletion"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"fast-feedback"},{"context":"integration"}]}}]}`,
		".github/rulesets/tags.json": `{"name":"tagi wydań","target":"tag","rules":[{"type":"update"}]}`,
		".github/environments/production.json": `{"name":"production","wait_timer":0,"prevent_self_review":false,"reviewers":["autor"],` +
			`"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true},"policies":[{"name":"v*","type":"tag"}]}`,
	}
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestApply(t *testing.T) {
	var out bytes.Buffer
	oldOut := cli.Out
	cli.Out = &out
	defer func() { cli.Out = oldOut }()

	g := &github{policies: map[reposettings.Policy]int64{{Name: "release/*", Type: "branch"}: 3}}
	if err := (Settings{Root: repo(t), GitHub: g}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"auto-merge", "secret scanning", "sha true all",
		"update 7 main.json", "create tags.json",
		"environment production [4242]",
		"add production tag v*", "delete production 3",
	}
	if !slices.Equal(g.changes, want) {
		t.Errorf("zmiany:\n%s\noczekiwano:\n%s", strings.Join(g.changes, "\n"), strings.Join(want, "\n"))
	}
	for _, msg := range []string{"reguła 'main' zaktualizowana (id 7)", "reguła 'tagi wydań' utworzona", "allowed_actions=all"} {
		if !strings.Contains(out.String(), msg) {
			t.Errorf("brak %q w:\n%s", msg, out.String())
		}
	}

	// Tryb próbny: żadnej zmiany, także gdy środowiska jeszcze nie ma.
	g = &github{noEnvironment: true}
	if err := (Settings{Root: repo(t), GitHub: g, DryRun: true}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.changes) != 0 {
		t.Errorf("tryb próbny zmienił: %v", g.changes)
	}
}

func TestCheck(t *testing.T) {
	var out bytes.Buffer
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = &out, &out
	defer func() { cli.Out, cli.Err = oldOut, oldErr }()
	inSync := func() *github {
		return &github{
			rules:    []reposettings.Rule{{Type: "deletion"}, {Type: "required_status_checks", Checks: []string{"integration", "fast-feedback"}}},
			approval: true,
			policies: map[reposettings.Policy]int64{{Name: "v*", Type: "tag"}: 1},
		}
	}
	g := inSync()
	if err := (Settings{Root: repo(t), GitHub: g}).Check(context.Background()); err != nil {
		t.Fatalf("zgodne ustawienia: %v", err)
	}
	for name, change := range map[string]func(*github){
		"plik niezastosowany":          func(g *github) { g.rules[1].Checks = []string{"test", "build"} },
		"zgoda zdjęta w UI":            func(g *github) { g.approval = false },
		"dodana gałąź w UI":            func(g *github) { g.policies[reposettings.Policy{Name: "main", Type: "branch"}] = 2 },
		"reguła usunięcia zdjęta w UI": func(g *github) { g.rules = g.rules[1:] },
	} {
		g := inSync()
		change(g)
		err := (Settings{Root: repo(t), GitHub: g}).Check(context.Background())
		if cli.ExitCode(err) != cli.ExitUnmet {
			t.Errorf("%s: %v", name, err)
		}
		if len(g.changes) != 0 {
			t.Errorf("%s: sprawdzenie zmieniło %v", name, g.changes)
		}
	}
}
