package kube

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
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
		err := Guard(cluster.Target{Kubeconfig: path, Context: "k3d-docfind", APIServer: "https://127.0.0.1:6550"})
		if code := cli.ExitCode(err); code != tt.want {
			t.Errorf("%s: kod %d, oczekiwano %d (%v)", tt.name, code, tt.want, err)
		}
	}
	if err := Guard(cluster.Target{Kubeconfig: filepath.Join(t.TempDir(), "brak")}); cli.ExitCode(err) != cli.ExitFailure {
		t.Errorf("brak kubeconfigu to awaria, nie wynik: %v", err)
	}
}

func lease(holder string, renewed time.Time) *coordinationv1.Lease {
	seconds := int32(60)
	t := metav1.NewMicroTime(renewed)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: "blokada", Namespace: "docfind"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, RenewTime: &t},
	}
}

func TestLease(t *testing.T) {
	ctx := context.Background()
	got, err := New(fake.NewClientset(), nil, nil, cluster.Target{}).AcquireLease(ctx, "docfind", "blokada", "nasz", time.Minute)
	if err != nil || !got.Acquired || got.TookOver {
		t.Errorf("wolna blokada: %+v, %v", got, err)
	}
	// Inny bieg trwa (odnowił przed chwilą): nie wzięta, z właścicielem.
	got, err = New(fake.NewClientset(lease("inny", time.Now())), nil, nil, cluster.Target{}).AcquireLease(ctx, "docfind", "blokada", "nasz", time.Minute)
	if err != nil || got.Acquired || got.Holder != "inny" || got.Expires.IsZero() {
		t.Errorf("żywa cudza blokada: %+v, %v", got, err)
	}
	// Bieg zabity SIGKILL-em przestał odnawiać: przejęta.
	cs := fake.NewClientset(lease("martwy", time.Now().Add(-2*time.Minute)))
	c := New(cs, nil, nil, cluster.Target{})
	got, err = c.AcquireLease(ctx, "docfind", "blokada", "nasz", time.Minute)
	if err != nil || !got.Acquired || !got.TookOver {
		t.Fatalf("wygasła blokada nie przejęta: %+v, %v", got, err)
	}
	l, _ := cs.CoordinationV1().Leases("docfind").Get(ctx, "blokada", metav1.GetOptions{})
	if *l.Spec.HolderIdentity != "nasz" {
		t.Errorf("blokadę trzyma %q", *l.Spec.HolderIdentity)
	}
	// Zwolnienie cudzej blokady niczego nie robi; własnej — usuwa ją.
	if err := c.ReleaseLease(ctx, "docfind", "blokada", "inny"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoordinationV1().Leases("docfind").Get(ctx, "blokada", metav1.GetOptions{}); err != nil {
		t.Error("zwolnienie cudzej blokady ją usunęło")
	}
	if err := c.ReleaseLease(ctx, "docfind", "blokada", "nasz"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoordinationV1().Leases("docfind").Get(ctx, "blokada", metav1.GetOptions{}); err == nil {
		t.Error("własna blokada nie zwolniona")
	}
}

func TestCordonUncordon(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}})
	c := New(cs, nil, nil, cluster.Target{})
	if err := c.Cordon(ctx, "a", map[string]string{"x/by": "bramka", "x/run": "1"}); err != nil {
		t.Fatal(err)
	}
	nodes, err := c.Nodes(ctx)
	if err != nil || len(nodes) != 1 || !nodes[0].Unschedulable || nodes[0].Annotations["x/by"] != "bramka" {
		t.Fatalf("po cordon: %+v, %v", nodes, err)
	}
	if err := c.Uncordon(ctx, "a", []string{"x/by", "x/run"}); err != nil {
		t.Fatal(err)
	}
	nodes, _ = c.Nodes(ctx)
	if nodes[0].Unschedulable || nodes[0].Annotations["x/by"] != "" {
		t.Errorf("po uncordon: %+v", nodes[0])
	}
}

func TestPDBFor(t *testing.T) {
	labels := map[string]string{"app": "api"}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "docfind-api", Namespace: "docfind"},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
		Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 1},
	}
	c := New(fake.NewClientset(pdb), nil, nil, cluster.Target{})
	got, found, err := c.PDBFor(context.Background(), "docfind", labels)
	if err != nil || !found || got.DisruptionsAllowed != 1 {
		t.Errorf("PDB obejmujący pody: %+v, %v, %v", got, found, err)
	}
	if _, found, _ := c.PDBFor(context.Background(), "docfind", map[string]string{"app": "inne"}); found {
		t.Error("PDB dopasowany do obcych etykiet")
	}
}

func TestRolloutAndPods(t *testing.T) {
	replicas := int32(2)
	labels := map[string]string{"app": "api"}
	now := metav1.Now()
	objects := []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "docfind-api", Namespace: "docfind", Generation: 3},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}}},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 3, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "docfind", Labels: labels},
			Spec:       corev1.PodSpec{NodeName: "w1", Containers: []corev1.Container{{Name: "api", Image: "img:1"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "api", RestartCount: 2}, {Name: "sidecar"}}},
		},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "docfind", Labels: labels, DeletionTimestamp: &now, Finalizers: []string{"x"}}},
	}
	c := New(fake.NewClientset(objects...), nil, nil, cluster.Target{})
	r, err := c.Rollout(context.Background(), "docfind", "docfind-api")
	if err != nil || !r.Complete() || r.Selector != "app=api" {
		t.Errorf("rollout: %+v, %v", r, err)
	}
	pods, err := c.Pods(context.Background(), "docfind", r.Selector)
	if err != nil || len(pods) != 2 {
		t.Fatalf("pody: %+v, %v", pods, err)
	}
	for _, p := range pods {
		switch p.Name {
		case "a":
			// Restarted — tylko kontener, który ma poprzednią instancję.
			if !p.Running || p.Restarts != 2 || p.Images["api"] != "img:1" || p.Node != "w1" || len(p.Restarted) != 1 || p.Restarted[0] != "api" {
				t.Errorf("pod a: %+v", p)
			}
		case "b":
			if !p.Terminating {
				t.Errorf("pod b bez Terminating: %+v", p)
			}
		}
	}
}

func TestStoreSecret(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, nil, nil, cluster.Target{})
	ctx := context.Background()
	for _, value := range []string{"pierwszy", "drugi"} {
		if err := c.StoreSecret(ctx, "cert-manager", "token", map[string][]byte{"api-token": []byte(value)}); err != nil {
			t.Fatal(err)
		}
		data, err := c.SecretData(ctx, "cert-manager", "token")
		if err != nil || string(data["api-token"]) != value {
			t.Errorf("Secret ma %q, oczekiwano %q (%v)", data["api-token"], value, err)
		}
	}
	if ok, err := c.SecretExists(ctx, "cert-manager", "brak"); ok || err != nil {
		t.Errorf("SecretExists dla brakującego: %v, %v", ok, err)
	}
}

func TestGatewayRouteCertificate(t *testing.T) {
	// Obiekty zakładane przez Create z jawnym GVR: tracker atrapy zgaduje
	// liczbę mnogą z rodzaju („Gateway" → „gatewaies"), więc obiekty podane
	// konstruktorowi trafiłyby pod zły zasób.
	ctx := context.Background()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		gatewayGVR: "GatewayList", httpRouteGVR: "HTTPRouteList", certificateGVR: "CertificateList",
	})
	for _, o := range []struct {
		gvr schema.GroupVersionResource
		obj map[string]any
	}{
		{gatewayGVR, map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1", "kind": "Gateway",
			"metadata": map[string]any{"name": "docfind", "namespace": "gateway", "annotations": map[string]any{"cert-manager.io/cluster-issuer": "letsencrypt-staging"}},
			"spec": map[string]any{"listeners": []any{
				map[string]any{"name": "http", "port": int64(80)},
				map[string]any{"name": "https", "hostname": "local.docfind.lol"},
			}},
			"status": map[string]any{"conditions": []any{map[string]any{"type": "Programmed", "status": "True"}}},
		}},
		{httpRouteGVR, map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute",
			"metadata": map[string]any{"name": "docfind-api", "namespace": "docfind"},
			"status": map[string]any{"parents": []any{map[string]any{"conditions": []any{
				map[string]any{"type": "Accepted", "status": "True"},
				map[string]any{"type": "ResolvedRefs", "status": "False"},
			}}}},
		}},
		{certificateGVR, map[string]any{
			"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
			"metadata": map[string]any{"name": "docfind-tls", "namespace": "gateway"},
			"status":   map[string]any{"revision": int64(4), "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
		}},
	} {
		u := &unstructured.Unstructured{Object: o.obj}
		if _, err := dyn.Resource(o.gvr).Namespace(u.GetNamespace()).Create(ctx, u, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	c := New(fake.NewClientset(), dyn, nil, cluster.Target{})
	gw, err := c.Gateway(ctx, "gateway", "docfind")
	if err != nil || gw.HTTPSHostname != "local.docfind.lol" || gw.Issuer != "letsencrypt-staging" || !gw.Programmed {
		t.Errorf("Gateway: %+v, %v", gw, err)
	}
	route, err := c.Route(ctx, "docfind", "docfind-api")
	if err != nil || route.Accepted != "True" || route.ResolvedRefs != "False" {
		t.Errorf("HTTPRoute: %+v, %v", route, err)
	}
	cert, err := c.Certificate(ctx, "gateway", "docfind-tls")
	if err != nil || cert.Revision != 4 || !cert.Ready {
		t.Errorf("Certificate: %+v, %v", cert, err)
	}
}

// TestTools: kubectl i cmctl dostają jawny kubeconfig i kontekst
// środowiska, a nie bieżący kontekst z powłoki.
func TestTools(t *testing.T) {
	target := cluster.Target{Kubeconfig: "/repo/.kube/dev", Context: "k3d-docfind"}
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"kubectl drain w1 --ignore-daemonsets --delete-emptydir-data --timeout=3m0s --kubeconfig /repo/.kube/dev --context k3d-docfind": {},
		"cmctl renew --namespace gateway docfind-tls --kubeconfig /repo/.kube/dev --context k3d-docfind":                                {},
		"kubectl logs -n docfind api-1 --prefix --timestamps -c api --previous --kubeconfig /repo/.kube/dev --context k3d-docfind":      {Stdout: "log"},
		"kubectl version --client -o json": {Stdout: `{"clientVersion":{"gitVersion":"v1.36.4"}}`},
	}}
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "docfind-api", Namespace: "docfind"},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "sidecar", Image: "inny"}, {Name: "api", Image: "ghcr.io/x/api:1@sha256:abc"}},
		}}},
	}
	c := New(fake.NewClientset(dep), nil, f, target)
	ctx := context.Background()
	if err := c.Drain(ctx, "w1", 3*time.Minute); err != nil {
		t.Error(err)
	}
	if err := c.RenewCertificate(ctx, "gateway", "docfind-tls"); err != nil {
		t.Error(err)
	}
	var log strings.Builder
	if err := c.Logs(ctx, "docfind", "api-1", "api", true, &log); err != nil || log.String() != "log" {
		t.Errorf("logi: %q, %v", log.String(), err)
	}
	if v, err := (Kubectl{Runner: f}).ClientVersion(ctx); err != nil || v != "v1.36.4" {
		t.Errorf("wersja kubectl: %q, %v", v, err)
	}
	if img, err := c.DeploymentImage(ctx, "docfind", "docfind-api", "api"); err != nil || img != "ghcr.io/x/api:1@sha256:abc" {
		t.Errorf("obraz z szablonu: %q, %v", img, err)
	}
	if _, err := c.DeploymentImage(ctx, "docfind", "docfind-api", "brak"); err == nil {
		t.Error("brak kontenera przyjęty")
	}
}
