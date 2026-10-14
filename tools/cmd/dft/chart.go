package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/registry"
	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/chartpub"
)

// sourceURL — repozytorium kodu, jak w etykiecie obrazu (Dockerfile).
const sourceURL = "https://github.com/mateuszf757/Docfind"

// chartName — repozytorium charta w rejestrze obrazów; DF_IMAGE_REGISTRY
// przenosi je razem z obrazami na rejestr lokalny.
func chartName() string {
	return artifact.ChartName(os.Getenv("DF_IMAGE_REGISTRY"), os.Getenv("GITHUB_REPOSITORY_OWNER"))
}

func (w *workspace) chartPublisher() chartpub.Publisher {
	return chartpub.Publisher{
		ChartDir: filepath.Join(w.root, "deploy", "charts", "docfind"), Repo: w.git(), Registry: registry.Client{}, Images: registry.Client{},
		Repository: chartName(), Source: sourceURL,
	}
}

// runChart — `dft chart publish --image <obraz@sha256:…>`: chart aplikacji
// z tym obrazem w rejestrze OCI (decyzja 45). W CI na main, po publikacji
// obrazu; wyjścia kroku — repozytorium, digest i wersja charta dla
// atestacji.
func runChart(ctx context.Context, w *workspace, args []string) error {
	const usage = "dft chart publish --image <obraz@sha256:…>"
	if len(args) == 0 || args[0] != "publish" {
		return cli.Usage("użycie: %s", usage)
	}
	fs := flag.NewFlagSet("chart publish", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	image := fs.String("image", "", "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 || *image == "" {
		return cli.Usage("użycie: %s", usage)
	}
	pub, err := w.chartPublisher().Publish(ctx, *image)
	if err != nil {
		return err
	}
	if err := githubOutput(fmt.Sprintf("chart=%s\nchart-digest=%s\nchart-version=%s\n", pub.Repository, pub.Digest, pub.Version)); err != nil {
		return err
	}
	if err := w.writeReport("chart", pub); err != nil {
		return err
	}
	return appendSummary(fmt.Sprintf("### Chart OCI\n\n| Wersja | Chart | Obraz |\n|---|---|---|\n| %s | `%s@%s` | `%s` |\n", pub.Version, pub.Repository, pub.Digest, pub.Image))
}
