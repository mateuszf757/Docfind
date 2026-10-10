package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
)

// createCluster tworzy klaster z deploy/k3d/cluster.yaml, z nazwą i portami
// z definicji środowiska, po sprawdzeniu warunków wstępnych maszyny.
func (d Deployer) createCluster(ctx context.Context) error {
	k3sImage := d.Pins.Get(pins.K3sImage)
	pre := &preflight{}
	if err := d.checkEngine(ctx, pre, k3sImage); err != nil {
		return err
	}
	ports := d.Env.Cluster.Ports
	d.checkPorts(pre, map[string]int{"API": ports.API, "HTTP": ports.HTTP, "HTTPS": ports.HTTPS})
	if err := pre.err(); err != nil {
		return err
	}
	config, err := K3dConfig(d.path("deploy/k3d/cluster.yaml"), d.Env)
	if err != nil {
		return err
	}
	// Docker rootless uruchamia węzły w przestrzeni nazw użytkownika
	// (decyzja 19) — kubelet potrzebuje wtedy bramki KubeletInUserNamespace.
	rootless, err := d.Engine.Rootless(ctx)
	if err != nil {
		return err
	}
	if rootless {
		cli.Step("Docker rootless: kubelet z bramką KubeletInUserNamespace")
	}
	logDir := filepath.Join(d.LogDir, fmt.Sprintf("k3d-create-%s-%s", d.Env.Cluster.Name, time.Now().UTC().Format("20060102T150405Z")))
	cli.Step("tworzenie klastra %s (%s), logi węzłów: %s", d.Env.Cluster.Name, k3sImage, logDir)
	return d.Clusters.Create(ctx, cluster.LocalCluster{
		Name: d.Env.Cluster.Name, Config: config, Image: k3sImage, UserNamespace: rootless,
		Timeout: 180 * time.Second, LogDir: logDir,
	})
}

var hostPort = regexp.MustCompile(`^127\.0\.0\.1:([0-9]+):([0-9]+)$`)

// K3dConfig zwraca konfigurację klastra k3d z deploy/k3d/cluster.yaml
// z nazwą i portami z definicji środowiska. Wszystko, co klaster wystawia,
// musi słuchać na 127.0.0.1 (decyzja 17) — konfiguracja z innym adresem
// jest odrzucana, zamiast przepisana po cichu.
func K3dConfig(path string, e environment.Definition) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	meta, _ := cfg["metadata"].(map[string]any)
	api, _ := cfg["kubeAPI"].(map[string]any)
	ports, _ := cfg["ports"].([]any)
	if meta == nil || api == nil || len(ports) == 0 {
		return nil, fmt.Errorf("%s: brak metadata, kubeAPI albo ports", path)
	}
	meta["name"] = e.Cluster.Name
	if api["host"] != "127.0.0.1" || api["hostIP"] != "127.0.0.1" {
		return nil, cli.Unmet("%s: API klastra ma słuchać na 127.0.0.1 (decyzja 17), jest %v/%v", path, api["host"], api["hostIP"])
	}
	api["hostPort"] = strconv.Itoa(e.Cluster.Ports.API)
	for _, raw := range ports {
		port, _ := raw.(map[string]any)
		spec, _ := port["port"].(string)
		m := hostPort.FindStringSubmatch(spec)
		if m == nil {
			return nil, cli.Unmet("%s: mapowanie portu %q spoza postaci 127.0.0.1:HOST:KONTENER (decyzja 17)", path, spec)
		}
		switch m[2] {
		case "80":
			port["port"] = fmt.Sprintf("127.0.0.1:%d:80", e.Cluster.Ports.HTTP)
		case "443":
			port["port"] = fmt.Sprintf("127.0.0.1:%d:443", e.Cluster.Ports.HTTPS)
		default:
			return nil, fmt.Errorf("%s: nieznane mapowanie portu %q", path, spec)
		}
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// writeKubeconfig zapisuje dane dostępowe prosto z k3d do kubeconfigu
// środowiska, przy każdym uruchomieniu — także dla klastra utworzonego
// wcześniej, więc plik nigdy nie wskazuje na klaster, którego już nie ma.
// Globalny ~/.kube/config zostaje nietknięty (decyzja 26).
func (d Deployer) writeKubeconfig(ctx context.Context) error {
	raw, err := d.Clusters.Kubeconfig(ctx, d.Env.Cluster.Name)
	if err != nil {
		return err
	}
	path := d.Env.KubeconfigPath(d.Root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// 0600: w pliku jest certyfikat klienta z uprawnieniami admina klastra.
	return os.WriteFile(path, raw, 0o600)
}

// waitNodes czeka, aż wszystkie węzły są gotowe.
func waitNodes(ctx context.Context, c Cluster) error {
	const timeout = 120 * time.Second
	ready := 0
	err := poll.Until(ctx, timeout, 2*time.Second, func(ctx context.Context) (bool, error) {
		nodes, err := c.Nodes(ctx)
		if err != nil || len(nodes) == 0 {
			return false, nil
		}
		ready = 0
		for _, n := range nodes {
			if n.Ready {
				ready++
			}
		}
		return ready == len(nodes), nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		return cli.Unmet("węzły nie są gotowe po %v", timeout)
	}
	if err != nil {
		return err
	}
	cli.Step("węzły gotowe: %d", ready)
	return nil
}
