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

// TestSecrets: kod 7 to sekrety (z raportu, bez wartości), inny niezerowy
// kod to awaria, a konfiguracja i wyjątki zawsze z ci/ — nigdy ze
// skanowanego katalogu — i bez zwolnień komentarzem gitleaks:allow.
func TestSecrets(t *testing.T) {
	oldOut := cli.Out
	cli.Out = io.Discard
	t.Cleanup(func() { cli.Out = oldOut })
	report := filepath.Join(t.TempDir(), "raporty", "gitleaks.json")
	leak := `[{"RuleID":"github-pat","File":"a.py","StartLine":3,"Commit":"abc","Fingerprint":"abc:a.py:github-pat:3","Secret":"REDACTED"}]`
	tests := []struct {
		name      string
		code      int
		wantCode  int
		wantLeaks int
	}{
		{"czysto", 0, cli.ExitOK, 0},
		{"sekret", 7, cli.ExitUnmet, 1},
		{"awaria", 1, cli.ExitFailure, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args []string
			runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
				args = c.Args
				if tt.code == 0 {
					return proc.Result{}, nil
				}
				if err := os.WriteFile(report, []byte(leak), 0o644); err != nil {
					t.Fatal(err)
				}
				return proc.Result{}, &proc.ExitError{Cmd: c.String(), Code: tt.code}
			})
			leaks, err := Secrets{Runner: runner, Root: "/repo", Report: report}.Check(context.Background())
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if len(leaks) != tt.wantLeaks || (tt.wantLeaks > 0 && leaks[0].Fingerprint != "abc:a.py:github-pat:3") {
				t.Errorf("ustalenia: %+v", leaks)
			}
			joined := strings.Join(args, " ")
			for _, want := range []string{"--config /repo/ci/gitleaks.toml", "--gitleaks-ignore-path /repo/ci/gitleaks-ignore", "--ignore-gitleaks-allow", "--redact", "--diff-merges=first-parent"} {
				if !strings.Contains(joined, want) {
					t.Errorf("gitleaks bez %q: %s", want, joined)
				}
			}
		})
	}
}

// TestSecretsFilesExist: pliki, które Check podaje gitleaks wprost, są
// w repozytorium — bez nich gitleaks kończy się awarią w każdym biegu.
func TestSecretsFilesExist(t *testing.T) {
	for _, f := range []string{GitleaksConfig, GitleaksIgnore} {
		if _, err := os.Stat(filepath.Join("..", "..", "..", "..", f)); err != nil {
			t.Error(err)
		}
	}
}
