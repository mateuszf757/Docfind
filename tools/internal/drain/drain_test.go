package drain

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

func gate(objects ...runtime.Object) *Gate {
	return &Gate{
		Core: fake.NewClientset(objects...),
		Cfg:  Config{Namespace: "docfind", Deployment: "docfind-api", RunID: "nasz", LeaseDuration: time.Minute, LeaseRenew: time.Second},
	}
}

func lease(holder string, renewed time.Time) *coordinationv1.Lease {
	seconds := int32(60)
	t := metav1.NewMicroTime(renewed)
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: "docfind"},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, RenewTime: &t},
	}
}

func TestAcquireLease(t *testing.T) {
	quiet(t)
	ctx := context.Background()
	if err := gate().acquireLease(ctx); err != nil {
		t.Errorf("wolna blokada: %v", err)
	}
	// Inny bieg trwa (blokada odnowiona przed chwilą): odmowa, nie awaria.
	err := gate(lease("inny", time.Now())).acquireLease(ctx)
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "trwa inny bieg") {
		t.Errorf("żywa cudza blokada: %v", err)
	}
	// Bieg zabity SIGKILL-em przestał odnawiać: blokada przejęta.
	g := gate(lease("martwy", time.Now().Add(-2*time.Minute)))
	if err := g.acquireLease(ctx); err != nil {
		t.Fatalf("wygasła blokada nie przejęta: %v", err)
	}
	l, _ := g.Core.CoordinationV1().Leases("docfind").Get(ctx, LeaseName, metav1.GetOptions{})
	if *l.Spec.HolderIdentity != "nasz" {
		t.Errorf("blokadę trzyma %q", *l.Spec.HolderIdentity)
	}
}

func node(name string, unschedulable bool, annotations map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}, Spec: corev1.NodeSpec{Unschedulable: unschedulable}}
}

func TestRepairAndForeignCordons(t *testing.T) {
	quiet(t)
	ctx := context.Background()
	ours := map[string]string{AnnotationCordonedBy: CordonedByGate, AnnotationCordonedRun: "stary", AnnotationCordonedAt: "2026-10-07T08:27:43Z"}
	probe := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "drain-probe-stary-1", Namespace: "docfind", Labels: map[string]string{probeNameLabel: probeNameValue, runLabel: "stary"}}}
	g := gate(node("a", true, ours), node("b", true, nil), node("c", false, nil), probe)

	repaired, err := g.repairPrevious(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 2 {
		t.Errorf("naprawione: %v — oczekiwano węzła a i sondy", repaired)
	}
	a, _ := g.Core.CoreV1().Nodes().Get(ctx, "a", metav1.GetOptions{})
	if a.Spec.Unschedulable || a.Annotations[AnnotationCordonedBy] != "" {
		t.Errorf("węzeł zostawiony przez bramkę nie przywrócony: %+v", a)
	}
	if pods, _ := g.Core.CoreV1().Pods("docfind").List(ctx, metav1.ListOptions{}); len(pods.Items) != 0 {
		t.Errorf("osierocona sonda została")
	}
	// Węzeł b odciął ktoś inny — bramka go nie rusza, tylko odmawia.
	foreign, err := g.foreignCordons(ctx)
	if err != nil || len(foreign) != 1 || foreign[0] != "b" {
		t.Errorf("cordon spoza bramki: %v, %v", foreign, err)
	}
	b, _ := g.Core.CoreV1().Nodes().Get(ctx, "b", metav1.GetOptions{})
	if !b.Spec.Unschedulable {
		t.Error("bramka zdjęła cudzy cordon")
	}
}

func TestCheckPDB(t *testing.T) {
	quiet(t)
	labels := map[string]string{"app": "api"}
	pdb := func(allowed int32) *policyv1.PodDisruptionBudget {
		return &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "docfind-api", Namespace: "docfind"},
			Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
			Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: allowed},
		}
	}
	fail := func(format string, args ...any) error { return cli.Unmet(format, args...) }
	tests := []struct {
		name    string
		objects []runtime.Object
		want    int
	}{
		{"PDB dopuszcza zakłócenie", []runtime.Object{pdb(1)}, cli.ExitOK},
		{"PDB nie dopuszcza teraz", []runtime.Object{pdb(0)}, cli.ExitUnmet},
		{"brak PDB", nil, cli.ExitUnmet},
	}
	for _, tt := range tests {
		err := gate(tt.objects...).checkPDB(context.Background(), labels, fail)
		if code := cli.ExitCode(err); code != tt.want {
			t.Errorf("%s: kod %d, oczekiwano %d (%v)", tt.name, code, tt.want, err)
		}
	}
}

// TestProbePodMeetsRestricted: sonda spełnia profil Pod Security
// restricted — inaczej API server odrzuci ją w przestrzeni nazw aplikacji.
func TestProbePodMeetsRestricted(t *testing.T) {
	p := ProbePod("drain-probe-x-1", "docfind", "x", "w1", "docfind-drain-probe:local-1", "http://svc")
	sc := p.Spec.SecurityContext
	c := p.Spec.Containers[0]
	switch {
	case sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser == 0:
		t.Error("brak runAsNonRoot z numerycznym UID")
	case sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault:
		t.Error("brak seccomp RuntimeDefault")
	case c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation:
		t.Error("eskalacja uprawnień dopuszczona")
	case c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL":
		t.Error("uprawnienia nie zdjęte")
	case p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken:
		t.Error("sonda z tokenem konta usługi")
	case c.ImagePullPolicy != corev1.PullNever:
		t.Error("obraz sondy byłby pobierany z rejestru, którego nie ma")
	}
	if _, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		t.Error("limit CPU wbrew decyzji 16")
	}
}

func TestProbeImageIsReproducible(t *testing.T) {
	bin := t.TempDir() + "/drain-probe"
	if err := writeFile(bin, "binarka"); err != nil {
		t.Fatal(err)
	}
	first, err := ProbeImage(bin)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProbeImage(bin)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := ProbeTag(first)
	b, _ := ProbeTag(second)
	if a != b {
		t.Errorf("ta sama binarka, różne obrazy: %s ≠ %s", a, b)
	}
	cfg, _ := first.ConfigFile()
	if cfg.Config.User != "65532:65532" || len(cfg.Config.Entrypoint) != 1 {
		t.Errorf("konfiguracja obrazu: %+v", cfg.Config)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o755)
}
