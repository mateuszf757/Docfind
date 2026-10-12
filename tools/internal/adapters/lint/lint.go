// Package lint to adapter linterów repozytorium: skrypty powłoki
// shellcheckiem, workflowy actionlintem (poprawność) i zizmorem (czy
// poprawny workflow nie jest groźny), historia gita gitleaksem (sekrety).
// Narzędzia w wersjach z mise.lock — różne wersje zgłaszają różne uwagi.
package lint

import (
	"context"
	"io"
	"path/filepath"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Workflows — skrypty ci/ i workflowy .github/ repozytorium w Root.
type Workflows struct {
	Runner proc.Runner
	Root   string
	// Out i Err — wyjście linterów na bieżąco.
	Out, Err io.Writer
}

func (w Workflows) run(ctx context.Context, name string, args ...string) error {
	cli.Step("%s %s", name, args[0])
	_, err := w.Runner.Run(ctx, proc.Cmd{Name: name, Args: args, Dir: w.Root, Stdout: w.Out, Stderr: w.Err})
	if code := proc.ExitCode(err); code > 0 {
		return cli.Unmet("%s %s: kod %d", name, args[0], code)
	}
	return err
}

// Check uruchamia wszystkie trzy i kończy się na pierwszym z uwagami.
func (w Workflows) Check(ctx context.Context) error {
	scripts, err := filepath.Glob(filepath.Join(w.Root, "ci", "*.sh"))
	if err != nil {
		return err
	}
	scripts = append(scripts, filepath.Join(w.Root, "ci", "dft"))
	if err := w.run(ctx, "shellcheck", scripts...); err != nil {
		return err
	}
	if err := w.run(ctx, "actionlint", "-no-color"); err != nil {
		return err
	}
	// --offline: bez tokenu i bez sieci, te same wyniki lokalnie i w CI.
	return w.run(ctx, "zizmor", "--offline", "--no-progress", ".github/")
}
