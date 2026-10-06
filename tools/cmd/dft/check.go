package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/gocheck"
	"github.com/mateuszf757/Docfind/tools/internal/versions"
)

const checkUsage = "dft check <versions|go|reproducible-tools>"

func runCheck(ctx context.Context, env *environment, args []string) error {
	if err := expectArgs(args, 1, 1, checkUsage); err != nil {
		return err
	}
	switch args[0] {
	case "versions":
		if err := requireTools("kubectl", "uv"); err != nil {
			return err
		}
		return versions.Check(ctx, env.root, env.runner, runtime.Version())
	case "go":
		if err := requireTools("go"); err != nil {
			return err
		}
		return goChecker(env).All(ctx)
	case "reproducible-tools":
		if err := requireTools("go"); err != nil {
			return err
		}
		return checkReproducibleTools(ctx, env)
	default:
		return cli.Usage("użycie: %s", checkUsage)
	}
}

func goChecker(env *environment) gocheck.Checker {
	return gocheck.Checker{
		ModuleDir: filepath.Join(env.root, "tools"),
		Runner:    env.runner,
		// GitHub Actions ustawia CI=true. Tam brak kompilatora C dla -race
		// to awaria, a nie po cichu pominięty detektor wyścigów.
		RequireRace: os.Getenv("CI") == "true",
		Out:         os.Stdout,
		Err:         os.Stderr,
	}
}

func checkReproducibleTools(ctx context.Context, env *environment) (err error) {
	tmp, err := os.MkdirTemp("", "dft-reproducible-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()

	binaries, err := goChecker(env).Reproducible(ctx, tmp)
	if len(binaries) > 0 {
		var table strings.Builder
		table.WriteString("### Powtarzalność narzędzi Go\n\n| Program | SHA-256 |\n|---|---|\n")
		for _, b := range binaries {
			cli.Step("%s  %s", b.SHA256, b.Name)
			fmt.Fprintf(&table, "| %s | `%s` |\n", b.Name, b.SHA256)
		}
		if summaryErr := appendSummary(table.String()); summaryErr != nil {
			return errors.Join(err, summaryErr)
		}
	}
	if err == nil {
		cli.Step("binarki powtarzalne: build z pamięcią podręczną i od zera dały te same bajty")
	}
	return err
}

// requireTools sprawdza wymagania na starcie i zgłasza wszystkie braki
// naraz. Brak narzędzia w połowie biegu wyglądałby jak błąd w kodzie,
// a nie w środowisku.
func requireTools(names ...string) error {
	var missing []string
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("brak w PATH: %s — uruchom przez ./bin/mise run …, które instaluje narzędzia z mise.lock", strings.Join(missing, ", "))
	}
	return nil
}
