// Package reproducible sprawdza, że build obrazu jest powtarzalny: build
// z pamięcią podręczną i build od zera dają identyczny obraz (decyzja 21).
// Port ci/check-reproducible.sh i ci/compare_oci.py.
//
// Powtarzalność oznacza, że obraz da się niezależnie odtworzyć z commita i że
// ta sama zawartość ma zawsze ten sam digest — więc zmiana digestu na klastrze
// zawsze oznacza zmianę zawartości, a nie tylko chwilę budowania.
package reproducible

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/build"
	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/oci"
	"github.com/mateuszf757/Docfind/tools/internal/pins"
)

// Checker porównuje dwa buildy tej samej usługi.
type Checker struct {
	Builder build.Builder
	// Options — build bazowy (korzeń, usługa, obraz); NoCache i wyjście
	// ustawia Run.
	Options build.Options
	// PinnedBuildKit — DF_BUILDKIT_IMAGE, BuildKit z CI.
	PinnedBuildKit string
	Out            io.Writer
}

// Run buduje dwa razy i porównuje obrazy warstwa po warstwie.
//
// Pierwszy build korzysta z pamięci podręcznej, jaka akurat jest; drugi idzie
// od zera. Dwa buildy od zera przepuściły zależność od stanu pamięci
// podręcznej: warstwy z cache niosły czasy z wcześniejszych buildów, a build
// od zera — nie. W CI pamięć podręczna jest pusta i oba buildy są świeże.
func (c Checker) Run(ctx context.Context, tmp string) error {
	// Brudne drzewo dostaje bieżący czas zamiast czasu commita, więc
	// z definicji nie jest powtarzalne — sprawdzanie go nic by nie dowiodło.
	if err := c.Builder.Repo.RequireClean(ctx); err != nil {
		return err
	}

	// Oba buildy biegną na tym samym builderze, więc wersja BuildKitu nie
	// wpływa na werdykt. Wpływa za to na twierdzenie, że lokalny obraz jest
	// bajt w bajt taki jak z CI: frontend Dockerfile jest wbudowany w BuildKit.
	if b, err := c.Builder.Docker.CurrentBuilder(ctx); err == nil {
		if ciVersion := pins.ImageTag(c.PinnedBuildKit); b.BuildKitVersion != "" && b.BuildKitVersion != ciVersion {
			cli.Warn("lokalny BuildKit %s, CI używa %s — lokalny digest może różnić się od tego z CI", b.BuildKitVersion, ciVersion)
		}
	}

	var layouts []oci.Layout
	for _, run := range []struct {
		name    string
		noCache bool
	}{{"a", false}, {"b", true}} {
		label := "z pamięcią podręczną"
		if run.noCache {
			label = "od zera"
		}
		cli.Step("build %s %s", run.name, label)
		layout, err := c.buildAndSave(ctx, tmp, run.name, run.noCache)
		if err != nil {
			return err
		}
		layouts = append(layouts, layout)
	}

	cli.Step("porównanie manifestów, konfiguracji i warstw")
	identical, err := oci.Compare(layouts[0], layouts[1], c.Out)
	if err != nil {
		// Kody rozdzielone: wcześniej awaria porównania i różnica w obrazach
		// kończyły się tym samym „build nie jest powtarzalny".
		return fmt.Errorf("porównanie obrazów nie wykonało się — to awaria narzędzia, nie wynik: %w", err)
	}
	if !identical {
		return cli.Unmet("build nie jest powtarzalny — wyżej warstwy i pliki, które się różnią")
	}
	cli.Step("build powtarzalny: build z pamięcią podręczną i build od zera dały identyczny obraz")
	return nil
}

func (c Checker) buildAndSave(ctx context.Context, tmp, name string, noCache bool) (oci.Layout, error) {
	logPath := filepath.Join(tmp, "build-"+name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return "", err
	}
	opts := c.Options
	opts.NoCache = noCache
	opts.Out, opts.Err = logFile, logFile
	res, buildErr := c.Builder.Build(ctx, opts)
	if err := logFile.Close(); err != nil {
		return "", err
	}
	if buildErr != nil {
		if log, err := os.ReadFile(logPath); err == nil {
			fmt.Fprintln(cli.Err, tail(string(log), 20))
		}
		// %v, nie %w: niespełniony warunek wewnątrz builda to tutaj awaria
		// sprawdzenia powtarzalności, a nie jego wynik.
		return "", fmt.Errorf("build %s nie powiódł się: %v", name, buildErr)
	}
	archive := filepath.Join(tmp, name+".tar")
	if err := c.Builder.Docker.Save(ctx, res.Ref(), archive); err != nil {
		return "", err
	}
	dir := filepath.Join(tmp, name)
	if err := oci.Extract(archive, dir); err != nil {
		return "", err
	}
	if err := os.Remove(archive); err != nil {
		return "", err
	}
	return oci.Layout(dir), nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
