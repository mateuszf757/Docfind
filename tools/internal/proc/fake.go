package proc

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Fake odtwarza nagrane wyniki zamiast uruchamiać programy — do testów
// logiki, która parsuje wyjście docker, kubectl czy helm.
type Fake struct {
	mu sync.Mutex
	// Responses — klucz to polecenie w postaci Cmd.String().
	Responses map[string]FakeResponse
	// Calls — wywołania w kolejności, do sprawdzenia w teście.
	Calls []Cmd
}

// FakeResponse to nagrany wynik jednego polecenia.
type FakeResponse struct {
	Stdout string
	Stderr string
	// Code różny od zera daje ExitError.
	Code int
	// Err — awaria uruchomienia (brak programu).
	Err error
}

// Run zwraca nagrany wynik albo błąd dla polecenia, którego test nie przewidział.
func (f *Fake) Run(_ context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	resp, ok := f.Responses[c.String()]
	f.mu.Unlock()
	if !ok {
		return Result{}, fmt.Errorf("Fake: nieoczekiwane polecenie %q", c.String())
	}
	if resp.Err != nil {
		return Result{}, resp.Err
	}
	if c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, resp.Stdout)
	}
	if c.Stderr != nil {
		_, _ = io.WriteString(c.Stderr, resp.Stderr)
	}
	result := Result{Stdout: []byte(resp.Stdout), Stderr: []byte(resp.Stderr)}
	if resp.Code != 0 {
		return result, &ExitError{Cmd: c.String(), Code: resp.Code, Stderr: result.Stderr}
	}
	return result, nil
}
