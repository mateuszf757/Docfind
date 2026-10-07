package proc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExecRun(t *testing.T) {
	ctx := context.Background()

	res, err := Exec{}.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "cat; echo błąd >&2"}, Stdin: strings.NewReader("wejście")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(res.Stdout) != "wejście" || strings.TrimSpace(string(res.Stderr)) != "błąd" {
		t.Errorf("stdout %q, stderr %q", res.Stdout, res.Stderr)
	}

	_, err = Exec{}.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "echo powód >&2; exit 3"}})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 3 {
		t.Fatalf("oczekiwano ExitError z kodem 3, jest %v", err)
	}
	if ExitCode(err) != 3 || !strings.Contains(err.Error(), "powód") {
		t.Errorf("ExitCode %d, komunikat %q", ExitCode(err), err)
	}

	// Program, którego nie ma, to awaria uruchomienia — nie wynik programu.
	_, err = Exec{}.Run(ctx, Cmd{Name: "nie-ma-takiego-programu-dft"})
	if err == nil || errors.As(err, &exitErr) {
		t.Errorf("oczekiwano błędu uruchomienia, jest %v", err)
	}
}

// TestExecCancel: anulowanie kontekstu (SIGINT do dft) kończy dziecko
// SIGTERM-em, a Run wraca z błędem kontekstu, a nie z ExitError — przerwanie
// nie może wyglądać jak wynik programu.
func TestExecCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	_, err := Exec{}.Run(ctx, Cmd{Name: "sleep", Args: []string{"30"}})
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Run wrócił po %v — dziecko nie dostało sygnału", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("oczekiwano context.Canceled, jest %v", err)
	}
	var exitErr *ExitError
	if errors.As(err, &exitErr) {
		t.Errorf("przerwanie zgłoszone jako ExitError: %v", err)
	}
}

func TestFake(t *testing.T) {
	f := &Fake{Responses: map[string]FakeResponse{
		"docker version": {Stdout: "29.8.1\n"},
		"docker stop x":  {Code: 1, Stderr: "No such container"},
	}}
	out, err := Output(context.Background(), f, Cmd{Name: "docker", Args: []string{"version"}})
	if err != nil || out != "29.8.1" {
		t.Errorf("Output = %q, %v", out, err)
	}
	if _, err := f.Run(context.Background(), Cmd{Name: "docker", Args: []string{"stop", "x"}}); ExitCode(err) != 1 {
		t.Errorf("oczekiwano kodu 1, jest %v", err)
	}
	if _, err := f.Run(context.Background(), Cmd{Name: "rm", Args: []string{"-rf", "/"}}); err == nil {
		t.Error("nieprzewidziane polecenie przeszło")
	}
	if len(f.Calls) != 3 {
		t.Errorf("zapisane wywołania: %d", len(f.Calls))
	}
}
