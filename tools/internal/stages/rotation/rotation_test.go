package rotation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	store "github.com/mateuszf757/Docfind/tools/internal/core/secretstore"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

// world — OpenBao i pody: każdy pod widzi zapisany sekret dopiero po lag
// pytaniach (ESO, kubelet). open — /metrics bez tokenu.
type world struct {
	vault   []string
	view    map[string][]string
	pending map[string]int
	lag     map[string]int
	open    bool
	pods    []cluster.Pod
	restart string
}

func newWorld(lag map[string]int) *world {
	w := &world{vault: []string{"stary"}, view: map[string][]string{}, pending: map[string]int{}, lag: lag}
	for i, name := range []string{"api-a", "api-b"} {
		w.pods = append(w.pods, cluster.Pod{Name: name, Ready: true, Running: true, UID: fmt.Sprintf("uid-%d", i)})
		w.view[name] = []string{"stary"}
	}
	return w
}

func (w *world) Pods(context.Context, string, string) ([]cluster.Pod, error) {
	out := append([]cluster.Pod(nil), w.pods...)
	for i := range out {
		if out[i].Name == w.restart && len(w.vault) == 1 && w.vault[0] != "stary" {
			out[i].Restarts = 1
		}
	}
	return out, nil
}
func (w *world) SecretData(context.Context, string, string) (map[string][]byte, error) {
	return map[string][]byte{"metrics-token": []byte(strings.Join(w.view["api-a"], "\n") + "\n")}, nil
}
func (w *world) PortForward(_ context.Context, _, pod string, _ int) (string, func(), error) {
	return pod, func() {}, nil
}
func (w *world) PutSecret(_ context.Context, account, role, path string, data map[string]sensitive.Value) (int, error) {
	if account != store.AppWriterAccount || role != store.AppWriterRole || path != store.AppMetricsPath {
		return 0, errors.New("nie ta rola albo ścieżka")
	}
	w.vault = strings.Split(data[store.AppMetricsField].Reveal(), "\n")
	for _, p := range w.pods {
		w.pending[p.Name] = w.lag[p.Name]
	}
	return len(w.vault), nil
}

// Status — pod odpowiada według swojego widoku sekretu, który dogania
// OpenBao po lag pytaniach.
func (w *world) Status(_ context.Context, pod, _ string, token sensitive.Value) (int, error) {
	if w.pending[pod] > 0 {
		w.pending[pod]--
	} else {
		w.view[pod] = w.vault
	}
	if w.open {
		return http.StatusOK, nil
	}
	for _, t := range w.view[pod] {
		if t == token.Reveal() {
			return http.StatusOK, nil
		}
	}
	return http.StatusUnauthorized, nil
}

func gate(w *world) Gate {
	return Gate{
		Cluster: w, Vault: w, HTTP: w, Namespace: "docfind", Selector: "app=api", Secret: "docfind-api-secrets", Key: "metrics-token",
		Port: 8000, Budget: 500 * time.Millisecond, Interval: time.Millisecond,
		NewToken: func() (sensitive.Value, error) { return sensitive.New("nowy"), nil },
	}
}

// TestRotation: obie fazy docierają do każdego poda, w OpenBao najpierw
// stary i nowy, na końcu tylko nowy.
func TestRotation(t *testing.T) {
	quiet(t)
	w := newWorld(map[string]int{"api-a": 3, "api-b": 7})
	rep, err := gate(w).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Phases) != 2 || len(rep.Phases[0].Seconds) != 2 || len(rep.Phases[1].Seconds) != 2 || rep.Phases[1].Version != 1 {
		t.Errorf("raport: %+v", rep)
	}
	if strings.Join(w.vault, ",") != "nowy" {
		t.Errorf("OpenBao na końcu: %v", w.vault)
	}
}

// TestRotationRefuses: otwarte /metrics (nowy token „działa" od razu),
// pod, do którego rotacja nie dociera, i restart w trakcie — warunek
// niespełniony.
func TestRotationRefuses(t *testing.T) {
	quiet(t)
	tests := []struct {
		name   string
		mutate func(*world)
		want   string
	}{
		{"metryki bez tokenu", func(w *world) { w.open = true }, "oczekiwano 401"},
		{"rotacja nie dociera", func(w *world) { w.lag["api-b"] = 1 << 30 }, "api-b"},
		{"restart w trakcie", func(w *world) { w.restart = "api-a" }, "rotacja bez restartu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(map[string]int{"api-a": 1, "api-b": 2})
			tt.mutate(w)
			_, err := gate(w).Run(context.Background())
			if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("kod %d: %v, oczekiwano %q", cli.ExitCode(err), err, tt.want)
			}
		})
	}
}
