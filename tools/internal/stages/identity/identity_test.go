package identity

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// fakeCluster — Deployment z replikami, pody z obrazami i odpowiedzi
// /version po nazwie poda.
type fakeCluster struct {
	replicas int32
	pods     []cluster.Pod
	versions map[string]string
}

func (f fakeCluster) Rollout(context.Context, string, string) (cluster.Rollout, error) {
	return cluster.Rollout{Wanted: f.replicas, Selector: "app=api"}, nil
}

func (f fakeCluster) Pods(_ context.Context, _, selector string) ([]cluster.Pod, error) {
	if selector != "app=api" {
		return nil, errors.New("zły selektor")
	}
	return f.pods, nil
}

func (f fakeCluster) ProxyGet(_ context.Context, _, pod, _, path string) ([]byte, error) {
	if path != "/version" {
		return nil, errors.New("zła ścieżka")
	}
	v, ok := f.versions[pod]
	if !ok {
		return nil, errors.New("connection refused")
	}
	return []byte(v), nil
}

func pod(name, image string) cluster.Pod {
	return cluster.Pod{Name: name, Node: "w1", Images: map[string]string{"api": image}, Running: true}
}

func TestVerify(t *testing.T) {
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	defer func() { cli.Out, cli.Err = oldOut, oldErr }()

	want := artifact.Expected{Image: "img:1", Version: "1.0.0", Commit: "abc"}
	ok := `{"version":"1.0.0","commit":"abc"}`
	terminating := pod("stary", "img:0")
	terminating.Terminating = true
	tests := []struct {
		name     string
		replicas int32
		pods     []cluster.Pod
		versions map[string]string
		wantCode int
	}{
		{"każdy pod zgodny", 2, []cluster.Pod{pod("a", "img:1"), pod("b", "img:1")}, map[string]string{"a": ok, "b": ok}, cli.ExitOK},
		{"wygaszany pod pominięty", 2, []cluster.Pod{pod("a", "img:1"), pod("b", "img:1"), terminating}, map[string]string{"a": ok, "b": ok}, cli.ExitOK},
		{"inny obraz na jednym podzie", 2, []cluster.Pod{pod("a", "img:1"), pod("b", "img:0")}, map[string]string{"a": ok, "b": ok}, cli.ExitUnmet},
		{"inny commit na podzie", 2, []cluster.Pod{pod("a", "img:1"), pod("b", "img:1")}, map[string]string{"a": ok, "b": `{"version":"1.0.0","commit":"zzz"}`}, cli.ExitUnmet},
		{"pod nie odpowiada", 1, []cluster.Pod{pod("a", "img:1")}, nil, cli.ExitUnmet},
		{"mniej podów niż replik", 2, []cluster.Pod{pod("a", "img:1")}, map[string]string{"a": ok}, cli.ExitUnmet},
	}
	for _, tt := range tests {
		results, err := Verify(context.Background(), fakeCluster{tt.replicas, tt.pods, tt.versions}, "docfind", "docfind-api", "api", want)
		if code := cli.ExitCode(err); code != tt.wantCode {
			t.Errorf("%s: kod %d, oczekiwano %d (%v)", tt.name, code, tt.wantCode, err)
		}
		if tt.wantCode == cli.ExitOK && len(results) != int(tt.replicas) {
			t.Errorf("%s: wyniki %+v", tt.name, results)
		}
	}
}

type registry struct{}

func (registry) Inspect(_ context.Context, ref string) (artifact.Publication, error) {
	return artifact.Publication{Artifact: artifact.Artifact{Version: "0.4.0", Commit: "z-rejestru"}}, nil
}

type versions struct{}

func (versions) Version(context.Context) (string, error) { return "0.4.1-dev.1+abc1234", nil }
func (versions) Commit(context.Context) (string, error)  { return "z-gita", nil }

// TestExpect: obraz po digeście — punkt odniesienia z rejestru, obraz
// lokalny — z gita.
func TestExpect(t *testing.T) {
	oldOut := cli.Out
	cli.Out = io.Discard
	defer func() { cli.Out = oldOut }()
	ctx := context.Background()
	pinned := "ghcr.io/x/api:0.4.0@sha256:" + "d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0d0"
	got, err := Expect(ctx, pinned, registry{}, versions{})
	if err != nil || got.Commit != "z-rejestru" || got.Image != pinned {
		t.Errorf("z rejestru: %+v, %v", got, err)
	}
	got, err = Expect(ctx, "ghcr.io/x/api:local-abc", registry{}, versions{})
	if err != nil || got.Commit != "z-gita" || got.Version != "0.4.1-dev.1+abc1234" {
		t.Errorf("z gita: %+v, %v", got, err)
	}
}
