// Package evidence zbiera dowody z klastra po biegu bramek: stan węzłów
// i podów, zdarzenia, logi kontenerów (także poprzednich instancji po
// restarcie), zasoby platformy i logi kontenerów węzłów k3d.
//
// W CI klaster znika razem z maszyną runnera. Bramka, która zawiodła
// o drugiej w nocy, bez tych plików zostawia tylko „0 z 3 drainów" —
// a przyczyna (eksmisja CoreDNS, OOM, wyzwanie ACME) jest w stanie klastra.
// Zbieranie jest najlepszym możliwym wysiłkiem: brak jednego elementu
// (klaster nie wstał, CRD nie zainstalowane) jest zapisany i nie przerywa
// zbierania reszty.
package evidence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/mateuszf757/Docfind/tools/internal/kube"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// Collector — zbieranie dowodów z jednego klastra.
type Collector struct {
	// Core — klient klastra; nil, gdy połączenie się nie udało (zbierane są
	// wtedy tylko logi kontenerów węzłów).
	Core   kubernetes.Interface
	Runner proc.Runner
	Target kube.Target
	// Cluster — nazwa klastra k3d; kontenery węzłów to k3d-<Cluster>-….
	Cluster string
	// Namespaces — przestrzenie nazw, z których brane są opisy podów i logi.
	Namespaces []string
	Dir        string
}

// Collect zapisuje dowody do Dir i zwraca listę tego, czego nie udało się
// zebrać.
func (c Collector) Collect(ctx context.Context) ([]string, error) {
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		return nil, err
	}
	var missing []string
	note := func(what string, err error) {
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s: %v", what, err))
		}
	}

	if c.Core != nil {
		kubectl := func(file string, args ...string) {
			note(file, c.toFile(ctx, file, "kubectl", append(args, c.Target.KubectlArgs()...)...))
		}
		kubectl("nodes.txt", "get", "nodes", "-o", "wide")
		kubectl("pods.txt", "get", "pods", "-A", "-o", "wide")
		kubectl("events.txt", "get", "events", "-A", "--sort-by=.lastTimestamp")
		kubectl("platform.yaml", "get", "gateways,httproutes,certificates,certificaterequests,orders,challenges,clusterissuers", "-A", "-o", "yaml")
		for _, ns := range c.Namespaces {
			kubectl(filepath.Join(ns, "describe-pods.txt"), "describe", "pods", "-n", ns)
			pods, err := c.Core.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				note(ns+"/pods", err)
				continue
			}
			for _, pod := range pods.Items {
				kubectl(filepath.Join(ns, pod.Name+".log"), "logs", "-n", ns, pod.Name, "--all-containers", "--prefix", "--timestamps")
				restarted := false
				for _, s := range pod.Status.ContainerStatuses {
					restarted = restarted || s.RestartCount > 0
				}
				if restarted {
					kubectl(filepath.Join(ns, pod.Name+".previous.log"), "logs", "-n", ns, pod.Name, "--all-containers", "--prefix", "--timestamps", "--previous")
				}
			}
		}
	} else {
		missing = append(missing, "stan klastra: brak połączenia z API")
	}

	res, err := c.Runner.Run(ctx, proc.Cmd{Name: "docker", Args: []string{"ps", "-a", "--format", "{{.Names}}", "--filter", "name=^k3d-" + c.Cluster + "-"}})
	note("kontenery węzłów", err)
	if err == nil {
		for _, node := range strings.Fields(string(res.Stdout)) {
			note(node, c.toFile(ctx, filepath.Join("nodes", node+".log"), "docker", "logs", "--timestamps", node))
		}
	}
	return missing, nil
}

// toFile zapisuje stdout i stderr polecenia do pliku w Dir.
func (c Collector) toFile(ctx context.Context, rel, name string, args ...string) (err error) {
	path := filepath.Join(c.Dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = c.Runner.Run(ctx, proc.Cmd{Name: name, Args: args, Stdout: f, Stderr: f})
	return err
}
