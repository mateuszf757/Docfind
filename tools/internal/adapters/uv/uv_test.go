package uv

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

func TestCheck(t *testing.T) {
	quiet(t)
	var calls []string
	runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
		calls = append(calls, c.String())
		if strings.Contains(c.String(), "mypy") {
			return proc.Result{}, &proc.ExitError{Cmd: c.String(), Code: 1}
		}
		if c.Dir != "services/api" || len(c.Env) != 1 || c.Env[0] != "PYTHONPATH=src" {
			t.Errorf("%s: katalog %q, środowisko %v", c, c.Dir, c.Env)
		}
		return proc.Result{}, nil
	})
	err := Python{Runner: runner, Dir: "services/api"}.Check(context.Background(), "../../deploy/config/app.schema.json")
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "uv run --frozen mypy") {
		t.Errorf("mypy z uwagami: %v", err)
	}
	if len(calls) != 4 {
		t.Errorf("po porażce dalsze kroki: %v", calls)
	}
}

func TestCheckConfigAndVersion(t *testing.T) {
	cmd := "uv run --frozen python -m docfind_api.configtool check /tmp/app.yml"
	ctx := context.Background()
	for code, want := range map[int]int{0: cli.ExitOK, 1: cli.ExitUnmet, 2: cli.ExitFailure} {
		f := &proc.Fake{Responses: map[string]proc.FakeResponse{cmd: {Code: code}}}
		if got := cli.ExitCode(Python{Runner: f}.CheckConfig(ctx, "/tmp/app.yml")); got != want {
			t.Errorf("kod configtool %d: %d, oczekiwano %d", code, got, want)
		}
	}
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{"uv --version": {Stdout: "uv 0.11.3 (abc 2026-10-01)\n"}}}
	if v, err := (UV{Runner: f}).Version(ctx); err != nil || v != "0.11.3" {
		t.Errorf("wersja uv: %q, %v", v, err)
	}
}
