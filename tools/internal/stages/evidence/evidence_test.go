package evidence

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

type fakeCluster struct{}

func (fakeCluster) Pods(context.Context, string, string) ([]cluster.Pod, error) {
	return []cluster.Pod{{Name: "api-1", Restarts: 2}, {Name: "api-2"}}, nil
}
func (fakeCluster) DumpNodes(_ context.Context, w io.Writer) error {
	_, err := io.WriteString(w, "węzły")
	return err
}
func (fakeCluster) DumpPods(_ context.Context, w io.Writer) error {
	_, err := io.WriteString(w, "pody")
	return err
}
func (fakeCluster) DumpEvents(_ context.Context, w io.Writer) error {
	_, err := io.WriteString(w, "zdarzenia")
	return err
}

// DumpPlatform zawodzi jak kubectl bez zainstalowanych CRD.
func (fakeCluster) DumpPlatform(context.Context, io.Writer) error {
	return errors.New("the server doesn't have a resource type \"gateways\"")
}
func (fakeCluster) DescribePods(_ context.Context, ns string, w io.Writer) error {
	_, err := io.WriteString(w, "opis "+ns)
	return err
}
func (fakeCluster) Logs(_ context.Context, _, pod string, previous bool, w io.Writer) error {
	_, err := io.WriteString(w, pod)
	return err
}

type nodes struct{ dir *string }

func (n nodes) NodeLogs(_ context.Context, cluster, dir string) []string {
	*n.dir = dir
	return []string{"k3d-" + cluster + "-agent-0: kontener usunięty"}
}

// TestCollect: poprzedni log tylko z kontenera po restarcie, brak jednego
// elementu (CRD platformy) zapisany, a nie przerywający reszty.
func TestCollect(t *testing.T) {
	dir := t.TempDir()
	var nodeDir string
	c := Collector{Cluster: fakeCluster{}, Nodes: nodes{&nodeDir}, Name: "docfind-ci", Namespaces: []string{"docfind"}, Dir: dir}
	missing, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 2 || !strings.HasPrefix(missing[0], "platform.yaml") || !strings.HasPrefix(missing[1], "k3d-docfind-ci-agent-0") {
		t.Errorf("braki: %v", missing)
	}
	for _, rel := range []string{"nodes.txt", "pods.txt", "events.txt", "docfind/describe-pods.txt", "docfind/api-1.log", "docfind/api-1.previous.log", "docfind/api-2.log"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("brak %s", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "docfind/api-2.previous.log")); err == nil {
		t.Error("poprzedni log kontenera bez restartu")
	}
	if nodeDir != filepath.Join(dir, "nodes") {
		t.Errorf("logi węzłów do %s", nodeDir)
	}
}

func TestCollectWithoutCluster(t *testing.T) {
	var nodeDir string
	missing, err := Collector{Nodes: nodes{&nodeDir}, Name: "docfind-ci", Dir: t.TempDir()}.Collect(context.Background())
	if err != nil || len(missing) != 2 || !strings.Contains(missing[0], "brak połączenia") {
		t.Errorf("braki %v, błąd %v", missing, err)
	}
}
