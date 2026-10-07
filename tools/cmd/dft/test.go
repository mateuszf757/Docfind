package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"github.com/mateuszf757/Docfind/tools/internal/charts"
	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/pins"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
	"github.com/mateuszf757/Docfind/tools/internal/versions"
)

// runTest to zadanie test z CI: wersje, workflowy i skrypty, Python
// aplikacji, charty, kod narzędzi. Kończy się na pierwszej grupie, która nie
// przeszła — każda zakłada, że poprzednie przeszły (dawniej ci/run-tests.sh).
func runTest(ctx context.Context, e *environment, args []string) error {
	if err := expectArgs(args, 0, 0, "dft test"); err != nil {
		return err
	}
	// Wymagania na starcie i zgłaszane razem: brak narzędzia w połowie biegu
	// wyglądałby jak błąd w kodzie, a nie w środowisku.
	if err := requireTools("uv", "shellcheck", "actionlint", "zizmor", "helm", "kubeconform", "kubectl", "go"); err != nil {
		return err
	}
	steps := []struct {
		name string
		run  func() error
	}{
		{"spójność wersji", func() error { return versions.Check(ctx, e.root, e.runner, runtime.Version(), clientGoVersion()) }},
		{"workflowy i skrypty", func() error { return checkWorkflows(ctx, e) }},
		{"Python aplikacji", func() error { return checkPython(ctx, e) }},
		{"charty", func() error { return checkCharts(ctx, e) }},
		{"kod Go", func() error {
			c, err := goChecker(e)
			if err != nil {
				return err
			}
			return c.All(ctx)
		}},
	}
	for _, step := range steps {
		cli.Step("— %s —", step.name)
		if err := step.run(); err != nil {
			return err
		}
	}
	cli.Step("OK")
	return nil
}

// tool uruchamia program z wyjściem na bieżąco; niezerowy kod to warunek
// niespełniony (kod nie przechodzi bramki), brak programu — awaria.
func tool(ctx context.Context, e *environment, dir string, env []string, name string, args ...string) error {
	cli.Step("%s %s", name, args[0])
	_, err := e.runner.Run(ctx, proc.Cmd{Name: name, Args: args, Dir: dir, Env: env, Stdout: os.Stdout, Stderr: os.Stderr})
	if code := proc.ExitCode(err); code > 0 {
		return cli.Unmet("%s %s: kod %d", name, args[0], code)
	}
	return err
}

// checkWorkflows: skrypty powłoki shellcheckiem, workflowy actionlintem
// (poprawność) i zizmorem (czy poprawny workflow nie jest groźny).
// Narzędzia w wersjach z mise.lock — różne wersje zgłaszają różne uwagi.
func checkWorkflows(ctx context.Context, e *environment) error {
	scripts, err := filepath.Glob(filepath.Join(e.root, "ci", "*.sh"))
	if err != nil {
		return err
	}
	scripts = append(scripts, filepath.Join(e.root, "ci", "dft"))
	if err := tool(ctx, e, e.root, nil, "shellcheck", scripts...); err != nil {
		return err
	}
	if err := tool(ctx, e, e.root, nil, "actionlint", "-no-color"); err != nil {
		return err
	}
	// --offline: bez tokenu i bez sieci, te same wyniki lokalnie i w CI.
	return tool(ctx, e, e.root, nil, "zizmor", "--offline", "--no-progress", ".github/")
}

// checkPython: zależności z locka (--locked kończy się błędem, gdy lock nie
// odpowiada pyproject.toml), lint, format, typy, testy i schemat konfiguracji
// generowany z modelu.
func checkPython(ctx context.Context, e *environment) error {
	dir := filepath.Join(e.root, "services", "api")
	pythonPath := []string{"PYTHONPATH=src"}
	for _, step := range [][]string{
		{"sync", "--locked"},
		{"run", "--frozen", "ruff", "check", "."},
		{"run", "--frozen", "ruff", "format", "--check", "."},
		// Adnotacje typów bez sprawdzania to dokumentacja, która może kłamać.
		{"run", "--frozen", "mypy", "src", "tests"},
		{"run", "--frozen", "pytest", "-q"},
		{"run", "--frozen", "python", "-m", "docfind_api.configtool", "schema", "--check", "../../deploy/config/app.schema.json"},
	} {
		if err := tool(ctx, e, dir, pythonPath, "uv", step...); err != nil {
			return err
		}
	}
	return nil
}

func checkCharts(ctx context.Context, e *environment) (err error) {
	p, err := pins.Load(e.root)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "dft-charts-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
	return charts.Checker{Root: e.root, Runner: e.runner, Pins: p, Out: os.Stdout, Err: os.Stderr}.Run(ctx, tmp)
}

func runCheckWorkflows(ctx context.Context, e *environment, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check workflows"); err != nil {
		return err
	}
	if err := requireTools("shellcheck", "actionlint", "zizmor"); err != nil {
		return err
	}
	return checkWorkflows(ctx, e)
}

func runCheckPython(ctx context.Context, e *environment, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check python"); err != nil {
		return err
	}
	if err := requireTools("uv"); err != nil {
		return err
	}
	return checkPython(ctx, e)
}

func runCheckCharts(ctx context.Context, e *environment, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check charts"); err != nil {
		return err
	}
	if err := requireTools("helm", "kubeconform", "uv"); err != nil {
		return err
	}
	return checkCharts(ctx, e)
}
