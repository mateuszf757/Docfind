// Package docker to adapter lokalnego silnika Dockera i BuildKitu: build
// obrazu, archiwa `docker save`, kontenery bramek i właściwości demona.
// Rejestr obrazów to osobny adapter (adapters/registry).
//
// CLI, a nie SDK: client.FromEnv z SDK czyta DOCKER_HOST, a nie aktywny
// kontekst CLI (`docker context use`). Na maszynie autora aktywny jest
// kontekst „rootless", a domyślne gniazdo /var/run/docker.sock nie istnieje —
// SDK łączyłby się donikąd. Budowanie i tak idzie przez buildx, którego SDK
// nie obejmuje.
package docker

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Client to Docker CLI uruchamiany przez Runner.
type Client struct {
	Runner proc.Runner
}

func (c Client) output(ctx context.Context, args ...string) (string, error) {
	return proc.Output(ctx, c.Runner, proc.Cmd{Name: "docker", Args: args})
}

func (c Client) run(ctx context.Context, args ...string) (proc.Result, error) {
	return c.Runner.Run(ctx, proc.Cmd{Name: "docker", Args: args})
}

// ImageUser zwraca użytkownika obrazu (USER z konfiguracji); pusty —
// root, jak domyślnie w Dockerze.
func (c Client) ImageUser(ctx context.Context, ref string) (string, error) {
	out, err := c.output(ctx, "image", "inspect", ref, "--format", "{{.Config.User}}")
	if err != nil {
		return "", fmt.Errorf("użytkownik obrazu %s: %w", ref, err)
	}
	return out, nil
}

// ReadFiles zwraca treść plików z obrazu, sklejoną jak w `cat a b`.
// Przez `cat` z obrazu, a nie powłokę: nic w obrazie nie interpretuje
// skryptu, a wynik parsuje Go.
func (c Client) ReadFiles(ctx context.Context, ref string, paths ...string) ([]byte, error) {
	args := append([]string{"run", "--rm", "--network", "none", "--entrypoint", "cat", ref}, paths...)
	res, err := c.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("odczyt %s z obrazu %s: %w", strings.Join(paths, ", "), ref, err)
	}
	return res.Stdout, nil
}

// Save zapisuje obraz do archiwum (`docker save`).
func (c Client) Save(ctx context.Context, ref, path string) error {
	if _, err := c.run(ctx, "save", "-o", path, ref); err != nil {
		return fmt.Errorf("docker save %s: %w", ref, err)
	}
	return nil
}

// Builder opisuje bieżący builder buildx.
type Builder struct {
	Name   string
	Driver string
	// BuildKitVersion — np. "v0.33.0"; pusty, gdy buildx go nie podał.
	BuildKitVersion string
}

// CurrentBuilder pyta buildx o bieżący builder.
func (c Client) CurrentBuilder(ctx context.Context) (Builder, error) {
	out, err := c.output(ctx, "buildx", "inspect")
	if err != nil {
		return Builder{}, fmt.Errorf("docker buildx inspect: %w", err)
	}
	b := ParseBuildxInspect(out)
	if b.Driver == "" {
		return Builder{}, fmt.Errorf("docker buildx inspect: brak pola Driver w wyjściu")
	}
	return b, nil
}

// BuildKitVersion — wersja BuildKitu bieżącego buildera; pusta, gdy buildx
// jej nie podał.
func (c Client) BuildKitVersion(ctx context.Context) (string, error) {
	b, err := c.CurrentBuilder(ctx)
	return b.BuildKitVersion, err
}

// ParseBuildxInspect czyta wyjście `docker buildx inspect`. Bierze pierwsze
// wystąpienie każdego pola — sekcja Nodes powtarza Name dla węzła.
//
// Całe wyjście jest już w pamięci, zanim zacznie się parsowanie. W Bashu
// `buildx inspect | awk '… exit'` przerywał build bez komunikatu: awk kończył
// się po pierwszym dopasowaniu, buildx dostawał SIGPIPE, a pipefail uznawał
// potok za nieudany.
func ParseBuildxInspect(out string) Builder {
	var b Builder
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "Name":
			if b.Name == "" {
				b.Name = value
			}
		case "Driver":
			if b.Driver == "" {
				b.Driver = value
			}
		case "BuildKit version":
			if b.BuildKitVersion == "" {
				b.BuildKitVersion = value
			}
		}
	}
	return b
}
