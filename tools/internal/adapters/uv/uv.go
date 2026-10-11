// Package uv to adapter uv: środowisko Pythona aplikacji z uv.lock, jego
// narzędzia jakości (ruff, mypy, pytest) i walidacja konfiguracji modelem
// aplikacji (docfind_api.configtool).
package uv

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Python — projekt Pythona w Dir (services/api).
type Python struct {
	Runner proc.Runner
	Dir    string
	// Out i Err — wyjście narzędzi na bieżąco.
	Out, Err io.Writer
}

// PYTHONPATH=src, bo projekt nie jest instalowany do środowiska — jak
// w obrazie.
var pythonPath = []string{"PYTHONPATH=src"}

func (p Python) run(ctx context.Context, args ...string) error {
	cli.Step("uv %s", args[0])
	_, err := p.Runner.Run(ctx, proc.Cmd{Name: "uv", Args: args, Dir: p.Dir, Env: pythonPath, Stdout: p.Out, Stderr: p.Err})
	if code := proc.ExitCode(err); code > 0 {
		return cli.Unmet("uv %s: kod %d", strings.Join(args[:min(3, len(args))], " "), code)
	}
	return err
}

// Check sprawdza projekt: zależności z locka (--locked kończy się błędem,
// gdy lock nie odpowiada pyproject.toml), lint, format, typy, testy
// i schemat konfiguracji (schema) generowany z modelu.
func (p Python) Check(ctx context.Context, schema string) error {
	for _, step := range [][]string{
		{"sync", "--locked"},
		{"run", "--frozen", "ruff", "check", "."},
		{"run", "--frozen", "ruff", "format", "--check", "."},
		// Adnotacje typów bez sprawdzania to dokumentacja, która może kłamać.
		{"run", "--frozen", "mypy", "src", "tests"},
		{"run", "--frozen", "pytest", "-q"},
		{"run", "--frozen", "python", "-m", "docfind_api.configtool", "schema", "--check", schema},
	} {
		if err := p.run(ctx, step...); err != nil {
			return err
		}
	}
	return nil
}

// CheckConfig sprawdza plik app.yml modelem aplikacji. Kod 1 to
// konfiguracja nieprawidłowa (warunek niespełniony, pole i powód na
// stderr), wyższy — walidacja się nie wykonała.
func (p Python) CheckConfig(ctx context.Context, path string) error {
	_, err := p.Runner.Run(ctx, proc.Cmd{
		Name: "uv", Args: []string{"run", "--frozen", "python", "-m", "docfind_api.configtool", "check", path},
		Dir: p.Dir, Env: pythonPath, Stdout: p.Out, Stderr: p.Err,
	})
	switch code := proc.ExitCode(err); {
	case code == 1:
		return cli.Unmet("konfiguracja %s jest nieprawidłowa — wyżej pole i powód", path)
	case code > 1:
		return fmt.Errorf("walidacja konfiguracji nie wykonała się: %v", err)
	}
	return err
}

// UV — program uv.
type UV struct {
	Runner proc.Runner
}

// Version zwraca wersję uv (np. 0.11.3).
func (u UV) Version(ctx context.Context) (string, error) {
	out, err := proc.Output(ctx, u.Runner, proc.Cmd{Name: "uv", Args: []string{"--version"}})
	if err != nil {
		return "", fmt.Errorf("uv --version: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return "", fmt.Errorf("uv --version: nieczytelne %q", out)
	}
	return fields[1], nil
}
