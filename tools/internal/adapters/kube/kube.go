// Package kube to adapter klastra Kubernetesa: strażnik kontekstu,
// połączenie z klastrem środowiska i operacje, których potrzebują etapy,
// w typach z core/cluster. Większość idzie przez API (client-go); drain
// przez kubectl (eksmisja z poszanowaniem PDB i ponawianiem przy 429 to ten
// sam kod co w kubectl, bez wciągania k8s.io/kubectl — decyzja 33),
// odnowienie certyfikatu przez cmctl, a zrzuty dowodów przez kubectl
// w postaci, którą czyta człowiek. Wszystkie trzy drogi trafiają w ten sam
// klaster: jawny kubeconfig i kontekst z definicji środowiska.
package kube

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Guard sprawdza kubeconfig, zanim cokolwiek trafi do klastra:
//
//   - bieżący kontekst to kontekst środowiska — kontekst przełączony ręcznie
//     w kubeconfigu projektu znaczy, że ktoś pracuje na innym klastrze tym
//     samym plikiem;
//   - kontekst wskazuje na serwer z definicji — kontekst o tej samej nazwie,
//     ale z innym adresem (skopiowany kubeconfig, inny klaster po
//     odtworzeniu) przeszedłby samo porównanie nazw.
//
// Klienci i tak łączą się przez jawnie podany kontekst, więc bieżący
// kontekst nie decyduje, dokąd trafią żądania — strażnik jest drugą warstwą.
// Niezgodność to warunek niespełniony (kod 1): program odmawia, zamiast
// działać na cudzym klastrze.
func Guard(t cluster.Target) error {
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
	c, ok := raw.Clusters[kctx.Cluster]
	if !ok {
		return cli.Unmet("kontekst %q wskazuje na nieistniejący klaster %q", t.Context, kctx.Cluster)
	}
	if c.Server != t.APIServer {
		return cli.Unmet("kontekst %q wskazuje na %s, a środowisko na %s — nie działam na cudzym klastrze", t.Context, c.Server, t.APIServer)
	}
	return nil
}

// Cluster — klaster po połączeniu.
type Cluster struct {
	core kubernetes.Interface
	dyn  dynamic.Interface
	// config — połączenie z kubeconfigu, dla strumieni (port-forward); nil
	// w adapterze złożonym z gotowych klientów (New).
	config *rest.Config
	// runner i target — kubectl i cmctl na tym samym klastrze co klienci.
	runner proc.Runner
	target cluster.Target
	// mapper — rodzaje zasobów z discovery, przy pierwszym użyciu.
	mapper meta.RESTMapper
}

// New składa adapter z gotowych klientów i programów (testy: fałszywy
// clientset i nagrane odpowiedzi kubectl).
func New(core kubernetes.Interface, dyn dynamic.Interface, runner proc.Runner, t cluster.Target) *Cluster {
	return &Cluster{core: core, dyn: dyn, runner: runner, target: t}
}

// Connect sprawdza strażnika i łączy się przez jawnie podany kontekst.
func Connect(t cluster.Target, runner proc.Runner) (*Cluster, error) {
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
	c := New(core, dyn, runner, t)
	c.config = config
	return c, nil
}

// kubectlArgs kieruje kubectl i cmctl na ten sam klaster co klientów —
// jawnie, bez polegania na KUBECONFIG z powłoki.
func (c *Cluster) kubectlArgs() []string {
	return []string{"--kubeconfig", c.target.Kubeconfig, "--context", c.target.Context}
}
