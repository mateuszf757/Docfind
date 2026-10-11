package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// runPins podaje workflowom przypięcia, których potrzebują, zanim cokolwiek
// zbudują: wersję uv dla setup-uv i obraz BuildKitu dla setup-buildx-action.
// Wcześniej krok workflowu źródłował ci/lib.sh w `bash -c` — kod Basha
// wykonywany po to, żeby przeczytać daną.
func runPins(_ context.Context, w *workspace, args []string) error {
	const usage = "dft pins github-output"
	if err := expectArgs(args, 1, 1, usage); err != nil {
		return err
	}
	if args[0] != "github-output" {
		return cli.Usage("użycie: %s", usage)
	}
	p, err := pins.Load(w.root)
	if err != nil {
		return err
	}
	dockerfile, err := os.ReadFile(filepath.Join(w.root, pins.Dockerfile))
	if err != nil {
		return err
	}
	uv, err := pins.UVVersion(dockerfile)
	if err != nil {
		return err
	}
	lines := []string{
		"uv=" + uv,
		"buildkit=" + p.Get(pins.BuildKitImage),
	}

	if os.Getenv("GITHUB_OUTPUT") == "" {
		_, err := fmt.Fprintln(cli.Out, strings.Join(lines, "\n"))
		return err
	}
	if err := githubOutput(strings.Join(lines, "\n") + "\n"); err != nil {
		return err
	}
	cli.Step("przypięcia dla workflowu: %s", strings.Join(lines, ", "))
	return nil
}
