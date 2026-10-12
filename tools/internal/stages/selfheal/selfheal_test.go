package selfheal

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

// argo — klaster z Argo CD: ręczna zmiana wraca do stanu z charta po
// lag sprawdzeniach (lag < 0 — nie wraca nigdy).
type argo struct {
	app      cluster.Application
	replicas int32
	pdb      bool
	lag      int
	// polls — sprawdzenia od ostatniej zmiany; changes — zmiany ręczne.
	polls   int
	changes []string
}

func (a *argo) Application(context.Context, string, string) (cluster.Application, error) {
	return a.app, nil
}

func (a *argo) heal() {
	a.polls++
	if a.lag >= 0 && a.polls > a.lag {
		a.replicas, a.pdb = 2, true
	}
}

func (a *argo) Rollout(context.Context, string, string) (cluster.Rollout, error) {
	a.heal()
	return cluster.Rollout{Wanted: a.replicas}, nil
}

func (a *argo) ScaleDeployment(_ context.Context, _, name string, replicas int32) error {
	a.replicas, a.polls = replicas, 0
	a.changes = append(a.changes, "scale "+name)
	return nil
}

func (a *argo) PodDisruptionBudgetExists(context.Context, string, string) (bool, error) {
	a.heal()
	return a.pdb, nil
}

func (a *argo) DeletePodDisruptionBudget(_ context.Context, _, name string) error {
	a.pdb, a.polls = false, 0
	a.changes = append(a.changes, "delete pdb "+name)
	return nil
}

func gate(a *argo) Gate {
	return Gate{Cluster: a, ArgoNamespace: "argocd", Application: "docfind", Namespace: "docfind",
		Deployment: "docfind-api", PDB: "docfind-api", Budget: 50 * time.Millisecond, Interval: time.Millisecond}
}

func healthy() cluster.Application {
	return cluster.Application{Sync: "Synced", Health: "Healthy", Revision: "0.4.1-dev.3"}
}

func TestRun(t *testing.T) {
	quiet(t)
	a := &argo{app: healthy(), replicas: 2, pdb: true, lag: 3}
	rep, err := gate(a).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Najpierw replika więcej (bez utraty przepustowości), potem PDB.
	if strings.Join(a.changes, ",") != "scale docfind-api,delete pdb docfind-api" || len(rep.Changes) != 2 || rep.Revision != "0.4.1-dev.3" {
		t.Errorf("zmiany %v, raport %+v", a.changes, rep)
	}
	if !strings.Contains(rep.Changes[0].Change, "o jedną replikę więcej") {
		t.Errorf("pierwsza zmiana: %+v", rep.Changes[0])
	}
}

func TestRunRejects(t *testing.T) {
	quiet(t)
	t.Run("Argo nie przywraca", func(t *testing.T) {
		a := &argo{app: healthy(), replicas: 2, pdb: true, lag: -1}
		rep, err := gate(a).Run(context.Background())
		if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "nie przywrócił") || rep.Changes[0].RestoredSeconds != 0 {
			t.Errorf("%v, %+v", err, rep)
		}
		// Bez przywrócenia pierwszej zmiany druga nie jest wprowadzana.
		if len(a.changes) != 1 {
			t.Errorf("zmiany: %v", a.changes)
		}
	})
	t.Run("aplikacja niezsynchronizowana na starcie", func(t *testing.T) {
		app := healthy()
		app.Sync = "OutOfSync"
		a := &argo{app: app, replicas: 2, pdb: true}
		_, err := gate(a).Run(context.Background())
		if cli.ExitCode(err) != cli.ExitUnmet || len(a.changes) != 0 {
			t.Errorf("%v, zmiany %v", err, a.changes)
		}
	})
}
