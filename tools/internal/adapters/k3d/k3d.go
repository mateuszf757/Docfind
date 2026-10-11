// Package k3d to adapter klastrów k3d: lokalnych klastrów k3s, których
// węzły to kontenery Dockera na tym hoście (decyzje 17 i 19). Jak rozpoznać
// kontenery węzłów klastra, wie tylko ten pakiet.
package k3d

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// K3d — program k3d (i docker dla kontenerów węzłów).
type K3d struct {
	Runner proc.Runner
}

func (k K3d) stream(ctx context.Context, args ...string) error {
	_, err := k.Runner.Run(ctx, proc.Cmd{Name: "k3d", Args: args, Stdout: cli.Out, Stderr: cli.Err})
	if err != nil {
		return fmt.Errorf("k3d %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return nil
}

// Exists mówi, czy klaster istnieje.
func (k K3d) Exists(ctx context.Context, name string) (bool, error) {
	_, err := k.Runner.Run(ctx, proc.Cmd{Name: "k3d", Args: []string{"cluster", "get", name}})
	switch {
	case err == nil:
		return true, nil
	case proc.ExitCode(err) > 0:
		return false, nil
	default:
		return false, err
	}
}

// Create tworzy klaster z konfiguracji k3d i przechwytuje logi węzłów do
// c.LogDir: gdy węzeł nie wstanie w limicie, k3d wycofuje klaster razem
// z kontenerami — a więc z jedynym dowodem przyczyny. Po porażce wypisuje
// błędy z logów węzłów.
func (k K3d) Create(ctx context.Context, c cluster.LocalCluster) error {
	tmp, err := os.MkdirTemp("", "dft-k3d-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	configPath := filepath.Join(tmp, "cluster.json")
	if err := os.WriteFile(configPath, c.Config, 0o644); err != nil {
		return err
	}
	args := []string{"cluster", "create", "--config", configPath, "--image", c.Image, "--timeout", c.Timeout.String()}
	if c.UserNamespace {
		// Docker rootless uruchamia węzły w przestrzeni nazw użytkownika, gdzie
		// kubelet nie może zapisywać globalnych sysctli jądra i kończy się
		// „Failed to start ContainerManager". Bramka KubeletInUserNamespace
		// każe mu ten błąd pominąć (decyzja 19).
		args = append(args,
			"--k3s-arg", "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*",
			"--k3s-arg", "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@agent:*")
	}
	if err := os.MkdirAll(c.LogDir, 0o755); err != nil {
		return err
	}
	watchCtx, stopWatch := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		k.followNodeLogs(watchCtx, c.Name, c.LogDir)
	}()
	createErr := k.stream(ctx, args...)
	stopWatch()
	wg.Wait()
	if createErr != nil {
		fmt.Fprintln(cli.Err, "BŁĄD: klaster nie powstał. Błędy z logów węzłów:")
		printNodeErrors(c.LogDir)
		fmt.Fprintf(cli.Err, "Pełne logi: %s\n", c.LogDir)
		return createErr
	}
	return nil
}

// nodeFilter wybiera kontenery węzłów klastra po etykiecie, którą nadaje im
// k3d. Nie po przedrostku nazwy: kontenery klastra docfind-staging
// (k3d-docfind-staging-…) zaczynają się tak samo jak klastra docfind
// (k3d-docfind-…), więc dowody z dev zbierały logi węzłów stagingu.
func nodeFilter(name string) string { return "label=k3d.cluster=" + name }

// followNodeLogs zapisuje log każdego węzła od chwili, gdy jego kontener
// działa. Tylko działające: `docker logs -f` na kontenerze w stanie Created
// kończy się od razu i węzeł zostałby uznany za obserwowany z pustym logiem.
func (k K3d) followNodeLogs(ctx context.Context, name, dir string) {
	followed := map[string]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	for ctx.Err() == nil {
		res, err := k.Runner.Run(ctx, proc.Cmd{Name: "docker", Args: []string{"ps", "--format", "{{.Names}}", "--filter", nodeFilter(name), "--filter", "status=running"}})
		if err == nil {
			for _, node := range strings.Fields(string(res.Stdout)) {
				if followed[node] {
					continue
				}
				followed[node] = true
				f, err := os.Create(filepath.Join(dir, node+".log"))
				if err != nil {
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer f.Close()
					_, _ = k.Runner.Run(ctx, proc.Cmd{Name: "docker", Args: []string{"logs", "-f", node}, Stdout: f, Stderr: f})
				}()
			}
		}
		_ = poll.Sleep(ctx, 300*time.Millisecond)
	}
}

var nodeError = regexp.MustCompile(`level=(fatal|error)|^E[0-9]{4} `)

func printNodeErrors(dir string) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	for _, file := range files {
		f, err := os.Open(file)
		if err != nil {
			continue
		}
		var lines []string
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			if line := scanner.Text(); nodeError.MatchString(line) && !strings.Contains(line, "connection to the server") {
				lines = append(lines, line)
			}
		}
		_ = f.Close()
		if len(lines) > 8 {
			lines = lines[len(lines)-8:]
		}
		fmt.Fprintf(cli.Err, "--- %s\n%s\n", strings.TrimSuffix(filepath.Base(file), ".log"), strings.Join(lines, "\n"))
	}
}

// Delete usuwa klaster.
func (k K3d) Delete(ctx context.Context, name string) error {
	return k.stream(ctx, "cluster", "delete", name)
}

// Kubeconfig zwraca dane dostępowe klastra prosto z k3d.
func (k K3d) Kubeconfig(ctx context.Context, name string) ([]byte, error) {
	res, err := k.Runner.Run(ctx, proc.Cmd{Name: "k3d", Args: []string{"kubeconfig", "get", name}})
	if err != nil {
		return nil, fmt.Errorf("k3d kubeconfig get: %w", err)
	}
	return res.Stdout, nil
}

// Import wgrywa obraz (z demona Dockera albo archiwum) do węzłów klastra.
// Tryb direct wgrywa go prosto do containerd na każdym węźle; domyślny tryb
// uruchamia pomocniczy kontener z zamontowanym gniazdem Dockera — pełna
// kontrola nad demonem, a przy rootless i tak zawodzi (decyzja 19).
func (k K3d) Import(ctx context.Context, name, image string) error {
	if _, err := k.Runner.Run(ctx, proc.Cmd{Name: "k3d", Args: []string{"image", "import", image, "--cluster", name, "--mode", "direct"}}); err != nil {
		return fmt.Errorf("k3d image import %s do %s: %w", image, name, err)
	}
	return nil
}

// NodeLogs zapisuje logi kontenerów węzłów (także zatrzymanych) do
// dir/<węzeł>.log i zwraca listę tego, czego nie udało się zebrać.
func (k K3d) NodeLogs(ctx context.Context, name, dir string) []string {
	res, err := k.Runner.Run(ctx, proc.Cmd{Name: "docker", Args: []string{"ps", "-a", "--format", "{{.Names}}", "--filter", nodeFilter(name)}})
	if err != nil {
		return []string{fmt.Sprintf("kontenery węzłów: %v", err)}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return []string{fmt.Sprintf("kontenery węzłów: %v", err)}
	}
	var missing []string
	for _, node := range strings.Fields(string(res.Stdout)) {
		if err := k.logTo(ctx, filepath.Join(dir, node+".log"), node); err != nil {
			missing = append(missing, fmt.Sprintf("%s: %v", node, err))
		}
	}
	return missing
}

func (k K3d) logTo(ctx context.Context, path, node string) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = k.Runner.Run(ctx, proc.Cmd{Name: "docker", Args: []string{"logs", "--timestamps", node}, Stdout: f, Stderr: f})
	return err
}
