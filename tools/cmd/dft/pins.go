package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/pins"
)

// runPins podaje workflowom przypięcia, których potrzebują, zanim cokolwiek
// zbudują: wersję uv dla setup-uv i obraz BuildKitu dla setup-buildx-action.
// Wcześniej krok workflowu źródłował ci/lib.sh w `bash -c` — kod Basha
// wykonywany po to, żeby przeczytać daną.
func runPins(_ context.Context, env *environment, args []string) error {
	const usage = "dft pins github-output"
	if err := expectArgs(args, 1, 1, usage); err != nil {
		return err
	}
	if args[0] != "github-output" {
		return cli.Usage("użycie: %s", usage)
	}
	p, err := pins.Load(env.root)
	if err != nil {
		return err
	}
	dockerfile, err := os.ReadFile(filepath.Join(env.root, pins.Dockerfile))
	if err != nil {
		return err
	}
	uv, err := pins.UVVersion(dockerfile)
	if err != nil {
		return err
	}
	lines := []string{
		"uv=" + uv,
		"buildkit=" + p.Get("DF_BUILDKIT_IMAGE"),
	}

	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		_, err := fmt.Fprintln(cli.Out, strings.Join(lines, "\n"))
		return err
	}
	if err := appendFile(path, strings.Join(lines, "\n")+"\n"); err != nil {
		return fmt.Errorf("GITHUB_OUTPUT: %w", err)
	}
	cli.Step("przypięcia dla workflowu: %s", strings.Join(lines, ", "))
	return nil
}

// appendFile dopisuje do pliku, jak `>>` w powłoce. Błąd zamknięcia też
// jest błędem: przy zapisie buforowanym dopiero Close mówi, czy dane
// trafiły na dysk.
func appendFile(path, content string) (err error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	_, err = f.WriteString(content)
	return err
}

// appendSummary dopisuje Markdown do podsumowania zadania w GitHub Actions;
// poza CI nic nie robi.
func appendSummary(markdown string) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	return appendFile(path, markdown)
}
