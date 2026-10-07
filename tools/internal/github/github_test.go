package github

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
