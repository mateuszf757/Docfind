// Package deploy stawia środowisko na klastrze k3d: klaster (jeśli go nie
// ma), komponenty platformy w kolejności zależności, obraz i chart
// aplikacji, a na końcu sprawdza tożsamość na każdym podzie (dawniej
// ci/deploy-local.sh).
package deploy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/mateuszf757/Docfind/tools/internal/build"
	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/docker"
	"github.com/mateuszf757/Docfind/tools/internal/env"
	"github.com/mateuszf757/Docfind/tools/internal/fetch"
	"github.com/mateuszf757/Docfind/tools/internal/kube"
	"github.com/mateuszf757/Docfind/tools/internal/pins"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// Deployer — wdrożenie jednego środowiska.
type Deployer struct {
	Root    string
	Env     env.Environment
	Runner  proc.Runner
	Pins    pins.Pins
	Builder build.Builder
	Service string
	Image   string
}

func (d Deployer) docker() docker.Client { return docker.Client{Runner: d.Runner} }

func (d Deployer) target() kube.Target {
	return kube.Target{Kubeconfig: d.Env.KubeconfigPath(d.Root), Context: d.Env.Cluster.Context, APIServer: d.Env.Cluster.APIServer}
}

func (d Deployer) run(ctx context.Context, name string, args ...string) error {
	_, err := d.Runner.Run(ctx, proc.Cmd{Name: name, Args: args, Stdout: cli.Out, Stderr: cli.Err})
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args[:min(2, len(args))], " "), err)
	}
	return nil
}

// Down usuwa klaster środowiska — tylko k3d, tylko ten z definicji.
func (d Deployer) Down(ctx context.Context) error {
	if d.Env.Cluster.Provider != "k3d" {
		return cli.Unmet("środowisko %s nie jest klastrem k3d — dft go nie usuwa", d.Env.Name)
	}
	if err := d.run(ctx, "k3d", "cluster", "delete", d.Env.Cluster.Name); err != nil {
		return err
	}
	if err := os.Remove(d.Env.KubeconfigPath(d.Root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Up stawia środowisko.
func (d Deployer) Up(ctx context.Context) error {
	if d.Env.Cluster.Provider != "k3d" {
		return cli.Unmet("środowisko %s: wdrożenie przez dft obsługuje tylko klastry k3d", d.Env.Name)
	}
	if d.Env.Artifact.Source != "local" {
		return cli.Unmet("środowisko %s: obraz z rejestru (artifact.source=%s) wdraża zadanie CI, nie dft cluster up", d.Env.Name, d.Env.Artifact.Source)
	}
	p := &preflight{}
	checkInotify(p)
	if err := p.err(); err != nil {
		return err
	}

	_, err := d.Runner.Run(ctx, proc.Cmd{Name: "k3d", Args: []string{"cluster", "get", d.Env.Cluster.Name}})
	switch {
	case err == nil:
		cli.Step("klaster %s istnieje", d.Env.Cluster.Name)
	case proc.ExitCode(err) > 0:
		if err := d.createCluster(ctx); err != nil {
			return err
		}
	default:
		return err
	}

	if err := d.writeKubeconfig(ctx); err != nil {
		return err
	}
	clients, err := kube.Connect(d.target())
	if err != nil {
		return err
	}
	if err := waitNodes(ctx, clients, 120*time.Second); err != nil {
		return err
	}
	if err := d.platform(ctx, clients); err != nil {
		return err
	}
	ref, expected, err := d.image(ctx)
	if err != nil {
		return err
	}
	if err := d.namespace(ctx, clients); err != nil {
		return err
	}
	return d.app(ctx, clients, ref, expected)
}

// createCluster tworzy klaster z deploy/k3d/cluster.yaml, z nazwą i portami
// z definicji środowiska, i przechwytuje logi węzłów: gdy węzeł nie wstanie
// w limicie, k3d wycofuje klaster razem z kontenerami — a więc z jedynym
// dowodem przyczyny.
func (d Deployer) createCluster(ctx context.Context) error {
	pre := &preflight{}
	if err := checkDocker(ctx, d.docker(), pre, d.Pins.Get("DF_K3S_IMAGE")); err != nil {
		return err
	}
	ports := d.Env.Cluster.Ports
	checkPorts(pre, map[string]int{"API": ports.API, "HTTP": ports.HTTP, "HTTPS": ports.HTTPS})
	if err := pre.err(); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "dft-k3d-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	config, err := K3dConfig(filepath.Join(d.Root, "deploy", "k3d", "cluster.yaml"), d.Env)
	if err != nil {
		return err
	}
	configPath := filepath.Join(tmp, "cluster.json")
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		return err
	}

	args := []string{"cluster", "create", "--config", configPath, "--image", d.Pins.Get("DF_K3S_IMAGE"), "--timeout", "180s"}
	// Docker rootless uruchamia węzły w przestrzeni nazw użytkownika, gdzie
	// kubelet nie może zapisywać globalnych sysctli jądra i kończy się
	// „Failed to start ContainerManager". Bramka KubeletInUserNamespace
	// każe mu ten błąd pominąć — tylko przy rootless (decyzja 19).
	if rl, err := rootless(ctx, d.docker()); err != nil {
		return err
	} else if rl {
		args = append(args,
			"--k3s-arg", "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*",
			"--k3s-arg", "--kubelet-arg=feature-gates=KubeletInUserNamespace=true@agent:*")
		cli.Step("Docker rootless: kubelet z bramką KubeletInUserNamespace")
	}

	logDir := filepath.Join(cacheHome(), "docfind", fmt.Sprintf("k3d-create-%s", time.Now().UTC().Format("20060102T150405Z")))
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	cli.Step("tworzenie klastra %s (%s), logi węzłów: %s", d.Env.Cluster.Name, d.Pins.Get("DF_K3S_IMAGE"), logDir)
	watchCtx, stopWatch := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		d.followNodeLogs(watchCtx, logDir)
	}()
	createErr := d.run(ctx, "k3d", args...)
	stopWatch()
	wg.Wait()
	if createErr != nil {
		fmt.Fprintln(cli.Err, "BŁĄD: klaster nie powstał. Błędy z logów węzłów:")
		printNodeErrors(logDir)
		fmt.Fprintf(cli.Err, "Pełne logi: %s\n", logDir)
		return createErr
	}
	return nil
}

// followNodeLogs zapisuje log każdego węzła od chwili, gdy jego kontener
// działa. Tylko działające: `docker logs -f` na kontenerze w stanie Created
// kończy się od razu i węzeł zostałby uznany za obserwowany z pustym logiem.
func (d Deployer) followNodeLogs(ctx context.Context, dir string) {
	followed := map[string]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	for ctx.Err() == nil {
		res, err := d.docker().Run(ctx, "ps", "--format", "{{.Names}}", "--filter", "name=^k3d-"+d.Env.Cluster.Name+"-", "--filter", "status=running")
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
					_, _ = d.Runner.Run(ctx, proc.Cmd{Name: "docker", Args: []string{"logs", "-f", node}, Stdout: f, Stderr: f})
				}()
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(300 * time.Millisecond):
		}
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

func cacheHome() string {
	if dir := os.Getenv("XDG_CACHE_HOME"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache")
}

var hostPort = regexp.MustCompile(`^127\.0\.0\.1:([0-9]+):([0-9]+)$`)

// K3dConfig zwraca konfigurację klastra k3d z deploy/k3d/cluster.yaml
// z nazwą i portami z definicji środowiska. Wszystko, co klaster wystawia,
// musi słuchać na 127.0.0.1 (decyzja 17) — konfiguracja z innym adresem
// jest odrzucana, zamiast przepisana po cichu.
func K3dConfig(path string, e env.Environment) ([]byte, error) {
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
	res, err := d.Runner.Run(ctx, proc.Cmd{Name: "k3d", Args: []string{"kubeconfig", "get", d.Env.Cluster.Name}})
	if err != nil {
		return fmt.Errorf("k3d kubeconfig get: %w", err)
	}
	path := d.Env.KubeconfigPath(d.Root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// 0600: w pliku jest certyfikat klienta z uprawnieniami admina klastra.
	return os.WriteFile(path, res.Stdout, 0o600)
}

func waitNodes(ctx context.Context, c *kube.Clients, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		nodes, err := c.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err == nil && len(nodes.Items) > 0 {
			ready := 0
			for _, n := range nodes.Items {
				for _, cond := range n.Status.Conditions {
					if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
						ready++
					}
				}
			}
			if ready == len(nodes.Items) {
				cli.Step("węzły gotowe: %d", ready)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return cli.Unmet("węzły nie są gotowe po %v", timeout)
		}
		if err := sleep(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

// platform instaluje komponenty platformy w kolejności wynikającej z tego,
// czego każdy potrzebuje przy starcie, a nie z wygody: CoreDNS przed
// wszystkim (bez niego żaden pod nie rozwiąże nazwy), Envoy Gateway przed
// cert-managerem (jego chart instaluje CRD Gateway API, a cert-manager
// z obsługą Gateway API bez nich nie czeka, tylko wpada w CrashLoopBackOff),
// na końcu chart platformy, który tworzy zasoby obu.
func (d Deployer) platform(ctx context.Context, c *kube.Clients) error {
	kubeArgs := d.target().HelmArgs()
	helm := func(args ...string) error {
		return d.run(ctx, "helm", append(append([]string{"upgrade", "--install"}, args...), kubeArgs...)...)
	}
	p := d.Pins
	coredns, err := fetch.URL(ctx, d.Root, p.Get("DF_COREDNS_CHART_URL"), p.Get("DF_COREDNS_CHART_SHA256"))
	if err != nil {
		return err
	}
	cli.Step("CoreDNS: chart %s (suma zweryfikowana)", p.Get("DF_COREDNS_CHART_VERSION"))
	if err := helm("coredns", coredns, "--namespace", "kube-system", "--values", d.path("deploy/platform/coredns/values.yaml"), "--wait", "--timeout", "3m"); err != nil {
		return err
	}
	envoy, err := fetch.OCIChart(ctx, d.Runner, d.Root, p.Get("DF_ENVOY_GATEWAY_CHART_REF"), p.Get("DF_ENVOY_GATEWAY_CHART_VERSION"), p.Get("DF_ENVOY_GATEWAY_CHART_SHA256"))
	if err != nil {
		return err
	}
	cli.Step("Envoy Gateway: chart %s (suma zweryfikowana)", p.Get("DF_ENVOY_GATEWAY_CHART_VERSION"))
	if err := helm("envoy-gateway", envoy, "--namespace", "envoy-gateway-system", "--create-namespace", "--values", d.path("deploy/platform/envoy-gateway/values.yaml"), "--wait", "--timeout", "5m"); err != nil {
		return err
	}
	// startupapicheck w charcie sprawia, że --wait kończy się dopiero wtedy,
	// gdy webhook cert-managera naprawdę przyjmuje zapisy.
	certManager, err := fetch.URL(ctx, d.Root, p.Get("DF_CERT_MANAGER_CHART_URL"), p.Get("DF_CERT_MANAGER_CHART_SHA256"))
	if err != nil {
		return err
	}
	cli.Step("cert-manager: chart %s (suma zweryfikowana)", p.Get("DF_CERT_MANAGER_CHART_VERSION"))
	if err := helm("cert-manager", certManager, "--namespace", "cert-manager", "--create-namespace", "--values", d.path("deploy/platform/cert-manager/values.yaml"), "--wait", "--timeout", "5m"); err != nil {
		return err
	}

	gw := d.Env.Gateway
	// Let's Encrypt wymaga tokenu Cloudflare w klastrze. Bez niego wydawca by
	// powstał, a wyzwanie DNS-01 wisiałoby bez końca z błędem tylko w
	// statusie Challenge.
	if strings.HasPrefix(gw.Issuer, "letsencrypt") {
		if _, err := c.Core.CoreV1().Secrets("cert-manager").Get(ctx, "cloudflare-api-token", metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return cli.Unmet("wydawca %s wymaga tokenu Cloudflare w klastrze — ./bin/mise run dns-token", gw.Issuer)
		} else if err != nil {
			return err
		}
	}
	cli.Step("platforma: Gateway dla %s, wydawca %s", gw.Hostname, gw.Issuer)
	if err := helm("platform", d.path("deploy/charts/platform"), "--namespace", gw.Namespace, "--create-namespace",
		"--set", "gateway.hostname="+gw.Hostname, "--set", "gateway.issuer="+gw.Issuer,
		// Port przekierowania HTTP→HTTPS: na hoście HTTPS słucha tu, a nie
		// na 443 (decyzja 17).
		"--set", fmt.Sprintf("gateway.httpsRedirectPort=%d", d.Env.Cluster.Ports.HTTPS),
		"--set", "acme.email="+gw.ACMEEmail, "--set", "acme.dnsZone="+gw.ACMEZone,
		"--wait", "--timeout", "3m"); err != nil {
		return err
	}
	if err := waitCondition(ctx, c, schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}, "cert-manager", "docfind-root-ca", "Ready", 120*time.Second); err != nil {
		return err
	}
	if err := waitCondition(ctx, c, schema.GroupVersionResource{Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways"}, gw.Namespace, gw.Name, "Programmed", 180*time.Second); err != nil {
		return err
	}
	cli.Step("Gateway zaprogramowany")
	return nil
}

func (d Deployer) path(rel string) string { return filepath.Join(d.Root, rel) }

// image buduje obraz z drzewa roboczego i importuje go do węzłów.
//
// Tag wyliczony z zawartości obrazu, nie z wersji. Build z brudnego drzewa
// dostaje za każdym razem ten sam tag „…-dirty"; przy IfNotPresent
// i niezmienionej specyfikacji poda Helm nie zrobiłby rolloutu, a na
// klastrze dalej chodziłby stary kod pod nową nazwą.
func (d Deployer) image(ctx context.Context) (string, kube.Expected, error) {
	res, err := d.Builder.Build(ctx, build.Options{Root: d.Root, Service: d.Service, Image: d.Image, Out: cli.Out, Err: cli.Err})
	if err != nil {
		return "", kube.Expected{}, err
	}
	out, err := d.docker().Run(ctx, "image", "inspect", res.Ref(), "--format", "{{.Id}}")
	if err != nil {
		return "", kube.Expected{}, err
	}
	id := strings.TrimPrefix(strings.TrimSpace(string(out.Stdout)), "sha256:")
	if len(id) < 12 {
		return "", kube.Expected{}, fmt.Errorf("nieczytelny identyfikator obrazu %q", id)
	}
	ref := fmt.Sprintf("%s:local-%s", d.Image, id[:12])
	if _, err := d.docker().Run(ctx, "tag", res.Ref(), ref); err != nil {
		return "", kube.Expected{}, err
	}
	// Tryb direct wgrywa obraz prosto do containerd na każdym węźle. Domyślny
	// tryb uruchamia pomocniczy kontener z zamontowanym gniazdem Dockera —
	// pełna kontrola nad demonem, a przy rootless i tak zawodzi.
	cli.Step("import %s do węzłów", ref)
	if _, err := d.Runner.Run(ctx, proc.Cmd{Name: "k3d", Args: []string{"image", "import", ref, "--cluster", d.Env.Cluster.Name, "--mode", "direct"}}); err != nil {
		return "", kube.Expected{}, fmt.Errorf("k3d image import: %w", err)
	}
	return ref, kube.Expected{Image: ref, Version: res.Version, Commit: res.Commit}, nil
}

// namespace zakłada przestrzeń nazw aplikacji z etykietami, które są zgodą
// platformy, a nie aplikacji samej sobie: trasa może podpiąć się pod
// listener HTTPS (gateway.routeNamespaceLabel) i Pod Security restricted
// (decyzja 31) z wersją profilu przypiętą do wersji klastra.
func (d Deployer) namespace(ctx context.Context, c *kube.Clients) error {
	psa, err := kube.PSAVersion(d.Pins.Get("DF_KUBERNETES_VERSION"))
	if err != nil {
		return err
	}
	labels := map[string]string{
		"docfind.io/gateway-routes":                  "allowed",
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/enforce-version": psa,
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/warn-version":    psa,
	}
	ns := d.Env.App.Namespace
	_, err = c.Core.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.Core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: labels}}, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return err
	}
	_, err = c.Core.CoreV1().Namespaces().Patch(ctx, ns, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (d Deployer) app(ctx context.Context, c *kube.Clients, ref string, want kube.Expected) error {
	tag := ref[strings.LastIndex(ref, ":")+1:]
	release := d.Env.App.Release
	cli.Step("helm upgrade --install %s", release)
	args := []string{"upgrade", "--install", release, d.path("deploy/charts/docfind"),
		"--namespace", d.Env.App.Namespace,
		"--set", "route.hostname=" + d.Env.Gateway.Hostname,
		"--set", "api.image.tag=" + tag,
		"--wait", "--timeout", "3m"}
	if err := d.run(ctx, "helm", append(args, d.target().HelmArgs()...)...); err != nil {
		return err
	}
	// To, co stoi na klastrze, musi się zgadzać z gitem — sprawdzane na
	// każdym podzie, a liczba podów z liczbą replik.
	if _, err := kube.VerifyDeployment(ctx, c.Core, d.Env.App.Namespace, release+"-api", "api", want); err != nil {
		return err
	}
	return nil
}

func waitCondition(ctx context.Context, c *kube.Clients, gvr schema.GroupVersionResource, ns, name, condition string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		obj, err := c.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
			if slices.ContainsFunc(conditions, func(raw any) bool {
				m, ok := raw.(map[string]any)
				return ok && m["type"] == condition && m["status"] == "True"
			}) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return cli.Unmet("%s %s/%s nie ma warunku %s po %v", gvr.Resource, ns, name, condition, timeout)
		}
		if err := sleep(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
