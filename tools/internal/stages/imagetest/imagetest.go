// Package imagetest uruchamia testy jednostkowe w obrazie na musl — ta sama
// baza i ta sama łatka Pythona co produkcja (etap test w Dockerfile);
// dawniej ci/test-image.sh.
//
// Zadanie test uruchamia testy na interpreterze hosta: glibc i koła
// manylinux. Obraz biegnie na musl, z kołami musllinux. Kod, który przechodzi
// na jednym, a pada na drugim, wychodzi tylko tutaj.
package imagetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Builder — build etapu Dockerfile (adapters/docker).
type Builder interface {
	Build(ctx context.Context, req artifact.BuildRequest) (string, error)
}

var (
	// Linia podsumowania pytesta, np. „54 passed in 0.30s".
	summary = regexp.MustCompile(`[0-9]+ (passed|failed)[^"\n]*`)
	// BuildKit zgłasza porażkę kroku RUN tym zdaniem, z poleceniem w nim.
	pytestFailed = regexp.MustCompile(`python -m pytest.*did not complete successfully`)
)

// Run buduje etap test. Ten sam SOURCE_DATE_EPOCH co build obrazu: etap
// builder przychodzi wtedy z pamięci podręcznej, zamiast budować się drugi
// raz. Wynik testów też jest w pamięci podręcznej — te same wejścia dają ten
// sam wynik, więc bez zmian w kodzie pytest nie biegnie ponownie.
//
// Wynik negatywny (kod 1) tylko przy czerwonym pyteście; każda inna porażka
// budowania — np. brak sieci przy uv sync — to awaria (kod 2), bo nie może
// wyglądać jak czerwony test.
func Run(ctx context.Context, b Builder, root, service string, sourceDateEpoch int64, errOut io.Writer) error {
	contextDir := filepath.Join(root, "services", service)
	if info, err := os.Stat(contextDir); err != nil || !info.IsDir() {
		return cli.Usage("brak usługi %q w services/", service)
	}
	cli.Step("testy %s na obrazie (musl)", service)
	var log bytes.Buffer
	_, err := b.Build(ctx, artifact.BuildRequest{
		ContextDir: contextDir,
		Target:     "test",
		Contexts:   []artifact.BuildContext{{Name: "config", Dir: filepath.Join(root, "deploy", "config")}},
		Args:       []artifact.BuildArg{{Name: "SOURCE_DATE_EPOCH", Value: strconv.FormatInt(sourceDateEpoch, 10)}},
		CacheOnly:  true,
		Out:        &log,
		Err:        &log,
	})
	result, verdict := Verdict(log.String(), err)
	if verdict == nil {
		cli.Step("testy na musl: %s", result)
		return nil
	}
	if log.Len() > 0 {
		fmt.Fprintln(errOut, tail(log.String(), 40))
	}
	return verdict
}

// Verdict rozstrzyga wynik builda etapu test z jego logu: podsumowanie
// pytesta przy sukcesie, wynik negatywny przy czerwonym pyteście, awaria przy
// każdej innej porażce budowania.
func Verdict(log string, buildErr error) (string, error) {
	matches := summary.FindAllString(log, -1)
	last := ""
	if len(matches) > 0 {
		last = matches[len(matches)-1]
	}
	if buildErr == nil {
		if last == "" {
			last = "bez zmian od poprzedniego biegu (wynik z pamięci podręcznej)"
		}
		return last, nil
	}
	if errors.Is(buildErr, context.Canceled) || errors.Is(buildErr, context.DeadlineExceeded) {
		return "", buildErr
	}
	if pytestFailed.MatchString(log) {
		if last != "" {
			last = " (" + last + ")"
		}
		return "", cli.Unmet("testy nie przechodzą na musl%s — wyżej wynik pytesta", last)
	}
	// %v, nie %w: niezerowy kod builda nie może udawać wyniku testów.
	return "", fmt.Errorf("etap test nie zbudował się — to awaria budowania, nie wynik testów: %v", buildErr)
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
