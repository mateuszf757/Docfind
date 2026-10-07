// Package kube łączy się z klastrem środowiska i pilnuje, żeby to był ten
// klaster: jawny kontekst z definicji środowiska, a nie bieżący z powłoki.
package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Target — klaster, z którym bramka ma rozmawiać.
type Target struct {
	// Kubeconfig — ścieżka pliku; nigdy KUBECONFIG z powłoki ani
	// ~/.kube/config (decyzja 26).
	Kubeconfig string
	// Context — nazwa kontekstu z definicji środowiska.
	Context string
	// APIServer — adres serwera, który ten kontekst musi wskazywać.
	APIServer string
}

// Clients — klienci jednego klastra.
type Clients struct {
	Core    kubernetes.Interface
	Dynamic dynamic.Interface
	Config  *rest.Config
	Target  Target
}

// Guard sprawdza kubeconfig, zanim cokolwiek trafi do klastra:
//
//   - bieżący kontekst to kontekst środowiska — skrypty sprawdzały to przed
//     drainem; kontekst przełączony ręcznie w kubeconfigu projektu znaczy, że
//     ktoś pracuje na innym klastrze tym samym plikiem;
//   - kontekst wskazuje na serwer z definicji — kontekst o tej samej nazwie,
//     ale z innym adresem (skopiowany kubeconfig, inny klaster po
//     odtworzeniu) przeszedłby samo porównanie nazw.
//
// Klienci i tak łączą się przez jawnie podany kontekst, więc bieżący
// kontekst nie decyduje, dokąd trafią żądania — strażnik jest drugą warstwą.
// Niezgodność to warunek niespełniony (kod 1): bramka odmawia, zamiast
// działać na cudzym klastrze.
func Guard(t Target) error {
	raw, err := (&clientcmd.ClientConfigLoadingRules{ExplicitPath: t.Kubeconfig}).Load()
	if err != nil {
		return fmt.Errorf("kubeconfig %s: %w", t.Kubeconfig, err)
	}
	if raw.CurrentContext != t.Context {
		return cli.Unmet("bieżący kontekst w %s to %q, oczekiwano %q — nie działam na cudzym klastrze", t.Kubeconfig, raw.CurrentContext, t.Context)
	}
	kctx, ok := raw.Contexts[t.Context]
	if !ok {
		return cli.Unmet("kubeconfig %s nie ma kontekstu %q", t.Kubeconfig, t.Context)
	}
	cluster, ok := raw.Clusters[kctx.Cluster]
	if !ok {
		return cli.Unmet("kontekst %q wskazuje na nieistniejący klaster %q", t.Context, kctx.Cluster)
	}
	if cluster.Server != t.APIServer {
		return cli.Unmet("kontekst %q wskazuje na %s, a środowisko na %s — nie działam na cudzym klastrze", t.Context, cluster.Server, t.APIServer)
	}
	return nil
}

// Connect sprawdza strażnika i tworzy klientów dla jawnie podanego kontekstu.
func Connect(t Target) (*Clients, error) {
	if err := Guard(t); err != nil {
		return nil, err
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: t.Kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: t.Context},
	)
	config, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s, kontekst %s: %w", t.Kubeconfig, t.Context, err)
	}
	core, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Clients{Core: core, Dynamic: dyn, Config: config, Target: t}, nil
}

// KubectlArgs zwraca argumenty, które kierują kubectl i cmctl na ten sam
// klaster co klientów — jawnie, bez polegania na KUBECONFIG z powłoki.
func (t Target) KubectlArgs() []string {
	return []string{"--kubeconfig", t.Kubeconfig, "--context", t.Context}
}

// HelmArgs — to samo dla Helma, który kontekst nazywa --kube-context.
func (t Target) HelmArgs() []string {
	return []string{"--kubeconfig", t.Kubeconfig, "--kube-context", t.Context}
}

// Identity — co pod mówi o sobie na /version.
type Identity struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Expected — tożsamość, której oczekujemy na każdym podzie.
type Expected struct {
	Image   string
	Version string
	Commit  string
}

// PodIdentity — wynik sprawdzenia jednego poda.
type PodIdentity struct {
	Pod      string   `json:"pod"`
	Node     string   `json:"node"`
	Image    string   `json:"image"`
	Reported Identity `json:"reported"`
	Problem  string   `json:"problem,omitempty"`
}

// VerifyDeployment sprawdza, że na klastrze działa dokładnie ten obraz,
// który zbudowano z tego drzewa: obraz, wersja i commit na każdym podzie,
// a liczba sprawdzonych podów równa liczbie replik (warunek Etapu 9 w
// wykonywalnej postaci).
//
// /version czytany przez proxy API servera do poda, a nie `kubectl exec`
// z Pythonem w kontenerze: nie zależy od zawartości obrazu (powłoki,
// interpretera) i trafia dokładnie w wskazany pod, nie w Service.
//
// Pody wygaszane po rolloutcie (z deletionTimestamp) jeszcze odpowiadają
// starą wersją przez preStop, więc są pomijane.
func VerifyDeployment(ctx context.Context, cs kubernetes.Interface, namespace, deployment, container string, want Expected) ([]PodIdentity, error) {
	dep, err := cs.AppsV1().Deployments(namespace).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("odczyt Deploymentu %s/%s: %w", namespace, deployment, err)
	}
	if dep.Spec.Replicas == nil || dep.Spec.Selector == nil {
		return nil, fmt.Errorf("deployment %s/%s bez replik albo selektora", namespace, deployment)
	}
	selector := labels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String()
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("pody %s: %w", selector, err)
	}

	var results []PodIdentity
	v := &cli.Verdict{}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		r := PodIdentity{Pod: pod.Name, Node: pod.Spec.NodeName}
		for _, c := range pod.Spec.Containers {
			if c.Name == container {
				r.Image = c.Image
			}
		}
		switch {
		case r.Image != want.Image:
			r.Problem = fmt.Sprintf("uruchamia %s, oczekiwano %s", r.Image, want.Image)
		default:
			raw, err := cs.CoreV1().Pods(namespace).ProxyGet("http", pod.Name, "8000", "/version", nil).DoRaw(ctx)
			if err != nil {
				r.Problem = fmt.Sprintf("nie odpowiada na /version: %v", err)
				break
			}
			if err := json.Unmarshal(raw, &r.Reported); err != nil {
				r.Problem = fmt.Sprintf("nieczytelne /version: %v", err)
				break
			}
			if r.Reported.Version != want.Version || r.Reported.Commit != want.Commit {
				r.Problem = fmt.Sprintf("raportuje %s (%s), oczekiwano %s (%s)", r.Reported.Version, r.Reported.Commit, want.Version, want.Commit)
			}
		}
		if r.Problem != "" {
			v.Fail("%s: %s", r.Pod, r.Problem)
		} else {
			cli.Step("%s (%s): /version %s, commit %s", r.Pod, r.Node, r.Reported.Version, shortCommit(r.Reported.Commit))
		}
		results = append(results, r)
	}
	if replicas := int(*dep.Spec.Replicas); len(results) != replicas {
		v.Fail("sprawdzone pody: %d, a Deployment %s ma %d replik", len(results), deployment, replicas)
	}
	if err := v.Err("tożsamość na podach"); err != nil {
		return results, cli.Unmet("na klastrze działa coś innego niż obraz zbudowany z tego drzewa — %v", err)
	}
	return results, nil
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// PSAVersion zwraca wersję profilu Pod Security dla wersji Kubernetesa
// X.Y.Z: vX.Y (decyzja 31). „latest" zmieniałby reguły razem z aktualizacją
// klastra, bez przeglądu.
func PSAVersion(kubernetesVersion string) (string, error) {
	parts := strings.Split(kubernetesVersion, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("wersja Kubernetesa %q spoza X.Y.Z", kubernetesVersion)
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return "", fmt.Errorf("wersja Kubernetesa %q: %w", kubernetesVersion, err)
	}
	return "v" + parts[0] + "." + parts[1], nil
}
