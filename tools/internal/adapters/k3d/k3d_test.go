package k3d

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

func TestExists(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"k3d cluster get jest": {},
		"k3d cluster get brak": {Code: 1},
		"k3d cluster get zly":  {Err: context.DeadlineExceeded},
	}}
	k := K3d{Runner: f}
	ctx := context.Background()
	if ok, err := k.Exists(ctx, "jest"); !ok || err != nil {
		t.Errorf("istniejący: %v, %v", ok, err)
	}
	if ok, err := k.Exists(ctx, "brak"); ok || err != nil {
		t.Errorf("brak klastra to wynik, nie awaria: %v, %v", ok, err)
	}
	if _, err := k.Exists(ctx, "zly"); err == nil {
		t.Error("awaria k3d jako brak klastra")
	}
}

// TestCreate: konfiguracja trafia do pliku, rootless dokłada bramkę
// kubeleta, a logi węzłów są zbierane w trakcie tworzenia.
func TestCreate(t *testing.T) {
	quiet(t)
	var created []string
	var config string
	// Tworzenie trwa, dopóki obserwator nie zacznie zbierać logu węzła —
	// prawdziwy k3d tworzy klaster dziesiątki sekund.
	logged := make(chan struct{})
	var once sync.Once
	runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
		switch {
		case c.Name == "k3d":
			created = c.Args
			raw, err := os.ReadFile(c.Args[slices.Index(c.Args, "--config")+1])
			config = string(raw)
			select {
			case <-logged:
			case <-time.After(5 * time.Second):
			}
			return proc.Result{}, err
		case c.Name == "docker" && c.Args[0] == "ps":
			// Tylko węzły tego klastra — po etykiecie, nie po przedrostku nazwy.
			if !slices.Contains(c.Args, "label=k3d.cluster=x") {
				return proc.Result{Stdout: []byte("k3d-x-staging-server-0\n")}, nil
			}
			return proc.Result{Stdout: []byte("k3d-x-server-0\n")}, nil
		case c.Name == "docker" && c.Args[0] == "logs":
			_, _ = io.WriteString(c.Stdout, "level=info start\n")
			once.Do(func() { close(logged) })
		}
		return proc.Result{}, nil
	})
	logDir := filepath.Join(t.TempDir(), "logi")
	err := K3d{Runner: runner}.Create(context.Background(), cluster.LocalCluster{
		Name: "x", Config: []byte(`{"kind":"Simple"}`), Image: "rancher/k3s:v1@sha256:abc", UserNamespace: true, Timeout: 180 * time.Second, LogDir: logDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(created, " ")
	for _, want := range []string{"cluster create --config", "--image rancher/k3s:v1@sha256:abc --timeout 3m0s", "KubeletInUserNamespace=true@agent:*"} {
		if !strings.Contains(args, want) {
			t.Errorf("k3d bez %q: %s", want, args)
		}
	}
	if config != `{"kind":"Simple"}` {
		t.Errorf("konfiguracja: %q", config)
	}
	if _, err := os.Stat(filepath.Join(logDir, "k3d-x-server-0.log")); err != nil {
		t.Errorf("log węzła nie zapisany: %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "k3d-x-staging-server-0.log")); err == nil {
		t.Error("log węzła cudzego klastra")
	}
}

// TestNodeLogs: węzły klastra po etykiecie k3d — klaster docfind-staging
// ma nazwy kontenerów zaczynające się jak docfind, więc przedrostek nazwy
// zebrałby logi cudzego klastra.
func TestNodeLogs(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"docker ps -a --format {{.Names}} --filter label=k3d.cluster=x": {Stdout: "k3d-x-server-0\nk3d-x-agent-0\n"},
		"docker logs --timestamps k3d-x-server-0":                       {Stdout: "log serwera"},
		"docker logs --timestamps k3d-x-agent-0":                        {Code: 1},
	}}
	dir := t.TempDir()
	missing := K3d{Runner: f}.NodeLogs(context.Background(), "x", dir)
	if len(missing) != 1 || !strings.HasPrefix(missing[0], "k3d-x-agent-0") {
		t.Errorf("braki: %v", missing)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "k3d-x-server-0.log")); string(raw) != "log serwera" {
		t.Errorf("log serwera: %q", raw)
	}
}
