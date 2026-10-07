package kube

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

const kubeconfig = `apiVersion: v1
kind: Config
current-context: %s
clusters:
- name: k3d-docfind
  cluster:
    server: %s
contexts:
- name: k3d-docfind
  context: {cluster: k3d-docfind, user: admin}
- name: inny
  context: {cluster: k3d-docfind, user: admin}
users:
- name: admin
  user: {token: x}
`

func TestGuard(t *testing.T) {
	tests := []struct {
		name, current, server string
		want                  int
	}{
		{"zgodny", "k3d-docfind", "https://127.0.0.1:6550", cli.ExitOK},
		{"inny bieżący kontekst", "inny", "https://127.0.0.1:6550", cli.ExitUnmet},
		{"inny serwer pod tą samą nazwą", "k3d-docfind", "https://10.0.0.5:6443", cli.ExitUnmet},
	}
	for _, tt := range tests {
		path := filepath.Join(t.TempDir(), "kubeconfig")
		content := strings.Replace(strings.Replace(kubeconfig, "%s", tt.current, 1), "%s", tt.server, 1)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		err := Guard(Target{Kubeconfig: path, Context: "k3d-docfind", APIServer: "https://127.0.0.1:6550"})
		if code := cli.ExitCode(err); code != tt.want {
			t.Errorf("%s: kod %d, oczekiwano %d (%v)", tt.name, code, tt.want, err)
		}
	}
	if err := Guard(Target{Kubeconfig: filepath.Join(t.TempDir(), "brak")}); cli.ExitCode(err) != cli.ExitFailure {
		t.Errorf("brak kubeconfigu to awaria, nie wynik: %v", err)
	}
}

// proxyResponse udaje odpowiedź /version z proxy API servera.
type proxyResponse struct{ body string }

func (p proxyResponse) DoRaw(context.Context) ([]byte, error) { return []byte(p.body), nil }
func (p proxyResponse) Stream(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(p.body)), nil
}

func fakeCluster(replicas int32, pods map[string]string, versions map[string]string) *fake.Clientset {
	labels := map[string]string{"app": "api"}
	objects := []runtime.Object{&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "docfind-api", Namespace: "docfind"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}}
	for name, image := range pods {
		objects = append(objects, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "docfind", Labels: labels},
			Spec:       corev1.PodSpec{NodeName: "w-" + name, Containers: []corev1.Container{{Name: "api", Image: image}}},
		})
	}
	cs := fake.NewClientset(objects...)
	cs.PrependProxyReactor("pods", func(action k8stesting.Action) (bool, restclient.ResponseWrapper, error) {
		pod := action.(k8stesting.ProxyGetAction).GetName()
		return true, proxyResponse{body: versions[pod]}, nil
	})
	return cs
}

func TestVerifyDeployment(t *testing.T) {
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	defer func() { cli.Out, cli.Err = oldOut, oldErr }()

	want := Expected{Image: "img:1", Version: "1.0.0", Commit: "abc"}
	ok := `{"version":"1.0.0","commit":"abc"}`
	tests := []struct {
		name     string
		replicas int32
		pods     map[string]string
		versions map[string]string
		wantCode int
	}{
		{"każdy pod zgodny", 2, map[string]string{"a": "img:1", "b": "img:1"}, map[string]string{"a": ok, "b": ok}, cli.ExitOK},
		{"inny obraz na jednym podzie", 2, map[string]string{"a": "img:1", "b": "img:0"}, map[string]string{"a": ok, "b": ok}, cli.ExitUnmet},
		{"inny commit na podzie", 2, map[string]string{"a": "img:1", "b": "img:1"}, map[string]string{"a": ok, "b": `{"version":"1.0.0","commit":"zzz"}`}, cli.ExitUnmet},
		{"mniej podów niż replik", 2, map[string]string{"a": "img:1"}, map[string]string{"a": ok}, cli.ExitUnmet},
	}
	for _, tt := range tests {
		cs := fakeCluster(tt.replicas, tt.pods, tt.versions)
		_, err := VerifyDeployment(context.Background(), cs, "docfind", "docfind-api", "api", want)
		if code := cli.ExitCode(err); code != tt.wantCode {
			t.Errorf("%s: kod %d, oczekiwano %d (%v)", tt.name, code, tt.wantCode, err)
		}
	}
}

func TestPSAVersion(t *testing.T) {
	if v, err := PSAVersion("1.36.4"); err != nil || v != "v1.36" {
		t.Errorf("PSAVersion = %q, %v", v, err)
	}
	if _, err := PSAVersion("1.36"); err == nil {
		t.Error("wersja spoza X.Y.Z przyjęta")
	}
}
