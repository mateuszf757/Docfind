//go:build cluster

// Warianty negatywne na żywym klastrze środowiska (DOCFIND_ENV, domyślnie
// dev): API server ma odrzucić to, czego bramka nie może przemycić.
//
//	./bin/mise run test:cluster
package drain

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/kube"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
)

// clusterClient łączy się z klastrem środowiska przez strażnika kontekstu.
// Klient client-go wprost, bo próba na sucho (dryRun) nie jest operacją
// portu — test może sięgnąć głębiej niż etap.
func clusterClient(t *testing.T) (kubernetes.Interface, environment.Definition) {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := strings.TrimSpace(string(out))
	def, err := environment.Load(root, environment.Selected())
	if err != nil {
		t.Fatal(err)
	}
	target := def.Target(root)
	if err := kube.Guard(target); err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: target.Kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: target.Context},
	).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return client, def
}

// Przestrzeń nazw aplikacji ma Pod Security restricted (decyzja 31), więc
// sonda bez pełnego securityContext zostaje odrzucona przy tworzeniu —
// regresja wychodzi przy pierwszym biegu, a nie w audycie. Stara sonda
// (curl bez securityContext) była odrzucana dokładnie tak.
func TestProbeWithoutSecurityContextIsRejected(t *testing.T) {
	client, def := clusterClient(t)
	ctx := context.Background()
	dryRun := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}

	ok := ProbePod("drain-probe-test-ok", def.App.Namespace, "test", "k3d-docfind-agent-0", "docfind-drain-probe:test", "http://x")
	if _, err := client.CoreV1().Pods(def.App.Namespace).Create(ctx, ok, dryRun); err != nil {
		t.Fatalf("pełna sonda odrzucona — kontrola nie przeszła: %v", err)
	}

	bad := ProbePod("drain-probe-test-bad", def.App.Namespace, "test", "k3d-docfind-agent-0", "docfind-drain-probe:test", "http://x")
	bad.Spec.SecurityContext = nil
	bad.Spec.Containers[0].SecurityContext = nil
	_, err := client.CoreV1().Pods(def.App.Namespace).Create(ctx, bad, dryRun)
	if err == nil || !strings.Contains(err.Error(), "violates PodSecurity") {
		t.Fatalf("sonda bez securityContext przyjęta albo odrzucona z innego powodu: %v", err)
	}
	t.Logf("odrzucona: %v", err)
}
