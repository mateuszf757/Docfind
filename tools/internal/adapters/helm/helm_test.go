package helm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

func TestInstallAndTemplate(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"helm upgrade --install platform deploy/charts/platform --namespace gateway --create-namespace --values v.yaml --set gateway.name=docfind --wait=watcher --timeout 3m0s --kubeconfig /k --kube-context k3d-docfind": {},
		"helm upgrade --install openbao /c/openbao.tgz --namespace openbao --wait=hookOnly --timeout 5m0s --kubeconfig /k --kube-context k3d-docfind":                                                                       {},
		"helm template docfind deploy/charts/docfind --set api.replica=3":      {Stderr: "Error: values don't meet the specifications of the schema", Code: 1},
		"helm template docfind deploy/charts/docfind --set api.image.tag=lint": {Stdout: "kind: Deployment\n"},
	}}
	h := Helm{Runner: f}
	ctx := context.Background()
	err := h.Install(ctx, cluster.Target{Kubeconfig: "/k", Context: "k3d-docfind"}, cluster.Release{
		Name: "platform", Chart: "deploy/charts/platform", Namespace: "gateway", CreateNamespace: true,
		ValuesFiles: []string{"v.yaml"}, Set: []string{"gateway.name=docfind"}, Timeout: 3 * time.Minute,
	})
	if err != nil {
		t.Errorf("instalacja: %v", err)
	}
	// Bez czekania na gotowość — pody staną się gotowe dopiero po kroku po
	// instalacji (inicjalizacja OpenBao).
	err = h.Install(ctx, cluster.Target{Kubeconfig: "/k", Context: "k3d-docfind"}, cluster.Release{
		Name: "openbao", Chart: "/c/openbao.tgz", Namespace: "openbao", Timeout: 5 * time.Minute, HooksOnly: true,
	})
	if err != nil {
		t.Errorf("instalacja bez czekania na gotowość: %v", err)
	}
	err = h.Template(ctx, cluster.Release{Name: "docfind", Chart: "deploy/charts/docfind", Set: []string{"api.replica=3"}}, false, &strings.Builder{})
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "specifications of the schema") {
		t.Errorf("odmowa schematu: %v", err)
	}
	var out strings.Builder
	if err := h.Template(ctx, cluster.Release{Name: "docfind", Chart: "deploy/charts/docfind", Set: []string{"api.image.tag=lint"}}, false, &out); err != nil || out.String() != "kind: Deployment\n" {
		t.Errorf("render: %q, %v", out.String(), err)
	}
}

// ociRegistry — rejestr z jednym manifestem charta Helma.
type ociRegistry struct {
	manifest, layer []byte
	pulls           *int
}

func (r ociRegistry) Manifest(_ context.Context, ref string) ([]byte, string, error) {
	if ref != "docker.io/envoyproxy/gateway-helm:v1.9.2" {
		return nil, "", fmt.Errorf("nieznany %s", ref)
	}
	*r.pulls++
	return r.manifest, "application/vnd.oci.image.manifest.v1+json", nil
}

func (r ociRegistry) Blob(_ context.Context, repository, digest string) ([]byte, error) {
	if sum := sha256.Sum256(r.layer); repository != "docker.io/envoyproxy/gateway-helm" || digest != "sha256:"+hex.EncodeToString(sum[:]) {
		return nil, fmt.Errorf("nieznany blob %s@%s", repository, digest)
	}
	return r.layer, nil
}

// TestFetchOCI: chart z rejestru OCI to warstwa charta z manifestu pod
// wersją, w pamięci podręcznej po pierwszym pobraniu i sprawdzana sumą —
// także przy kolejnym użyciu.
func TestFetchOCI(t *testing.T) {
	content := []byte("archiwum charta")
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	manifest := []byte(`{"schemaVersion":2,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json"},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":"` + digest + `"}]}`)
	chart := pins.Chart{Name: pins.EnvoyGateway, Version: "v1.9.2", Source: "oci://docker.io/envoyproxy/gateway-helm", SHA256: hex.EncodeToString(sum[:])}
	pulls := 0
	c := Charts{Root: t.TempDir(), Registry: ociRegistry{manifest, content, &pulls}}
	for range 2 {
		path, err := c.Fetch(context.Background(), chart)
		if err != nil || filepath.Base(path) != "gateway-helm-v1.9.2.tgz" {
			t.Fatalf("chart: %s, %v", path, err)
		}
		if data, _ := os.ReadFile(path); string(data) != string(content) {
			t.Fatalf("archiwum: %q", data)
		}
	}
	if pulls != 1 {
		t.Errorf("manifest pobrany %d razy — drugi raz z pamięci podręcznej", pulls)
	}
	chart.SHA256 = strings.Repeat("0", 64)
	if _, err := c.Fetch(context.Background(), chart); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("podmieniony chart przyjęty: %v", err)
	}
	// Manifest bez warstwy charta (np. obraz pod tą nazwą) — odmowa.
	empty := Charts{Root: t.TempDir(), Registry: ociRegistry{[]byte(`{"layers":[]}`), content, &pulls}}
	chart.SHA256 = hex.EncodeToString(sum[:])
	if _, err := empty.Fetch(context.Background(), chart); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("manifest bez warstwy charta: %v", err)
	}
}

func TestRevisionAndRollback(t *testing.T) {
	target := cluster.Target{Kubeconfig: "/k", Context: "c"}
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"helm status docfind --namespace docfind --output json --kubeconfig /k --kube-context c":                                                                          {Stdout: `{"name":"docfind","version":3}`},
		"helm rollback docfind 2 --namespace docfind --wait=watcher --timeout 3m0s --kubeconfig /k --kube-context c":                                                      {},
		"helm upgrade --install docfind oci://r/charts/docfind --version 0.3.2-dev.44 --namespace docfind --wait=watcher --timeout 3m0s --kubeconfig /k --kube-context c": {},
	}}
	h := Helm{Runner: f}
	ctx := context.Background()
	if rev, err := h.Revision(ctx, target, "docfind", "docfind"); rev != 3 || err != nil {
		t.Errorf("rewizja: %d, %v", rev, err)
	}
	if err := h.Rollback(ctx, target, "docfind", "docfind", 2, 3*time.Minute); err != nil {
		t.Errorf("rollback: %v", err)
	}
	if err := h.Install(ctx, target, cluster.Release{Name: "docfind", Chart: "oci://r/charts/docfind", Version: "0.3.2-dev.44", Namespace: "docfind", Timeout: 3 * time.Minute}); err != nil {
		t.Errorf("chart z rejestru w wersji: %v", err)
	}
}
