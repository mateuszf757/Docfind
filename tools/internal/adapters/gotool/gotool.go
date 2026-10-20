// Package gotool to adapter toolchainu Go z mise (GOTOOLCHAIN=local): go vet,
// testy, staticcheck i govulncheck w wersjach przypiętych dyrektywą tool
// w go.mod oraz build programów tak samo jak ci/dft.
package gotool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Go — toolchain Go na module w ModuleDir.
type Go struct {
	Runner    proc.Runner
	ModuleDir string
	// Out i Err — wyjście narzędzi na bieżąco.
	Out, Err io.Writer
	// LookPath — wyszukiwanie programu w PATH; nil oznacza exec.LookPath.
	LookPath func(string) (string, error)
}

// Vet uruchamia go vet.
func (g Go) Vet(ctx context.Context) error {
	return g.check(ctx, nil, "vet", "./...")
}

// Staticcheck uruchamia staticcheck w wersji przypiętej dyrektywą tool.
func (g Go) Staticcheck(ctx context.Context) error {
	return g.check(ctx, nil, "tool", "staticcheck", "./...")
}

// Test uruchamia testy; race — z detektorem wyścigów (wymaga cgo, a więc
// kompilatora C: CCompiler).
func (g Go) Test(ctx context.Context, race bool, coverprofile string) error {
	args := []string{"test"}
	var env []string
	if race {
		args, env = append(args, "-race"), []string{"CGO_ENABLED=1"}
	}
	// -coverpkg=./...: pokrycie liczy kod wykonany przez jakikolwiek test,
	// a nie tylko testy własnego pakietu (decyzja 62).
	if coverprofile != "" {
		args = append(args, "-coverpkg=./...", "-coverprofile="+coverprofile)
	}
	return g.check(ctx, env, append(args, "./...")...)
}

// CompileTests kompiluje testy wszystkich pakietów do katalogu out bez
// uruchamiania (-c) — z tymi samymi flagami co Test (race, pokrycie) albo
// TestTagged (tag), żeby wpisy w pamięci podręcznej kompilacji były tymi,
// których szukają bramki (pisarz z decyzji 70).
func (g Go) CompileTests(ctx context.Context, out string, race, cover bool, tag string) error {
	args := []string{"test"}
	var env []string
	if race {
		args, env = append(args, "-race"), []string{"CGO_ENABLED=1"}
	}
	if cover {
		args = append(args, "-coverpkg=./...")
	}
	if tag != "" {
		args = append(args, "-tags", tag)
	}
	args = append(args, "-c", "-o", strings.TrimSuffix(out, "/")+"/")
	return g.check(ctx, env, append(args, "./...")...)
}

// TestTagged uruchamia testy z tagiem budowania (warianty negatywne na
// prawdziwym obrazie albo klastrze); run — wzorzec nazw testów (pusty —
// wszystkie), env — dodatkowe zmienne (np. DF_TEST_IMAGE).
func (g Go) TestTagged(ctx context.Context, tag, run string, env ...string) error {
	args := []string{"test", "-tags", tag, "-count=1"}
	if run != "" {
		args = append(args, "-run", run)
	}
	return g.check(ctx, env, append(args, "./...")...)
}

// CCompiler zwraca kompilator C, którego użyłoby cgo, i czy jest w PATH.
func (g Go) CCompiler(ctx context.Context) (string, bool) {
	cc, err := proc.Output(ctx, g.Runner, proc.Cmd{Name: "go", Args: []string{"env", "CC"}, Dir: g.ModuleDir})
	if err != nil || cc == "" {
		return "?", false
	}
	lookPath := g.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	_, err = lookPath(strings.Fields(cc)[0])
	return cc, err == nil
}

// Vulncheck zwraca podatności w kodzie osiągalnym z modułu (govulncheck)
// i migawkę bazy, z której pochodzą. dir — moduł z innego commita (punkt
// odniesienia); pusty — ModuleDir. govulncheck zawsze z ModuleDir, czyli
// w wersji przypiętej przez bieżący go.mod, a cudzy moduł dostaje przez -C:
// punkt odniesienia oceniony tym samym skanerem, nie tym z tamtego commita.
//
// Z -format json govulncheck kończy się kodem 0 także przy podatnościach —
// wynik niesie strumień; niezerowy kod to awaria (np. brak dostępu do
// vuln.go.dev). Błąd awarii nie owija pierwotnego (%v, nie %w): niezerowy
// kod narzędzia nie może udawać wyniku.
func (g Go) Vulncheck(ctx context.Context, dir string) ([]vuln.Finding, vuln.Database, error) {
	args := []string{"tool", "govulncheck"}
	if dir != "" {
		args = append(args, "-C", dir)
	}
	var out bytes.Buffer
	_, err := g.Runner.Run(ctx, proc.Cmd{Name: "go", Args: append(args, "-format", "json", "./..."), Dir: g.ModuleDir, Stdout: &out, Stderr: g.Err})
	if err != nil {
		return nil, vuln.Database{}, fmt.Errorf("govulncheck nie wykonał się — awaria narzędzia, nie wynik: %v", err)
	}
	return ParseGovulncheck(&out)
}

// Build buduje pakiet pkg do output (plik albo katalog zakończony
// separatorem) tak samo jak ci/dft: bez cgo, z -trimpath (bez ścieżek
// maszyny budującej w binarce) i bez zmian w go.mod. env — dodatkowe
// zmienne (GOOS, GOARCH, GOCACHE).
func (g Go) Build(ctx context.Context, pkg, output string, env ...string) error {
	_, err := g.Runner.Run(ctx, proc.Cmd{
		Name: "go",
		Args: []string{"build", "-mod=readonly", "-trimpath", "-o", output, pkg},
		Dir:  g.ModuleDir,
		Env:  append([]string{"CGO_ENABLED=0"}, env...),
	})
	if err != nil {
		return fmt.Errorf("go build %s: %w", pkg, err)
	}
	return nil
}

// check uruchamia `go …` w module; niezerowy kod to warunek niespełniony
// (kod nie przechodzi bramki), a niemożność uruchomienia — awaria.
func (g Go) check(ctx context.Context, env []string, args ...string) error {
	_, err := g.Runner.Run(ctx, proc.Cmd{Name: "go", Args: args, Dir: g.ModuleDir, Env: env, Stdout: g.Out, Stderr: g.Err})
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
