package pins

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Ścieżki plików, w których wersje zapisuje ktoś inny niż ten plik:
// Dependabot (Dockerfile), autor (.python-version), go (go.mod).
const (
	Dockerfile        = "services/api/Dockerfile"
	PythonVersionPath = "services/api/.python-version"
	GoMod             = "tools/go.mod"
)

var (
	uvFrom     = regexp.MustCompile(`(?m)^FROM ghcr\.io/astral-sh/uv:([0-9]+\.[0-9]+\.[0-9]+)@sha256:[0-9a-f]{64}\b`)
	pythonFrom = regexp.MustCompile(`(?m)^FROM python:([0-9]+\.[0-9]+)(?:\.[0-9]+)?(?:[-@\s]|$)`)
	goLine     = regexp.MustCompile(`(?m)^go ([0-9]+\.[0-9]+(\.[0-9]+)?)\s*$`)
)

// UVVersion zwraca wersję uv z Dockerfile (FROM ghcr.io/astral-sh/uv:X.Y.Z@sha256:…).
//
// To jedyne źródło prawdy o uv: przypięcie digestem aktualizuje Dependabot,
// CI instaluje tę samą wersję, a lokalny uv w innej wersji daje ostrzeżenie.
// Druga kopia wersji, której Dependabot nie widzi, rozjeżdżałaby się
// z pierwszą przy każdej jego aktualizacji.
func UVVersion(dockerfile []byte) (string, error) {
	m := uvFrom.FindAllSubmatch(dockerfile, -1)
	if len(m) != 1 {
		return "", fmt.Errorf("%s: oczekiwano jednego przypięcia uv (FROM ghcr.io/astral-sh/uv:X.Y.Z@sha256:…), znaleziono %d", Dockerfile, len(m))
	}
	return string(m[0][1]), nil
}

// PythonMinors zwraca różne wersje major.minor z linii FROM python:X.Y… —
// wszystkich etapów Dockerfile. Etapy muszą mieć tę samą bazę Pythona, bo
// bytecode z buildera jest uruchamiany w runtime.
func PythonMinors(dockerfile []byte) []string {
	var minors []string
	for _, m := range pythonFrom.FindAllSubmatch(dockerfile, -1) {
		if v := string(m[1]); !slices.Contains(minors, v) {
			minors = append(minors, v)
		}
	}
	slices.Sort(minors)
	return minors
}

// PythonVersion czyta services/api/.python-version: wersja major.minor,
// na której biegną testy na hoście.
func PythonVersion(content []byte) (string, error) {
	v := strings.TrimSpace(string(content))
	if !minorVersion.MatchString(v) {
		return "", fmt.Errorf("%s ma zawierać wersję w postaci major.minor, a zawiera %q", PythonVersionPath, v)
	}
	return v, nil
}

// GoDirective zwraca wersję z dyrektywy `go` w go.mod.
func GoDirective(gomod []byte) (string, error) {
	m := goLine.FindSubmatch(gomod)
	if m == nil {
		return "", fmt.Errorf("%s: brak dyrektywy go", GoMod)
	}
	return string(m[1]), nil
}
