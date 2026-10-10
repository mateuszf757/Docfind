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
	"io"
	"os"
	"path/filepath"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

// Cluster — zrzuty stanu klastra (adapters/kube).
type Cluster interface {
	Pods(ctx context.Context, namespace, selector string) ([]cluster.Pod, error)
	DumpNodes(ctx context.Context, w io.Writer) error
	DumpPods(ctx context.Context, w io.Writer) error
	DumpEvents(ctx context.Context, w io.Writer) error
	DumpPlatform(ctx context.Context, w io.Writer) error
	DescribePods(ctx context.Context, namespace string, w io.Writer) error
	Logs(ctx context.Context, namespace, pod string, previous bool, w io.Writer) error
}

// Nodes — logi kontenerów węzłów klastra lokalnego (adapters/k3d).
type Nodes interface {
	NodeLogs(ctx context.Context, cluster, dir string) []string
}

// Collector — zbieranie dowodów z jednego klastra.
type Collector struct {
	// Cluster — nil, gdy połączenie się nie udało (zbierane są wtedy tylko
	// logi kontenerów węzłów).
	Cluster Cluster
	Nodes   Nodes
	// Name — nazwa klastra k3d.
	Name string
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
	dump := func(rel string, write func(io.Writer) error) {
		note(rel, toFile(filepath.Join(c.Dir, rel), write))
	}

	if c.Cluster != nil {
		dump("nodes.txt", func(w io.Writer) error { return c.Cluster.DumpNodes(ctx, w) })
		dump("pods.txt", func(w io.Writer) error { return c.Cluster.DumpPods(ctx, w) })
		dump("events.txt", func(w io.Writer) error { return c.Cluster.DumpEvents(ctx, w) })
		dump("platform.yaml", func(w io.Writer) error { return c.Cluster.DumpPlatform(ctx, w) })
		for _, ns := range c.Namespaces {
			dump(filepath.Join(ns, "describe-pods.txt"), func(w io.Writer) error { return c.Cluster.DescribePods(ctx, ns, w) })
			pods, err := c.Cluster.Pods(ctx, ns, "")
			if err != nil {
				note(ns+"/pods", err)
				continue
			}
			for _, pod := range pods {
				dump(filepath.Join(ns, pod.Name+".log"), func(w io.Writer) error { return c.Cluster.Logs(ctx, ns, pod.Name, false, w) })
				if pod.Restarts > 0 {
					dump(filepath.Join(ns, pod.Name+".previous.log"), func(w io.Writer) error { return c.Cluster.Logs(ctx, ns, pod.Name, true, w) })
				}
			}
		}
	} else {
		missing = append(missing, "stan klastra: brak połączenia z API")
	}
	missing = append(missing, c.Nodes.NodeLogs(ctx, c.Name, filepath.Join(c.Dir, "nodes"))...)
	return missing, nil
}

// toFile zapisuje wynik zrzutu do pliku.
func toFile(path string, write func(io.Writer) error) (err error) {
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
	return write(f)
}
