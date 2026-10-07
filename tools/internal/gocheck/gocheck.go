// Package gocheck to bramki jakości kodu narzędzi w tools/: formatowanie,
// go vet, staticcheck, testy, govulncheck i powtarzalność binarek.
package gocheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/format"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// Checker uruchamia bramki na module Go w ModuleDir.
type Checker struct {
	ModuleDir string
	Runner    proc.Runner
	// RequireRace — brak kompilatora C dla `go test -race` jest awarią,
	// a nie pominięciem (w CI). Lokalnie wystarcza ostrzeżenie.
	RequireRace bool
	// Out i Err — wyjście narzędzi na bieżąco.
	Out, Err io.Writer
	// LookPath — wyszukiwanie programu w PATH; nil oznacza exec.LookPath.
	LookPath func(string) (string, error)
}

// All uruchamia wszystkie bramki poza powtarzalnością i zgłasza
// niespełnione razem: niesformatowany plik nie powinien ukrywać
// czerwonego testu.
func (c Checker) All(ctx context.Context) error {
	v := &cli.Verdict{}
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"gofmt", func(context.Context) error { return c.Gofmt() }},
		{"go vet", c.Vet},
		{"staticcheck", c.Staticcheck},
		{"go test", c.Test},
		{"govulncheck", c.Vulncheck},
	}
	for _, step := range steps {
		cli.Step("%s", step.name)
		err := step.run(ctx)
		switch cli.ExitCode(err) {
		case cli.ExitOK:
		case cli.ExitUnmet:
			v.Fail("%v", err)
		default:
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return v.Err("bramki jakości Go")
}

// Gofmt sprawdza formatowanie wszystkich plików .go modułu przez go/format —
// tę samą bibliotekę, której używa gofmt. `gofmt -l` kończy się kodem 0 także
// wtedy, gdy wypisuje niesformatowane pliki, więc w powłoce wynik
// rozstrzygałoby parsowanie jego wyjścia.
func (c Checker) Gofmt() error {
	var unformatted []string
	err := filepath.WalkDir(c.ModuleDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != c.ModuleDir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		formatted, err := format.Source(src)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if string(formatted) != string(src) {
			rel, _ := filepath.Rel(c.ModuleDir, path)
			unformatted = append(unformatted, rel)
		}
		return nil
	})
	if err != nil {
		return cli.Unmet("formatowanie: %v", err)
	}
	if len(unformatted) > 0 {
		return cli.Unmet("pliki bez gofmt (popraw: go fmt ./...): %s", strings.Join(unformatted, ", "))
	}
	return nil
}

// Vet uruchamia go vet.
func (c Checker) Vet(ctx context.Context) error {
	return c.goTool(ctx, nil, "vet", "./...")
}

// Staticcheck uruchamia staticcheck w wersji przypiętej dyrektywą tool w go.mod.
func (c Checker) Staticcheck(ctx context.Context) error {
	return c.goTool(ctx, nil, "tool", "staticcheck", "./...")
}

// Test uruchamia testy, z detektorem wyścigów, gdy się da.
//
// -race wymaga cgo, a cgo kompilatora C. Na hoście autora nie ma gcc
// (`cgo: C compiler "gcc" not found`), na runnerze ubuntu-24.04 jest. Narzędzia
// mają współbieżny kod — pętle żądań w tle, obserwatory logów, obsługę
// sygnałów — a wyścig w nim wychodzi jako losowo zły werdykt bramki.
func (c Checker) Test(ctx context.Context) error {
	cc, available := c.cCompiler(ctx)
	switch {
	case available:
		return c.goTool(ctx, []string{"CGO_ENABLED=1"}, "test", "-race", "./...")
	case c.RequireRace:
		return fmt.Errorf("brak kompilatora C (CC=%s) — w CI testy muszą biec z -race", cc)
	default:
		cli.Warn("brak kompilatora C (CC=%s) — testy bez -race; w CI biegną z -race", cc)
		return c.goTool(ctx, nil, "test", "./...")
	}
}

// Vulncheck uruchamia govulncheck: podatności w modułach, do których kod
// naprawdę sięga (analiza wywołań), a nie każde CVE w go.sum. Baza zmienia
// się codziennie, więc ten sam commit może jutro być czerwony — polityka
// progów i wyjątków przychodzi razem ze skanem obrazu.
func (c Checker) Vulncheck(ctx context.Context) error {
	err := c.goTool(ctx, nil, "tool", "govulncheck", "./...")
	// govulncheck: 3 — znalezione podatności (wynik), inne niezerowe —
	// awaria, np. brak dostępu do vuln.go.dev. Błąd awarii nie owija
	// pierwotnego (%v, nie %w): ten niesie typ „warunek niespełniony"
	// i kod wyjścia 1, a brak bazy podatności nie jest wynikiem.
	if code := proc.ExitCode(err); code == 3 {
		return cli.Unmet("govulncheck znalazł podatności w kodzie osiągalnym z tools/")
	} else if code > 0 {
		return fmt.Errorf("govulncheck nie wykonał się — awaria narzędzia, nie wynik: %v", err)
	}
	return err
}

func (c Checker) cCompiler(ctx context.Context) (string, bool) {
	cc, err := proc.Output(ctx, c.Runner, proc.Cmd{Name: "go", Args: []string{"env", "CC"}, Dir: c.ModuleDir})
	if err != nil || cc == "" {
		return "?", false
	}
	lookPath := c.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	_, err = lookPath(strings.Fields(cc)[0])
	return cc, err == nil
}

// goTool uruchamia `go …` w module; niezerowy kod to warunek niespełniony
// (kod nie przechodzi bramki), a niemożność uruchomienia — awaria.
func (c Checker) goTool(ctx context.Context, env []string, args ...string) error {
	_, err := c.Runner.Run(ctx, proc.Cmd{Name: "go", Args: args, Dir: c.ModuleDir, Env: env, Stdout: c.Out, Stderr: c.Err})
	var exitErr *proc.ExitError
	if errors.As(err, &exitErr) {
		return &unmetExit{cli.UnmetError{Msg: fmt.Sprintf("go %s zakończył się kodem %d", strings.Join(args, " "), exitErr.Code)}, exitErr}
	}
	return err
}

// unmetExit to warunek niespełniony, który zachowuje kod wyjścia programu.
type unmetExit struct {
	cli.UnmetError
	exit *proc.ExitError
}

func (e *unmetExit) Unwrap() []error { return []error{&e.UnmetError, e.exit} }

// Binary to wynik jednego buildu w sprawdzeniu powtarzalności.
type Binary struct {
	Name   string
	SHA256 string
}

// Reproducible buduje wszystkie programy z ./cmd/... dwa razy — z bieżącą
// pamięcią podręczną i z pustą — i porównuje bajty. Ten sam wariant co przy
// obrazie (decyzja 21): build z pamięci podręcznej trywialnie daje ten sam
// wynik, więc drugi idzie od zera.
func (c Checker) Reproducible(ctx context.Context, tmp string) ([]Binary, error) {
	cachedDir := filepath.Join(tmp, "z-pamieci")
	freshDir := filepath.Join(tmp, "od-zera")
	freshCache := filepath.Join(tmp, "gocache")
	for _, dir := range []string{cachedDir, freshDir, freshCache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	cli.Step("build z pamięcią podręczną")
	if err := c.build(ctx, cachedDir, nil); err != nil {
		return nil, err
	}
	cli.Step("build od zera (pusty GOCACHE)")
	if err := c.build(ctx, freshDir, []string{"GOCACHE=" + freshCache}); err != nil {
		return nil, err
	}

	cached, err := sums(cachedDir)
	if err != nil {
		return nil, err
	}
	fresh, err := sums(freshDir)
	if err != nil {
		return nil, err
	}
	if len(cached) == 0 {
		return nil, fmt.Errorf("build nie dał żadnej binarki w %s", cachedDir)
	}
	var binaries []Binary
	v := &cli.Verdict{}
	for _, name := range slices.Sorted(maps.Keys(cached)) {
		binaries = append(binaries, Binary{Name: name, SHA256: cached[name]})
		if fresh[name] != cached[name] {
			v.Fail("%s: %s z pamięci podręcznej ≠ %s od zera", name, cached[name], fresh[name])
		}
	}
	return binaries, v.Err("powtarzalność binarek Go")
}

// build buduje programy tak samo jak ci/dft: bez cgo, z -trimpath (bez
// ścieżek maszyny budującej w binarce) i bez zmian w go.mod.
func (c Checker) build(ctx context.Context, outDir string, env []string) error {
	env = append([]string{"CGO_ENABLED=0"}, env...)
	_, err := c.Runner.Run(ctx, proc.Cmd{
		Name: "go",
		Args: []string{"build", "-mod=readonly", "-trimpath", "-o", outDir + string(os.PathSeparator), "./cmd/..."},
		Dir:  c.ModuleDir,
		Env:  env,
	})
	if err != nil {
		return fmt.Errorf("go build: %w", err)
	}
	return nil
}

func sums(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		result[e.Name()] = hex.EncodeToString(sum[:])
	}
	return result, nil
}
