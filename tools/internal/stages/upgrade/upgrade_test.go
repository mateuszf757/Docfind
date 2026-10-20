package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/deploy"
	"github.com/mateuszf757/Docfind/tools/internal/stages/ingress"
)

var (
	older = artifact.Expected{Image: "ghcr.io/o/docfind-api:bbbbbbbbbbbb@sha256:" + strings.Repeat("b", 64), Version: "0.4.0", Commit: strings.Repeat("b", 40)}
	n     = artifact.Expected{Image: "ghcr.io/o/docfind-api:cccccccccccc", Version: "0.4.1-dev.10+ccccccc", Commit: strings.Repeat("c", 40)}
	prev  = deploy.Previous{Chart: "0.4.1-dev.9", Want: artifact.Expected{
		Image: "ghcr.io/o/docfind-api:aaaaaaaaaaaa@sha256:" + strings.Repeat("a", 64), Version: "0.4.1-dev.9+aaaaaaa", Commit: strings.Repeat("a", 40),
	}}
)

// world — release z historią rewizji (rewizja r to revisions[r-1]) i dwa
// pody, które od razu podają tożsamość bieżącej rewizji. Zapisuje
// kolejność wywołań, które coś zmieniają albo idą do rejestru.
type world struct {
	calls     []string
	revisions []artifact.Expected
	// fail — wywołanie, które się nie udaje; noisy — przejście z nieudanymi
	// żądaniami; stale — przejście, po którym pody zostają przy wersji
	// sprzed niego.
	fail, noisy, stale string
	override           *artifact.Expected
}

func (w *world) current() artifact.Expected {
	if w.override != nil {
		return *w.override
	}
	return w.revisions[len(w.revisions)-1]
}

func (w *world) call(format string, args ...any) error {
	c := fmt.Sprintf(format, args...)
	w.calls = append(w.calls, c)
	switch {
	case c != w.fail:
		return nil
	case strings.HasPrefix(c, "admit"):
		return cli.Unmet("wśród 10 najnowszych wersji charta nie ma obrazu z commita innego niż cccccccccccc")
	default:
		return errors.New(c + ": exit status 1")
	}
}

func (w *world) Revision(_ context.Context, _ cluster.Target, namespace, release string) (int, error) {
	if namespace != "docfind" || release != "docfind" {
		return 0, fmt.Errorf("release %s/%s", namespace, release)
	}
	return len(w.revisions), nil
}

func (w *world) Rollback(_ context.Context, _ cluster.Target, _, _ string, revision int, timeout time.Duration) error {
	if err := w.call("rollback %d (%v)", revision, timeout); err != nil {
		return err
	}
	w.revisions = append(w.revisions, w.revisions[revision-1])
	return nil
}

func (w *world) AdmitPrevious(_ context.Context, commit string) (deploy.Previous, error) {
	return prev, w.call("admit %s", artifact.ShortCommit(commit))
}

func (w *world) InstallPrevious(_ context.Context, p deploy.Previous) error {
	if err := w.call("upgrade %s", p.Chart); err != nil {
		return err
	}
	w.revisions = append(w.revisions, p.Want)
	return nil
}

func (w *world) UnderTraffic(ctx context.Context, what, deployment string, _ time.Duration, action func(context.Context) error) (ingress.RolloutReport, error) {
	_ = w.call("ruch: %s (%s)", what, deployment)
	rep := ingress.RolloutReport{Action: what, Deployment: deployment, Unmet: []string{}}
	before := w.current()
	if err := action(ctx); err != nil {
		return rep, err
	}
	if what == w.stale {
		w.override = &before
	}
	rep.Requests = 100
	if what == w.noisy {
		rep.Failed = 2
		err := cli.Unmet("%s: 2 z 100 żądań nie powiodło się", what)
		rep.Unmet = append(rep.Unmet, err.Error())
		return rep, err
	}
	return rep, nil
}

func (w *world) Rollout(context.Context, string, string) (cluster.Rollout, error) {
	return cluster.Rollout{Wanted: 2, Selector: "app=api"}, nil
}

func (w *world) Pods(context.Context, string, string) ([]cluster.Pod, error) {
	var pods []cluster.Pod
	for _, name := range []string{"api-1", "api-2"} {
		pods = append(pods, cluster.Pod{Name: name, Node: "w1", Images: map[string]string{"api": w.current().Image}, Running: true})
	}
	return pods, nil
}

func (w *world) ProxyGet(context.Context, string, string, string, string) ([]byte, error) {
	return json.Marshal(map[string]string{"version": w.current().Version, "commit": w.current().Commit})
}

func gate(w *world) *Gate {
	return &Gate{
		Releases: w, Charts: w, Traffic: w, Pods: w,
		Namespace: "docfind", Release: "docfind", Deployment: "docfind-api", Container: "api", Wait: 3 * time.Minute,
	}
}

func quiet(t *testing.T) {
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

// TestRun: cztery przejścia pod ruchem — upgrade do N-1, upgrade N-1 → N
// rollbackiem do rewizji N, rollback do rewizji N-1 i powrót do N — każde
// z tożsamością wersji docelowej na podach; klaster kończy na N.
func TestRun(t *testing.T) {
	quiet(t)
	w := &world{revisions: []artifact.Expected{older, n}}
	rep, err := gate(w).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"admit cccccccccccc",
		"ruch: upgrade do N-1 (docfind-api)", "upgrade 0.4.1-dev.9",
		"ruch: upgrade N-1 → N (docfind-api)", "rollback 2 (3m0s)",
		"ruch: rollback N → N-1 (docfind-api)", "rollback 3 (3m0s)",
		"ruch: powrót do N (docfind-api)", "rollback 2 (3m0s)",
	}
	if !slices.Equal(w.calls, wantCalls) {
		t.Errorf("wywołania:\n%q\n   oczekiwano\n%q", w.calls, wantCalls)
	}
	if w.current() != n || len(w.revisions) != 6 {
		t.Errorf("klaster po bramce: %+v, rewizji %d", w.current(), len(w.revisions))
	}
	wantN := Version{Revision: 2, Image: n.Image, Version: n.Version, Commit: n.Commit}
	wantPrev := Version{Revision: 3, Chart: prev.Chart, Image: prev.Want.Image, Version: prev.Want.Version, Commit: prev.Want.Commit}
	if rep.N != wantN || rep.Previous != wantPrev || rep.Restore != "" || len(rep.Unmet) != 0 {
		t.Errorf("raport: %+v", rep)
	}
	commands := []string{"helm upgrade docfind --version 0.4.1-dev.9", "helm rollback docfind 2", "helm rollback docfind 3", "helm rollback docfind 2"}
	if len(rep.Steps) != len(commands) {
		t.Fatalf("przejścia: %+v", rep.Steps)
	}
	for i, s := range rep.Steps {
		if s.Command != commands[i] || len(s.Pods) != 2 || s.Pods[0].Problem != "" {
			t.Errorf("przejście %d (%s): %s, pody %+v", i, s.Action, s.Command, s.Pods)
		}
	}
}

func TestRunUnmet(t *testing.T) {
	quiet(t)
	tests := []struct {
		name  string
		world world
		code  int
		// steps — przejścia w raporcie; restore — klaster mógł zostać poza N.
		steps   int
		restore bool
		want    string
	}{
		{
			name:  "brak N-1 w rejestrze",
			world: world{fail: "admit cccccccccccc"},
			code:  cli.ExitUnmet, want: "nie ma obrazu z commita innego niż",
		},
		{
			name:  "nieudane żądania przy rollbacku",
			world: world{noisy: "rollback N → N-1"},
			code:  cli.ExitUnmet, steps: 3, restore: true, want: "rollback N → N-1: 2 z 100 żądań nie powiodło się",
		},
		{
			name:  "pody zostają przy N-1 po upgrade N-1 → N",
			world: world{stale: "upgrade N-1 → N"},
			code:  cli.ExitUnmet, steps: 2, restore: true, want: "upgrade N-1 → N: na klastrze działa coś innego",
		},
		{
			name:  "helm rollback się nie udaje",
			world: world{fail: "rollback 3 (3m0s)"},
			code:  cli.ExitFailure, steps: 3, restore: true, want: "rollback N → N-1: rollback 3 (3m0s): exit status 1",
		},
		{
			name:  "helm upgrade do N-1 się nie udaje",
			world: world{fail: "upgrade 0.4.1-dev.9"},
			code:  cli.ExitFailure, steps: 1, restore: true, want: "upgrade do N-1: upgrade 0.4.1-dev.9: exit status 1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.world
			w.revisions = []artifact.Expected{older, n}
			rep, err := gate(&w).Run(context.Background())
			if cli.ExitCode(err) != tc.code || !strings.Contains(fmt.Sprint(err), tc.want) {
				t.Fatalf("Run: %v (kod %d), oczekiwano kodu %d i fragmentu %q", err, cli.ExitCode(err), tc.code, tc.want)
			}
			if len(rep.Steps) != tc.steps {
				t.Errorf("przejścia w raporcie: %d, oczekiwano %d", len(rep.Steps), tc.steps)
			}
			if restore := rep.Restore == "helm rollback docfind 2 --namespace docfind"; restore != tc.restore {
				t.Errorf("polecenie powrotu: %q", rep.Restore)
			}
			if unmet := len(rep.Unmet) == 1 && strings.Contains(rep.Unmet[0], tc.want); unmet != (tc.code == cli.ExitUnmet) {
				t.Errorf("niespełnione w raporcie: %q", rep.Unmet)
			}
			if tc.code == cli.ExitUnmet && tc.steps > 0 && len(rep.Steps[tc.steps-1].Unmet) == 0 {
				t.Errorf("przejście bez powodu w raporcie: %+v", rep.Steps[tc.steps-1])
			}
		})
	}
}
