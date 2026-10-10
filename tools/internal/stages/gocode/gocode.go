// Package gocode to bramki jakości kodu narzędzi w tools/: warstwy
// architektury (decyzja 39), formatowanie, go vet, staticcheck, testy,
// govulncheck i powtarzalność binarek.
package gocode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/layers"
	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Toolchain — toolchain Go (adapters/gotool).
type Toolchain interface {
	Vet(ctx context.Context) error
	Staticcheck(ctx context.Context) error
	Test(ctx context.Context, race bool) error
	CCompiler(ctx context.Context) (string, bool)
	Vulncheck(ctx context.Context) ([]vuln.Finding, error)
	Build(ctx context.Context, pkg, output string, env ...string) error
}

// Checker uruchamia bramki na module Go w ModuleDir.
type Checker struct {
	ModuleDir string
	Go        Toolchain
	// RequireRace — brak kompilatora C dla `go test -race` jest awarią,
	// a nie pominięciem (w CI). Lokalnie wystarcza ostrzeżenie.
	RequireRace bool
	// Exceptions — wyjątki z zakresu go (ci/vuln-exceptions.yaml) i dzień,
	// względem którego wygasają.
	Exceptions []vuln.Exception
	Today      time.Time
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
		{"warstwy", func(context.Context) error { return c.Layers() }},
		{"gofmt", func(context.Context) error { return c.Gofmt() }},
		{"go vet", c.Go.Vet},
		{"staticcheck", c.Go.Staticcheck},
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

// walk odwiedza pliki .go modułu z pominięciem katalogów ukrytych
// i testdata — to nie jest kod modułu.
func (c Checker) walk(visit func(path string) error) error {
	return filepath.WalkDir(c.ModuleDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != c.ModuleDir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		return visit(path)
	})
}

// Layers sprawdza kierunek zależności między warstwami (core/layers) na
// importach plików produkcyjnych — testy mogą składać etap z adapterami.
// Parser biblioteki standardowej, tylko importy: bez kompilacji i bez
// zależności od wyniku `go list`.
func (c Checker) Layers() error {
	module, err := modulePath(filepath.Join(c.ModuleDir, "go.mod"))
	if err != nil {
		return err
	}
	pkgs := map[string]*layers.Package{}
	fset := token.NewFileSet()
	err = c.walk(func(path string) error {
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(c.ModuleDir, filepath.Dir(path))
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		p, ok := pkgs[rel]
		if !ok {
			p = &layers.Package{Dir: rel}
			pkgs[rel] = p
		}
		for _, imp := range f.Imports {
			ip, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if !slices.Contains(p.Imports, ip) {
				p.Imports = append(p.Imports, ip)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var list []layers.Package
	for _, dir := range slices.Sorted(maps.Keys(pkgs)) {
		list = append(list, *pkgs[dir])
	}
	if problems := layers.Check(module, list); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(cli.Err, "WARSTWY: %s\n", p)
		}
		return cli.Unmet("%d naruszeń reguł warstw (decyzja 39) — wyżej pakiety i importy", len(problems))
	}
	cli.Step("warstwy: %d pakietów, kierunek zależności zgodny z regułami", len(list))
	return nil
}

func modulePath(gomod string) (string, error) {
	raw, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		if m, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "module "); ok {
			return strings.TrimSpace(m), nil
		}
	}
	return "", fmt.Errorf("%s: brak dyrektywy module", gomod)
}

// Gofmt sprawdza formatowanie wszystkich plików .go modułu przez go/format —
// tę samą bibliotekę, której używa gofmt. `gofmt -l` kończy się kodem 0 także
// wtedy, gdy wypisuje niesformatowane pliki, więc w powłoce wynik
// rozstrzygałoby parsowanie jego wyjścia.
func (c Checker) Gofmt() error {
	var unformatted []string
	err := c.walk(func(path string) error {
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		formatted, err := format.Source(src)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if !bytes.Equal(formatted, src) {
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

// Test uruchamia testy, z detektorem wyścigów, gdy się da.
//
// -race wymaga cgo, a cgo kompilatora C. Na hoście autora nie ma gcc
// (`cgo: C compiler "gcc" not found`), na runnerze ubuntu-24.04 jest. Narzędzia
// mają współbieżny kod — pętle żądań w tle, obserwatory logów, obsługę
// sygnałów — a wyścig w nim wychodzi jako losowo zły werdykt bramki.
func (c Checker) Test(ctx context.Context) error {
	cc, available := c.Go.CCompiler(ctx)
	switch {
	case available:
		return c.Go.Test(ctx, true)
	case c.RequireRace:
		return fmt.Errorf("brak kompilatora C (CC=%s) — w CI testy muszą biec z -race", cc)
	default:
		cli.Warn("brak kompilatora C (CC=%s) — testy bez -race; w CI biegną z -race", cc)
		return c.Go.Test(ctx, false)
	}
}

// Vulncheck ocenia podatności w kodzie osiągalnym (govulncheck) tą samą
// polityką co skan obrazu (decyzja 37): blokuje osiągalna podatność
// z poprawką, chyba że ma ważny wyjątek (zakres go); bez poprawki —
// ostrzeżenie.
func (c Checker) Vulncheck(ctx context.Context) error {
	findings, err := c.Go.Vulncheck(ctx)
	if err != nil {
		return err
	}
	evaluated, unused := vuln.Policy{Exceptions: c.Exceptions, Today: c.Today}.Evaluate(findings)
	blocking := 0
	for _, f := range evaluated {
		if f.Blocking {
			blocking++
		}
		fmt.Fprintf(cli.Out, "  %s %s (%s) poprawka: %s %s\n", f.ID, f.Package, f.Version, strings.Join(f.Fixed, ", "), f.Note)
	}
	for _, u := range unused {
		cli.Warn("wyjątek %s (zakres go) nie pasuje do żadnej podatności — do usunięcia", u)
	}
	if blocking > 0 {
		return cli.Unmet("govulncheck: %d osiągalnych podatności z poprawką bez wyjątku w kodzie z tools/", blocking)
	}
	return nil
}

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
	if err := c.Go.Build(ctx, "./cmd/...", cachedDir+string(os.PathSeparator)); err != nil {
		return nil, err
	}
	cli.Step("build od zera (pusty GOCACHE)")
	if err := c.Go.Build(ctx, "./cmd/...", freshDir+string(os.PathSeparator), "GOCACHE="+freshCache); err != nil {
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
