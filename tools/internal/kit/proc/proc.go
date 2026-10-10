// Package proc uruchamia programy zewnętrzne: git, docker, kubectl, helm.
//
// Logika bramek nie woła os/exec bezpośrednio, tylko przez interfejs Runner.
// W testach podstawia się Fake z nagranymi wynikami — parsowanie wyjścia
// i decyzja o wyniku są wtedy testowane bez Dockera, klastra i sieci.
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Cmd opisuje jedno wywołanie programu.
type Cmd struct {
	Name string
	Args []string
	// Dir — katalog roboczy; pusty oznacza bieżący.
	Dir string
	// Env jest dokładane do środowiska procesu, nie zastępuje go.
	Env []string
	// Stdin — wejście programu. Dane wrażliwe (token) idą tędy, nigdy
	// w Args: argumenty procesu widzi każdy użytkownik systemu w `ps`.
	Stdin io.Reader
	// Stdout i Stderr — gdy ustawione, wyjście płynie tam na bieżąco
	// (długie programy: testy, build) i nie trafia do Result.
	Stdout io.Writer
	Stderr io.Writer
}

// String zwraca polecenie w postaci do komunikatu.
func (c Cmd) String() string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

// Result to zebrane wyjście programu.
type Result struct {
	Stdout []byte
	Stderr []byte
}

// ExitError — program uruchomił się i zakończył kodem różnym od zera.
// Odróżnia „program odpowiedział nie" od „programu nie dało się uruchomić".
type ExitError struct {
	Cmd    string
	Code   int
	Stderr []byte
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("%s zakończył się kodem %d", e.Cmd, e.Code)
	if s := strings.TrimSpace(string(e.Stderr)); s != "" {
		msg += ": " + lastLines(s, 5)
	}
	return msg
}

// ExitCode zwraca kod wyjścia z błędu ExitError albo -1, gdy błąd jest inny.
func ExitCode(err error) int {
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}
	return -1
}

// Runner uruchamia programy.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// Exec uruchamia prawdziwe procesy.
type Exec struct{}

// Run uruchamia program i czeka na jego zakończenie.
//
// Anulowanie kontekstu (SIGINT, SIGTERM do nas) wysyła dziecku SIGTERM,
// a nie domyślny SIGKILL: kubectl drain, docker run czy helm dostają szansę
// zakończyć się po swojemu. Po WaitDelay Go zabija proces i zamyka potoki,
// żeby zawieszone dziecko nie trzymało nas bez końca.
func (Exec) Run(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	cmd.Stdin = c.Stdin
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	// Przy wyjściu płynącym na bieżąco stderr i tak zbieramy kopię — trafia
	// do ExitError, żeby komunikat o porażce mówił, co program zgłosił.
	cmd.Stderr = &stderr
	if c.Stderr != nil {
		cmd.Stderr = io.MultiWriter(c.Stderr, &stderr)
	}

	err := cmd.Run()
	result := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		return result, &ExitError{Cmd: c.String(), Code: exitErr.ExitCode(), Stderr: result.Stderr}
	}
	if ctx.Err() != nil {
		return result, fmt.Errorf("%s przerwany: %w", c.String(), ctx.Err())
	}
	return result, fmt.Errorf("nie udało się uruchomić %s: %w", c.String(), err)
}

// Output uruchamia program i zwraca stdout bez końcowych białych znaków.
func Output(ctx context.Context, r Runner, c Cmd) (string, error) {
	res, err := r.Run(ctx, c)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
