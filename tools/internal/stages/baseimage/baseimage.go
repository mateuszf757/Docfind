// Package baseimage sprawdza bazę zbudowanego obrazu względem wersji
// zapisanych w repozytorium (dawniej ci/check-image-base.sh).
//
// Dependabot odświeża digest pływającego tagu python:3.14-alpine i takie
// odświeżenie jest scalane automatycznie jak łatka (decyzja 22). Zwykle to
// łatka Pythona albo pakietów Alpine — ale pod tym samym tagiem pojawia się
// też nowe wydanie Alpine: nowy musl, nowy OpenSSL, a to nie jest łatka.
package baseimage

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/container"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Images — odczyt z obrazu (adapters/docker).
type Images interface {
	ReadFiles(ctx context.Context, ref string, paths ...string) ([]byte, error)
	RunToExit(ctx context.Context, s container.Spec, timeout time.Duration) (container.Exit, error)
}

// Facts — wersje odczytane z obrazu.
type Facts struct {
	Alpine  string
	Python  string
	Musl    string
	OpenSSL string
}

// Read czyta fakty z obrazu bez powłoki w kontenerze: plik wydania i baza
// pakietów apk przez `cat`, wersja Pythona z interpretera, który obraz
// naprawdę uruchomi (a nie z PYTHON_VERSION w konfiguracji obrazu).
func Read(ctx context.Context, images Images, image string) (Facts, error) {
	raw, err := images.ReadFiles(ctx, image, "/etc/alpine-release", "/lib/apk/db/installed")
	if err != nil {
		return Facts{}, err
	}
	release, db, _ := bytes.Cut(raw, []byte("\n"))
	packages := ParseAPKInstalled(db)
	exit, err := images.RunToExit(ctx, container.Spec{Image: image, NoNetwork: true, Entrypoint: []string{"python", "--version"}}, 0)
	if err == nil && exit.Code != 0 {
		err = fmt.Errorf("kod %d: %s", exit.Code, strings.TrimSpace(exit.Stderr))
	}
	if err != nil {
		return Facts{}, fmt.Errorf("python --version w %s: %w", image, err)
	}
	facts := Facts{
		Alpine:  strings.TrimSpace(string(release)),
		Python:  strings.TrimPrefix(strings.TrimSpace(exit.Stdout), "Python "),
		Musl:    packages["musl"],
		OpenSSL: packages["libssl3"],
	}
	if facts.Alpine == "" || facts.Python == "" || strings.Contains(facts.Python, " ") {
		return Facts{}, fmt.Errorf("nieczytelne wersje bazy %s: Alpine %q, Python %q", image, facts.Alpine, facts.Python)
	}
	return facts, nil
}

// ParseAPKInstalled czyta bazę zainstalowanych pakietów apk
// (/lib/apk/db/installed): rekordy oddzielone pustą linią, P: — nazwa,
// V: — wersja.
func ParseAPKInstalled(db []byte) map[string]string {
	packages := map[string]string{}
	var name, version string
	flush := func() {
		if name != "" {
			packages[name] = version
		}
		name, version = "", ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(db))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "P:"):
			name = line[2:]
		case strings.HasPrefix(line, "V:"):
			version = line[2:]
		}
	}
	flush()
	return packages
}

// Check porównuje fakty z zapisanym wydaniem Alpine (DF_BASE_ALPINE)
// i wersją Pythona testów (.python-version).
func Check(f Facts, wantAlpine, wantPython string) error {
	v := &cli.Verdict{}
	if minor := majorMinor(f.Alpine); minor != wantAlpine {
		v.Fail("obraz stoi na Alpine %s, a zapisane jest %s (DF_BASE_ALPINE w ci/pins.env). "+
			"Nowe wydanie Alpine przyszło pod tym samym tagiem bazy — sprawdź zmiany musl i OpenSSL, "+
			"potem podbij DF_BASE_ALPINE w tym samym PR-ze", f.Alpine, wantAlpine)
	}
	if minor := majorMinor(f.Python); minor != wantPython {
		v.Fail("obraz ma Pythona %s, a testy biegną na %s (services/api/.python-version)", f.Python, wantPython)
	}
	return v.Err("baza obrazu")
}

func majorMinor(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}
