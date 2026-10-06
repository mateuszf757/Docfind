//go:build docker

// Warianty negatywne bramki Etapu 1 na prawdziwym obrazie i prawdziwym
// Dockerze. Biegną tam, gdzie Docker jest (lokalnie, zadanie build w CI):
//
//	./bin/mise run test:negatives
//
// Każdy wariant musi zostać odrzucony — bramka, której nie da się oblać,
// niczego nie sprawdza.
package runtimecheck

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/docker"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

func dockerChecker(t *testing.T, name string) *Checker {
	t.Helper()
	image := os.Getenv("DF_TEST_IMAGE")
	if image == "" {
		t.Fatal("DF_TEST_IMAGE nie jest ustawione — uruchom przez ./bin/mise run test:negatives")
	}
	var out bytes.Buffer
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = &out, &out
	t.Cleanup(func() {
		cli.Out, cli.Err = oldOut, oldErr
		if t.Failed() {
			t.Log(out.String())
		}
	})
	limits := DefaultLimits
	// Krótsze limity: warianty negatywne mają się kończyć w sekundach.
	limits.Repeats = 1
	limits.StopTimeout = 3 * time.Second
	limits.StartupTimeout = 10 * time.Second
	limits.RefusalTimeout = 5 * time.Second
	return &Checker{
		Docker:        docker.Client{Runner: proc.Exec{}},
		Image:         image,
		ConfigExample: "../../../deploy/config/app.yml.example",
		Limits:        limits,
		Name:          fmt.Sprintf("dft-test-%s-%d", name, os.Getpid()),
	}
}

func requireUnmet(t *testing.T, rep Report, err error, want string) {
	t.Helper()
	if cli.ExitCode(err) != cli.ExitUnmet {
		t.Fatalf("kod %d, oczekiwano %d (%v)", cli.ExitCode(err), cli.ExitUnmet, err)
	}
	for _, u := range rep.Unmet {
		if strings.Contains(u, want) {
			return
		}
	}
	t.Fatalf("brak niespełnionego warunku z %q wśród: %q", want, rep.Unmet)
}

// Obraz, który bez konfiguracji wstaje i działa, zamiast odmówić startu.
// Dawny skrypt czekałby na niego bez końca.
func TestStartsWithoutConfigIsRejected(t *testing.T) {
	c := dockerChecker(t, "bez-odmowy")
	c.Entrypoint = []string{"sleep", "300"}
	rep, err := c.Run(context.Background())
	requireUnmet(t, rep, err, "brak konfiguracji: kontener wstał i działał")
}

// Powłoka jako PID 1, aplikacja jej dzieckiem: SIGTERM trafia do powłoki,
// która go nie obsługuje, a jądro nie dostarcza PID 1 sygnałów bez handlera.
// `docker stop` czeka cały limit do SIGKILL — klasyczny błąd ENTRYPOINT
// w formie powłoki, przed którym chroni forma exec w Dockerfile.
func TestProcessWithoutSigtermHandlingIsRejected(t *testing.T) {
	c := dockerChecker(t, "bez-sigterm")
	c.Entrypoint = []string{"/bin/sh", "-c", "python -m docfind_api && exit"}
	rep, err := c.Run(context.Background())
	requireUnmet(t, rep, err, "aplikacja zamyka się")
	if rep.Shutdown == nil || rep.Shutdown.OwnSeconds < 2 {
		t.Errorf("koszt własny zamykania %+v — oczekiwano ~limitu docker stop (3 s)", rep.Shutdown)
	}
}

// Żądanie dłuższe niż service.shutdown_grace_seconds (3 s w konfiguracji
// przykładowej): uvicorn po upływie limitu anuluje zadanie, a klient dostaje
// zerwane połączenie zamiast odpowiedzi.
func TestInflightLongerThanGraceIsRejected(t *testing.T) {
	c := dockerChecker(t, "w-locie")
	c.Limits.InflightDelay = 5 * time.Second
	c.Limits.StopTimeout = 10 * time.Second
	rep, err := c.Run(context.Background())
	requireUnmet(t, rep, err, "żądanie w locie przy SIGTERM nie zostało dokończone")
}
