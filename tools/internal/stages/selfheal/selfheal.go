// Package selfheal to warunek Etapu 5: ręczna zmiana zasobu aplikacji na
// klastrze wraca do stanu z charta bez człowieka — Argo CD z selfHeal
// (decyzja 46).
//
// Sprawdzane bez przestoju, na dwóch rodzajach zmiany:
//   - zmiana specyfikacji: Deployment API dostaje o jedną replikę więcej —
//     Argo przywraca liczbę z charta (więcej podów nie zmniejsza
//     przepustowości, mniej — tak);
//   - usunięcie zasobu: PodDisruptionBudget API znika — Argo zakłada go od
//     nowa.
//
// Usunięcie Deploymentu sprawdziłoby ten sam mechanizm za cenę przestoju
// stagingu: pody znikają razem z nim, a nowe wstają dopiero po
// synchronizacji. Czas mierzony od zmiany do stanu z charta i Application
// znowu zsynchronizowanej.
package selfheal

import (
	"context"
	"errors"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
)

// Cluster — aplikacja Argo CD i jej zasoby (adapters/kube).
type Cluster interface {
	Application(ctx context.Context, namespace, name string) (cluster.Application, error)
	Rollout(ctx context.Context, namespace, name string) (cluster.Rollout, error)
	ScaleDeployment(ctx context.Context, namespace, name string, replicas int32) error
	PodDisruptionBudgetExists(ctx context.Context, namespace, name string) (bool, error)
	DeletePodDisruptionBudget(ctx context.Context, namespace, name string) error
}

// Gate — bramka samonaprawy aplikacji Application (w ArgoNamespace) na
// Deploymencie i PDB w Namespace.
type Gate struct {
	Cluster                    Cluster
	ArgoNamespace, Application string
	Namespace, Deployment, PDB string
	// Budget — najdłuższy dopuszczalny czas przywrócenia jednej zmiany.
	Budget   time.Duration
	Interval time.Duration
	// Now — zegar (nil: time.Now).
	Now func() time.Time
}

// Report — wynik bramki.
type Report struct {
	Application string  `json:"application"`
	Revision    string  `json:"revision"`
	Changes     []Check `json:"changes"`
	// BudgetSeconds — próg na jedną zmianę.
	BudgetSeconds float64 `json:"budget_seconds"`
}

// Check — jedna ręczna zmiana i czas do stanu z charta.
type Check struct {
	Change string `json:"change"`
	// RestoredSeconds — od zmiany do stanu z charta i Application
	// zsynchronizowanej; zero — nie wróciło w limicie.
	RestoredSeconds float64 `json:"restored_seconds"`
}

func (g Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Run wprowadza obie zmiany po kolei i czeka na przywrócenie każdej.
func (g Gate) Run(ctx context.Context) (Report, error) {
	rep := Report{Application: g.ArgoNamespace + "/" + g.Application, BudgetSeconds: g.Budget.Seconds()}
	// Punkt wyjścia: aplikacja zsynchronizowana i zdrowa. Inaczej Argo może
	// właśnie wdrażać nową wersję i „przywrócenie" niczego by nie mówiło.
	app, err := g.Cluster.Application(ctx, g.ArgoNamespace, g.Application)
	if err != nil {
		return rep, err
	}
	rep.Revision = app.Revision
	if app.Sync != "Synced" || app.Health != "Healthy" {
		return rep, cli.Unmet("Application %s: %s/%s na wersji %q — bramka zaczyna od stanu zsynchronizowanego i zdrowego", rep.Application, app.Sync, app.Health, app.Revision)
	}
	rollout, err := g.Cluster.Rollout(ctx, g.Namespace, g.Deployment)
	if err != nil {
		return rep, err
	}
	want := rollout.Wanted

	scaled := Check{Change: "Deployment " + g.Deployment + ": o jedną replikę więcej niż w charcie"}
	cli.Step("zmiana ręczna: %s (%d → %d)", scaled.Change, want, want+1)
	if err := g.Cluster.ScaleDeployment(ctx, g.Namespace, g.Deployment, want+1); err != nil {
		return rep, err
	}
	err = g.restored(ctx, &scaled, func(ctx context.Context) (bool, error) {
		r, err := g.Cluster.Rollout(ctx, g.Namespace, g.Deployment)
		return err == nil && r.Wanted == want, nil
	})
	rep.Changes = append(rep.Changes, scaled)
	if err != nil {
		return rep, err
	}

	deleted := Check{Change: "PodDisruptionBudget " + g.PDB + ": usunięty"}
	cli.Step("zmiana ręczna: %s", deleted.Change)
	if err := g.Cluster.DeletePodDisruptionBudget(ctx, g.Namespace, g.PDB); err != nil {
		return rep, err
	}
	err = g.restored(ctx, &deleted, func(ctx context.Context) (bool, error) {
		ok, err := g.Cluster.PodDisruptionBudgetExists(ctx, g.Namespace, g.PDB)
		return err == nil && ok, nil
	})
	rep.Changes = append(rep.Changes, deleted)
	return rep, err
}

// restored czeka, aż zasób wróci do stanu z charta, a Application będzie
// znowu zsynchronizowana i zdrowa.
func (g Gate) restored(ctx context.Context, c *Check, back func(context.Context) (bool, error)) error {
	start := g.now()
	err := poll.Until(ctx, g.Budget, g.Interval, func(ctx context.Context) (bool, error) {
		if ok, err := back(ctx); err != nil || !ok {
			return false, err
		}
		app, err := g.Cluster.Application(ctx, g.ArgoNamespace, g.Application)
		return err == nil && app.Sync == "Synced" && app.Health == "Healthy", nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		return cli.Unmet("%s — Argo CD nie przywrócił stanu z charta w %v", c.Change, g.Budget)
	}
	if err != nil {
		return err
	}
	c.RestoredSeconds = g.now().Sub(start).Seconds()
	cli.Step("przywrócone po %.1f s", c.RestoredSeconds)
	return nil
}
