package lint

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

func TestCheck(t *testing.T) {
	oldOut := cli.Out
	cli.Out = io.Discard
	t.Cleanup(func() { cli.Out = oldOut })
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ci"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ci", "a.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls []string
	runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
		calls = append(calls, c.Name+" "+strings.Join(c.Args, " "))
		if c.Name == "zizmor" {
			return proc.Result{}, &proc.ExitError{Cmd: c.String(), Code: 13}
		}
		return proc.Result{}, nil
	})
	err := Workflows{Runner: runner, Root: root}.Check(context.Background())
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "zizmor --offline: kod 13") {
		t.Errorf("zizmor z uwagami: %v", err)
	}
	want := []string{
		"shellcheck " + filepath.Join(root, "ci", "a.sh") + " " + filepath.Join(root, "ci", "dft"),
		"actionlint -no-color",
		"zizmor --offline --no-progress .github/",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("wywołania:\n%s", strings.Join(calls, "\n"))
	}
}
