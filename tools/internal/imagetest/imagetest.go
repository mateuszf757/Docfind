// Package imagetest uruchamia testy jednostkowe w obrazie na musl — ta sama
// baza i ta sama łatka Pythona co produkcja (etap test w Dockerfile). Port
// ci/test-image.sh.
//
// Zadanie test uruchamia testy na interpreterze hosta: glibc i koła
// manylinux. Obraz biegnie na musl, z kołami musllinux. Kod, który przechodzi
// na jednym, a pada na drugim, wychodzi tylko tutaj.
package imagetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/docker"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

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
func Run(ctx context.Context, d docker.Client, root, service string, sourceDateEpoch int64, errOut io.Writer) error {
	contextDir := filepath.Join(root, "services", service)
	if info, err := os.Stat(contextDir); err != nil || !info.IsDir() {
		return cli.Usage("brak usługi %q w services/", service)
	}
	cli.Step("testy %s na obrazie (musl)", service)
	res, err := d.Run(ctx, "buildx", "build",
		"--target", "test",
		"--build-context", "config="+filepath.Join(root, "deploy", "config"),
		"--build-arg", fmt.Sprintf("SOURCE_DATE_EPOCH=%d", sourceDateEpoch),
		"--output", "type=cacheonly",
		"--progress", "plain",
		contextDir)
	log := string(res.Stdout) + string(res.Stderr)
	result, verdict := Verdict(log, err)
	if verdict == nil {
		cli.Step("testy na musl: %s", result)
		return nil
	}
	if code := cli.ExitCode(verdict); code == cli.ExitUnmet || proc.ExitCode(err) > 0 {
		fmt.Fprintln(errOut, tail(log, 40))
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
	var exitErr *proc.ExitError
	if !errors.As(buildErr, &exitErr) {
		return "", buildErr
	}
	if pytestFailed.MatchString(log) {
		if last != "" {
			last = " (" + last + ")"
		}
		return "", cli.Unmet("testy nie przechodzą na musl%s — wyżej wynik pytesta", last)
	}
	return "", fmt.Errorf("etap test nie zbudował się (kod %d) — to awaria budowania, nie wynik testów", exitErr.Code)
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
