package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/container"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// runArgs zamienia specyfikację kontenera na argumenty `docker run` po
// słowie run (i fladze -d albo --rm, które dokłada wywołujący).
func runArgs(s container.Spec) []string {
	var args []string
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	if s.Init {
		args = append(args, "--init")
	}
	if s.NoNetwork {
		args = append(args, "--network", "none")
	}
	for _, m := range s.Mounts {
		args = append(args, "-v", m.Source+":"+m.Target+":ro")
	}
	for _, p := range s.Publish {
		// Port hosta przydziela Docker, zawsze na 127.0.0.1 (decyzja 17).
		args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d", p))
	}
	if len(s.Entrypoint) > 0 {
		args = append(args, "--entrypoint", s.Entrypoint[0])
	}
	args = append(args, s.Image)
	if len(s.Entrypoint) > 1 {
		args = append(args, s.Entrypoint[1:]...)
	}
	return args
}

// RunToExit uruchamia kontener (--rm) i czeka na jego zakończenie. Kod
// wyjścia różny od zera to wynik (Exit.Code), a nie błąd; błąd to tylko
// niemożność uruchomienia.
//
// Po timeout (>0) usuwany jest sam kontener, a nie przerywany `docker run`:
// CLI przekazuje sygnał do kontenera, a PID 1 bez handlera go ignoruje, więc
// przerwany klient czekałby na swój własny limit. Usunięcie kontenera
// kończy `docker run` od razu. Wymaga nazwy kontenera.
func (c Client) RunToExit(ctx context.Context, s container.Spec, timeout time.Duration) (container.Exit, error) {
	if timeout > 0 && s.Name == "" {
		return container.Exit{}, errors.New("kontener z limitem czasu wymaga nazwy")
	}
	var timedOut atomic.Bool
	if timeout > 0 {
		timer := time.AfterFunc(timeout, func() {
			timedOut.Store(true)
			cleanupCtx, cancel := cli.CleanupContext(30 * time.Second)
			defer cancel()
			_, _ = c.run(cleanupCtx, "rm", "--force", s.Name)
		})
		defer timer.Stop()
	}
	res, err := c.run(ctx, append([]string{"run", "--rm"}, runArgs(s)...)...)
	exit := container.Exit{Stdout: string(res.Stdout), Stderr: string(res.Stderr)}
	switch {
	case timedOut.Load():
		exit.TimedOut = true
		exit.Code = -1
	case err == nil:
	case proc.ExitCode(err) > 0:
		exit.Code = proc.ExitCode(err)
	default:
		return exit, err
	}
	return exit, nil
}

// Start uruchamia kontener w tle (-d --rm).
func (c Client) Start(ctx context.Context, s container.Spec) error {
	if _, err := c.run(ctx, append([]string{"run", "-d", "--rm"}, runArgs(s)...)...); err != nil {
		return fmt.Errorf("start kontenera %s: %w", s.Name, err)
	}
	return nil
}

// Stop zatrzymuje kontener: SIGTERM, a po timeout SIGKILL.
func (c Client) Stop(ctx context.Context, name string, timeout time.Duration) error {
	seconds := strconv.Itoa(int(timeout.Round(time.Second) / time.Second))
	if _, err := c.run(ctx, "stop", "--timeout", seconds, name); err != nil {
		return fmt.Errorf("docker stop %s: %w", name, err)
	}
	return nil
}

// HostPort zwraca port hosta na 127.0.0.1, pod którym kontener wystawia
// port TCP containerPort.
func (c Client) HostPort(ctx context.Context, name string, containerPort int) (int, error) {
	res, err := c.run(ctx, "port", name, fmt.Sprintf("%d/tcp", containerPort))
	if err != nil {
		return 0, fmt.Errorf("docker port %s: %w", name, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		host, port, err := net.SplitHostPort(strings.TrimSpace(line))
		if err != nil || host != "127.0.0.1" {
			continue
		}
		if n, err := strconv.Atoi(port); err == nil {
			return n, nil
		}
	}
	return 0, fmt.Errorf("docker port %s: brak portu na 127.0.0.1 w %q", name, res.Stdout)
}

// Remove usuwa kontenery (także działające). Kontener, którego już nie ma
// (--rm), to nie błąd sprzątania.
func (c Client) Remove(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	res, err := c.run(ctx, append([]string{"rm", "--force"}, names...)...)
	if err != nil && !bytes.Contains(res.Stderr, []byte("No such container")) {
		return fmt.Errorf("sprzątanie kontenerów: %w", err)
	}
	return nil
}

// CgroupVersion zwraca wersję cgroup, na której działa demon.
func (c Client) CgroupVersion(ctx context.Context) (string, error) {
	out, err := c.output(ctx, "info", "--format", "{{.CgroupVersion}}")
	if err != nil {
		return "", fmt.Errorf("docker info: %w", err)
	}
	return out, nil
}

// Rootless mówi, czy demon działa bez roota.
func (c Client) Rootless(ctx context.Context) (bool, error) {
	out, err := c.output(ctx, "info", "--format", "{{.SecurityOptions}}")
	if err != nil {
		return false, fmt.Errorf("docker info: %w", err)
	}
	return strings.Contains(out, "name=rootless"), nil
}

// ImageID zwraca identyfikator obrazu w magazynie demona (bez sha256:).
func (c Client) ImageID(ctx context.Context, ref string) (string, error) {
	out, err := c.output(ctx, "image", "inspect", ref, "--format", "{{.Id}}")
	if err != nil {
		return "", err
	}
	id := strings.TrimPrefix(out, "sha256:")
	if len(id) < 12 {
		return "", fmt.Errorf("nieczytelny identyfikator obrazu %q", out)
	}
	return id, nil
}

// Tag nadaje obrazowi dodatkowy tag.
func (c Client) Tag(ctx context.Context, src, dst string) error {
	_, err := c.run(ctx, "tag", src, dst)
	return err
}
