package evidence

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mateuszf757/Docfind/tools/internal/kube"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// TestCollect: poprzedni log tylko z kontenera po restarcie, brak jednego
// elementu (CRD platformy) zapisany, a nie przerywający reszty.
func TestCollect(t *testing.T) {
	core := fake.NewClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "docfind"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "api", RestartCount: 2}}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-2", Namespace: "docfind"},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "api"}}}},
	)
	var calls []string
	runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
		cmd := c.String()
		calls = append(calls, cmd)
		switch {
		case strings.HasPrefix(cmd, "docker ps"):
			return proc.Result{Stdout: []byte("k3d-docfind-ci-server-0\nk3d-docfind-ci-agent-0\n")}, nil
		case strings.Contains(cmd, "gateways,httproutes"):
			return proc.Result{}, &proc.ExitError{Cmd: cmd, Code: 1}
		}
		if c.Stdout != nil {
			_, _ = c.Stdout.Write([]byte("wyjście " + cmd + "\n"))
		}
		return proc.Result{}, nil
	})
	dir := t.TempDir()
	c := Collector{Core: core, Runner: runner, Target: kube.Target{Kubeconfig: "k", Context: "k3d-docfind-ci"},
		Cluster: "docfind-ci", Namespaces: []string{"docfind"}, Dir: dir}
	missing, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || !strings.HasPrefix(missing[0], "platform.yaml") {
		t.Errorf("braki: %v", missing)
	}
	for _, rel := range []string{"pods.txt", "events.txt", "docfind/api-1.log", "docfind/api-1.previous.log", "docfind/api-2.log", "nodes/k3d-docfind-ci-agent-0.log"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("brak %s", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "docfind/api-2.previous.log")); err == nil {
		t.Error("poprzedni log kontenera bez restartu")
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "kubectl") && !strings.Contains(call, "--context k3d-docfind-ci") {
			t.Errorf("kubectl bez jawnego kontekstu: %s", call)
		}
	}
}

func TestCollectWithoutCluster(t *testing.T) {
	runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
		return proc.Result{}, nil
	})
	missing, err := Collector{Runner: runner, Cluster: "docfind-ci", Dir: t.TempDir()}.Collect(context.Background())
	if err != nil || len(missing) != 1 || !strings.Contains(missing[0], "brak połączenia") {
		t.Errorf("braki %v, błąd %v", missing, err)
	}
}
