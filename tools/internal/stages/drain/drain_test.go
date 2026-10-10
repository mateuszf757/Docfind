package drain

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

// fakeCluster — klaster w pamięci: węzły, repliki API, sondy z logiem,
// blokada. Drain przenosi repliki z węzła na pierwszy inny przyjmujący pody.
type fakeCluster struct {
	mu       sync.Mutex
	nodes    map[string]*cluster.Node
	replicas map[string]string // pod → węzeł
	probes   map[string]string // sonda → węzeł
	leftover []cluster.Pod     // sondy po poprzednim biegu
	pdb      *cluster.PDB
	lease    cluster.LeaseAttempt
	released bool
	// probeLog — log sondy: wyniki żądań.
	probeLog string
	drained  []string
}

func newCluster(replicaNodes ...string) *fakeCluster {
	f := &fakeCluster{
		nodes:    map[string]*cluster.Node{},
		replicas: map[string]string{},
		probes:   map[string]string{},
		pdb:      &cluster.PDB{Name: "docfind-api", DisruptionsAllowed: 1},
		lease:    cluster.LeaseAttempt{Acquired: true},
		probeLog: requests(150, 0),
	}
	for _, n := range []string{"server-0", "agent-0", "agent-1"} {
		f.nodes[n] = &cluster.Node{Name: n, Ready: true, Annotations: map[string]string{}}
	}
	for i, n := range replicaNodes {
		f.replicas[fmt.Sprintf("api-%d", i)] = n
	}
	return f
}

// requests — log sondy z ok udanymi i failed nieudanymi żądaniami.
func requests(ok, failed int) string {
	var b strings.Builder
	for i := range ok + failed {
		code, connected := 200, true
		if i >= ok {
			code, connected = 503, true
		}
		fmt.Fprintf(&b, `{"t":"2026-10-10T08:00:00Z","seq":%d,"code":%d,"connected":%v}`+"\n", i+1, code, connected)
	}
	return b.String()
}

func (f *fakeCluster) AcquireLease(context.Context, string, string, string, time.Duration) (cluster.LeaseAttempt, error) {
	return f.lease, nil
}
func (f *fakeCluster) RenewLease(context.Context, string, string, string) error { return nil }
func (f *fakeCluster) ReleaseLease(context.Context, string, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = true
	return nil
}

func (f *fakeCluster) Nodes(context.Context) ([]cluster.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var nodes []cluster.Node
	for _, n := range slices.Sorted(maps.Keys(f.nodes)) {
		node := *f.nodes[n]
		node.Annotations = maps.Clone(node.Annotations)
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func (f *fakeCluster) Cordon(_ context.Context, node string, annotations map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[node].Unschedulable = true
	maps.Copy(f.nodes[node].Annotations, annotations)
	return nil
}

func (f *fakeCluster) Uncordon(_ context.Context, node string, annotations []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[node].Unschedulable = false
	for _, a := range annotations {
		delete(f.nodes[node].Annotations, a)
	}
	return nil
}

func (f *fakeCluster) Pods(_ context.Context, _, selector string) ([]cluster.Pod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if selector == probeNameLabel+"="+probeNameValue {
		return f.leftover, nil
	}
	var pods []cluster.Pod
	for name, node := range f.replicas {
		pods = append(pods, cluster.Pod{Name: name, Node: node, Running: true})
	}
	return pods, nil
}

func (f *fakeCluster) PodsOnNode(context.Context, string) ([]cluster.Pod, error) { return nil, nil }

func (f *fakeCluster) CreatePod(_ context.Context, pod *corev1.Pod) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes[pod.Name] = pod.Spec.NodeSelector["kubernetes.io/hostname"]
	return nil
}

func (f *fakeCluster) PodLog(_ context.Context, _, pod, _ string) ([]byte, error) {
	return []byte(f.probeLog), nil
}

func (f *fakeCluster) DeletePod(_ context.Context, _, pod string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.probes, pod)
	f.leftover = slices.DeleteFunc(f.leftover, func(p cluster.Pod) bool { return p.Name == pod })
	return nil
}

func (f *fakeCluster) Rollout(context.Context, string, string) (cluster.Rollout, error) {
	n := int32(len(f.replicas))
	return cluster.Rollout{Wanted: n, Updated: n, Available: n, Total: n, Observed: true, Selector: "app=api", TemplateLabels: map[string]string{"app": "api"}}, nil
}

func (f *fakeCluster) PDBFor(context.Context, string, map[string]string) (cluster.PDB, bool, error) {
	if f.pdb == nil {
		return cluster.PDB{}, false, nil
	}
	return *f.pdb, true, nil
}

func (f *fakeCluster) Drain(_ context.Context, node string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drained = append(f.drained, node)
	for pod, n := range f.replicas {
		if n != node {
			continue
		}
		for _, other := range slices.Sorted(maps.Keys(f.nodes)) {
			if other != node && !f.nodes[other].Unschedulable && !slices.Contains(slices.Collect(maps.Values(f.replicas)), other) {
				f.replicas[pod] = other
				break
			}
		}
	}
	return nil
}

func gate(f *fakeCluster) *Gate {
	return &Gate{Cluster: f, Cfg: Config{
		Namespace: "docfind", Deployment: "docfind-api", RunID: "nasz", ProbeImage: "probe:1", ServiceURL: "http://svc",
		MinRequests: 100, RolloutWait: time.Second, LeaseDuration: time.Minute, LeaseRenew: time.Hour,
	}}
}

// TestRun: drain każdego węzła, na którym stoi replika — także tego, na
// który replika przeszła po wcześniejszym drainie — sonda na innym węźle,
// a po biegu węzły przywrócone, sondy usunięte i blokada zwolniona.
func TestRun(t *testing.T) {
	quiet(t)
	f := newCluster("agent-0", "agent-1")
	rep, err := gate(f).Run(context.Background())
	if err != nil {
		t.Fatalf("%v (%+v)", err, rep)
	}
	if !slices.Equal(f.drained, []string{"agent-0", "agent-1", "server-0"}) || len(rep.Drains) != 3 {
		t.Errorf("drainy %v, raport %+v", f.drained, rep.Drains)
	}
	for _, d := range rep.Drains {
		if d.ProbeNode == d.Node || d.Summary.Total != 150 || d.Summary.Failed != 0 {
			t.Errorf("drain %+v", d)
		}
	}
	for _, n := range f.nodes {
		if n.Unschedulable || len(n.Annotations) > 0 {
			t.Errorf("węzeł %s zostawiony: %+v", n.Name, n)
		}
	}
	if len(f.probes) != 0 || !f.released {
		t.Errorf("sondy %v, blokada zwolniona %v", f.probes, f.released)
	}

	// DRAIN_NODE: tylko wskazany węzeł.
	f = newCluster("agent-0", "agent-1")
	g := gate(f)
	g.Cfg.Node = "agent-1"
	if _, err := g.Run(context.Background()); err != nil || !slices.Equal(f.drained, []string{"agent-1"}) {
		t.Errorf("jeden węzeł: %v, drainy %v", err, f.drained)
	}
}

func TestRunRejects(t *testing.T) {
	quiet(t)
	tests := []struct {
		name    string
		prepare func(f *fakeCluster, g *Gate)
		want    string
	}{
		{"nieudane żądania w trakcie drainu", func(f *fakeCluster, _ *Gate) { f.probeLog = requests(150, 2) }, "2 z 152 żądań nie powiodło się"},
		{"za mało żądań", func(f *fakeCluster, _ *Gate) { f.probeLog = requests(20, 0) }, "tylko 20 żądań"},
		{"repliki na jednym węźle", func(f *fakeCluster, _ *Gate) { f.replicas = map[string]string{"api-0": "agent-0", "api-1": "agent-0"} }, "repliki API na 1 węźle"},
		{"brak PDB", func(f *fakeCluster, _ *Gate) { f.pdb = nil }, "brak PodDisruptionBudget"},
		{"PDB nie dopuszcza zakłócenia", func(f *fakeCluster, _ *Gate) { f.pdb.DisruptionsAllowed = 0 }, "nie dopuszcza teraz zakłócenia"},
		{"cudzy cordon", func(f *fakeCluster, _ *Gate) { f.nodes["server-0"].Unschedulable = true }, "węzły odcięte spoza bramki: server-0"},
		{"inny bieg trwa", func(f *fakeCluster, _ *Gate) {
			f.lease = cluster.LeaseAttempt{Holder: "inny", Expires: time.Now().Add(time.Minute)}
		}, "trwa inny bieg bramki drainu (inny"},
		{"węzeł bez repliki", func(_ *fakeCluster, g *Gate) { g.Cfg.Node = "server-0" }, "na węźle server-0 nie ma repliki API"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newCluster("agent-0", "agent-1")
			g := gate(f)
			tt.prepare(f, g)
			rep, err := g.Run(context.Background())
			if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("oczekiwano niespełnionego z %q, jest %v", tt.want, err)
			}
			if !slices.ContainsFunc(rep.Unmet, func(u string) bool { return strings.Contains(u, tt.want) }) {
				t.Errorf("raport bez powodu: %v", rep.Unmet)
			}
			// Sprzątanie po porażce: nasz cordon zdjęty, sondy usunięte.
			for _, n := range f.nodes {
				if n.Annotations[AnnotationCordonedBy] == CordonedByGate {
					t.Errorf("węzeł %s zostawiony przez bramkę", n.Name)
				}
			}
			if len(f.probes) != 0 {
				t.Errorf("sondy zostały: %v", f.probes)
			}
		})
	}
}

// TestRepairPrevious: węzeł i sonda zostawione przez bieg zabity bez
// sprzątania są przywracane, a cudzy cordon zostaje nietknięty.
func TestRepairPrevious(t *testing.T) {
	quiet(t)
	f := newCluster("agent-0", "agent-1")
	f.nodes["agent-0"].Unschedulable = true
	f.nodes["agent-0"].Annotations = map[string]string{AnnotationCordonedBy: CordonedByGate, AnnotationCordonedRun: "stary", AnnotationCordonedAt: "2026-10-07T08:27:43Z"}
	f.nodes["server-0"].Unschedulable = true
	f.leftover = []cluster.Pod{{Name: "drain-probe-stary-1", Labels: map[string]string{runLabel: "stary"}}}
	g := gate(f)
	repaired, err := g.repairPrevious(context.Background())
	if err != nil || !slices.Equal(repaired, []string{"węzeł agent-0", "sonda drain-probe-stary-1"}) {
		t.Errorf("naprawione: %v, %v", repaired, err)
	}
	if f.nodes["agent-0"].Unschedulable || !f.nodes["server-0"].Unschedulable {
		t.Errorf("węzły po naprawie: %+v %+v", f.nodes["agent-0"], f.nodes["server-0"])
	}
	foreign, err := g.foreignCordons(context.Background())
	if err != nil || !slices.Equal(foreign, []string{"server-0"}) {
		t.Errorf("cudzy cordon: %v, %v", foreign, err)
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

type goBuilder struct{}

func (goBuilder) Build(_ context.Context, pkg, output string, env ...string) error {
	if pkg != "./cmd/drain-probe" || !slices.Contains(env, "GOOS=linux") {
		return fmt.Errorf("build %s %v", pkg, env)
	}
	return writeFile(output, "binarka")
}

type importer struct{ imported []string }

func (i *importer) Import(_ context.Context, cluster, image string) error {
	i.imported = append(i.imported, cluster+" "+image)
	return nil
}

// TestPrepareProbe: ta sama binarka daje ten sam tag, a archiwum trafia do
// węzłów klastra środowiska.
func TestPrepareProbe(t *testing.T) {
	quiet(t)
	imp := &importer{}
	p := ProbeImage{Go: goBuilder{}, Importer: imp, Cluster: "docfind"}
	first, err := p.Prepare(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Prepare(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, ProbeRepository+":local-") {
		t.Errorf("tagi %s i %s", first, second)
	}
	if len(imp.imported) != 2 || !strings.HasPrefix(imp.imported[0], "docfind ") || !strings.HasSuffix(imp.imported[0], "drain-probe.tar") {
		t.Errorf("import: %v", imp.imported)
	}
}
