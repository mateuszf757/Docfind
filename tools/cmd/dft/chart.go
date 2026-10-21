package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/registry"
	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
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

// registryOwner — rejestr i właściciel z otoczenia: DF_IMAGE_REGISTRY przenosi
// charty razem z obrazami na rejestr lokalny.
func registryOwner() (string, string) {
	return os.Getenv("DF_IMAGE_REGISTRY"), os.Getenv("GITHUB_REPOSITORY_OWNER")
}

const chartUsage = "dft chart <publish --image <obraz@sha256:…> [--tested <digest>] | platform | mirror | delivery --app <chart@sha256:…> --platform <chart@sha256:…>>"

// runChart — charty w rejestrze OCI: aplikacji (publish, decyzja 45),
// platformy i lustro przypiętych chartów z zewnątrz (platform, mirror,
// decyzja 80).
func runChart(ctx context.Context, w *workspace, args []string) error {
	if len(args) == 0 {
		return cli.Usage("użycie: %s", chartUsage)
	}
	switch args[0] {
	case "publish":
		return runChartPublish(ctx, w, args[1:])
	case "platform":
		return runChartPlatform(ctx, w, args[1:])
	case "mirror":
		return runChartMirror(ctx, w, args[1:])
	case "delivery":
		return runChartDelivery(ctx, w, args[1:])
	}
	return cli.Usage("użycie: %s", chartUsage)
}

// runChartPublish — `dft chart publish --image <obraz@sha256:…>`: chart
// aplikacji z tym obrazem w rejestrze OCI (decyzja 45). W CI na main, po
// bramkach klastra; wyjścia kroku — repozytorium, digest i wersja charta dla
// atestacji.
func runChartPublish(ctx context.Context, w *workspace, args []string) error {
	const usage = "dft chart publish --image <obraz@sha256:…> [--tested <digest charta z zadania cluster>]"
	fs := flag.NewFlagSet("chart publish", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	image := fs.String("image", "", "")
	tested := fs.String("tested", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *image == "" {
		return cli.Usage("użycie: %s", usage)
	}
	pub, err := w.chartPublisher().Publish(ctx, *image, *tested)
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

// runChartPlatform — `dft chart platform`: chart platformy z bieżącego
// commita w rejestrze OCI (decyzja 80), obok charta aplikacji. Wyjścia
// kroku — repozytorium i digest dla atestacji.
func runChartPlatform(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft chart platform"); err != nil {
		return err
	}
	reg, owner := registryOwner()
	pub, err := chartpub.PlatformPublisher{
		ChartDir: filepath.Join(w.root, "deploy", "charts", "platform"), Repo: w.git(),
		Registry: registry.Client{}, Public: registry.Client{Anonymous: true},
		Repository: artifact.PlatformChartName(reg, owner), Source: sourceURL,
	}.Publish(ctx)
	if err != nil {
		return err
	}
	if err := githubOutput(fmt.Sprintf("platform-chart=%s\nplatform-chart-digest=%s\n", pub.Repository, pub.Digest)); err != nil {
		return err
	}
	if err := w.writeReport("chart-platform", pub); err != nil {
		return err
	}
	return appendSummary(fmt.Sprintf("### Chart platformy\n\n| Wersja | Chart | Publiczny |\n|---|---|---|\n| %s | `%s@%s` | %s |\n", pub.Version, pub.Repository, pub.Digest, yesNo(pub.Public)))
}

// runChartMirror — `dft chart mirror`: przypięte charty platformy, którymi
// zarządza Argo CD, w lustrze w rejestrze OCI (decyzja 80) — te same bajty co
// po sprawdzeniu sumy z ci/pins.env, przypięte digestem.
func runChartMirror(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft chart mirror"); err != nil {
		return err
	}
	m, err := w.mirror()
	if err != nil {
		return err
	}
	stored, err := m.Run(ctx)
	if reportErr := w.writeReport("chart-mirror", stored); reportErr != nil {
		return errors.Join(err, reportErr)
	}
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("### Lustro chartów platformy\n\n| Chart | Wersja | Digest | Wysłany | Publiczny |\n|---|---|---|---|---|\n")
	for _, s := range stored {
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s | %s |\n", s.Repository, s.Version, s.Digest, yesNo(s.Pushed), yesNo(s.Public))
	}
	return appendSummary(b.String())
}

// mirror — lustro przypiętych chartów w rejestrze z otoczenia.
func (w *workspace) mirror() (chartpub.Mirror, error) {
	p, err := pins.Load(w.root)
	if err != nil {
		return chartpub.Mirror{}, err
	}
	reg, owner := registryOwner()
	return chartpub.Mirror{
		Pins: p, Charts: w.charts(), Registry: registry.Client{}, Public: registry.Client{Anonymous: true},
		Repository: func(pin string) string { return artifact.MirrorName(reg, owner, pin) },
		Source:     sourceURL,
	}, nil
}

// runChartDelivery — `dft chart delivery --app <chart@sha256:…> --platform
// <chart@sha256:…>`: pakiet dostawy z bieżącego commita (decyzja 81) —
// Applications platformy i aplikacji po digestach charta aplikacji i charta
// platformy opublikowanych w tym biegu oraz lustra. W CI po nich, w zadaniu
// chart; wyjścia kroku — repozytorium i digest dla atestacji.
func runChartDelivery(ctx context.Context, w *workspace, args []string) error {
	const usage = "dft chart delivery --app <chart@sha256:…> --platform <chart@sha256:…>"
	fs := flag.NewFlagSet("chart delivery", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	app := fs.String("app", "", "")
	plat := fs.String("platform", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *app == "" || *plat == "" {
		return cli.Usage("użycie: %s", usage)
	}
	m, err := w.mirror()
	if err != nil {
		return err
	}
	reg, owner := registryOwner()
	pub, b, err := chartpub.DeliveryPublisher{
		Root: w.root, Repo: w.git(), Registry: registry.Client{}, Public: registry.Client{Anonymous: true},
		Mirror: m, Repository: artifact.DeliveryChartName(reg, owner), Source: sourceURL,
	}.Publish(ctx, *app, *plat)
	if err != nil {
		return err
	}
	if err := githubOutput(fmt.Sprintf("delivery-chart=%s\ndelivery-chart-digest=%s\n", pub.Repository, pub.Digest)); err != nil {
		return err
	}
	if err := w.writeReport("chart-delivery", map[string]any{"chart": pub, "bundle": b}); err != nil {
		return err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "### Pakiet dostawy\n\n`%s@%s` (%s), publiczny: %s\n\n| Fala | Komponent | Chart |\n|---|---|---|\n", pub.Repository, pub.Digest, pub.Version, yesNo(pub.Public))
	for _, c := range b.Components {
		fmt.Fprintf(&sb, "| %d | %s | `%s@%s` |\n", c.Wave, c.Name, c.RepoURL, c.Digest)
	}
	return appendSummary(sb.String())
}

func yesNo(v bool) string {
	if v {
		return "tak"
	}
	return "nie"
}
