// Package upgrade sprawdza przejścia między wersjami aplikacji pod ruchem
// przez bramę (§5.7 kontraktu, decyzja 79): każde w Z, bez nieudanych
// żądań, z tożsamością wersji docelowej na każdym podzie.
//
// Na klastrze stoi N — wdrożenie tego commita. Przejścia:
//
//  1. upgrade do N-1 — `helm upgrade` do N-1 z rejestru z wartościami
//     środowiska: cofnięcie przez przypięcie wersji, jak w GitOps;
//  2. upgrade N-1 → N — `helm rollback` do rewizji N. Rollback to nowa
//     rewizja z manifestem tamtej, więc na klastrze te same operacje co
//     upgrade do N; chart nie ma hooków, które by to rozróżniały;
//  3. rollback N → N-1 — `helm rollback` do rewizji N-1, jak u klienta;
//  4. powrót do N — klaster jak przed bramką.
//
// Po porażce klaster zostaje w stanie z nieudanego przejścia — dowody idą
// z niego — a raport podaje polecenie powrotu do N.
package upgrade

import (
	"context"
	"fmt"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/deploy"
	"github.com/mateuszf757/Docfind/tools/internal/stages/identity"
	"github.com/mateuszf757/Docfind/tools/internal/stages/ingress"
)

// Releases — rewizje i rollback release aplikacji (adapters/helm).
type Releases interface {
	Revision(ctx context.Context, t cluster.Target, namespace, release string) (int, error)
	Rollback(ctx context.Context, t cluster.Target, namespace, release string, revision int, timeout time.Duration) error
}

// Charts — N-1 z rejestru: wybór z polityką środowiska i instalacja
// (stages/deploy).
type Charts interface {
	AdmitPrevious(ctx context.Context, commit string) (deploy.Previous, error)
	InstallPrevious(ctx context.Context, p deploy.Previous) error
}

// Traffic — akcja pod ruchem HTTPS przez bramę (stages/ingress).
type Traffic interface {
	UnderTraffic(ctx context.Context, what, deployment string, wait time.Duration, action func(context.Context) error) (ingress.RolloutReport, error)
}

// Gate — bramka na klastrze środowiska.
type Gate struct {
	Releases Releases
	Charts   Charts
	Traffic  Traffic
	// Pods — tożsamość z podów (adapters/kube).
	Pods       identity.Cluster
	Target     cluster.Target
	Namespace  string
	Release    string
	Deployment string
	Container  string
	// Wait — limit jednego przejścia: helm --timeout i rollout.
	Wait time.Duration
}

// Version — wersja aplikacji: rewizja Helma i tożsamość z podów.
type Version struct {
	Revision int `json:"revision"`
	// Chart — wersja charta z rejestru; tylko N-1.
	Chart   string `json:"chart,omitempty"`
	Image   string `json:"image"`
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Step — jedno przejście: pomiar pod ruchem i pody po nim.
type Step struct {
	ingress.RolloutReport
	Command string         `json:"command"`
	Pods    []identity.Pod `json:"pods"`
}

// Report — wynik bramki w JSON.
type Report struct {
	Release  string  `json:"release"`
	N        Version `json:"n"`
	Previous Version `json:"previous"`
	Steps    []Step  `json:"steps"`
	// Restore — polecenie powrotu do N po nieudanym przejściu.
	Restore string   `json:"restore,omitempty"`
	Unmet   []string `json:"unmet"`
}

// Run wykonuje przejścia po kolei i kończy na pierwszym nieudanym.
func (g *Gate) Run(ctx context.Context) (rep Report, err error) {
	rep = Report{Release: g.Release, Steps: []Step{}, Unmet: []string{}}
	defer func() {
		if cli.ExitCode(err) == cli.ExitUnmet {
			rep.Unmet = append(rep.Unmet, err.Error())
		}
	}()
	cur, err := identity.Current(ctx, g.Pods, g.Namespace, g.Deployment, g.Container)
	if err != nil {
		return rep, err
	}
	revN, err := g.Releases.Revision(ctx, g.Target, g.Namespace, g.Release)
	if err != nil {
		return rep, err
	}
	rep.N = Version{Revision: revN, Image: cur.Image, Version: cur.Version, Commit: cur.Commit}
	cli.Step("N: rewizja %d, %s z commita %s", revN, cur.Version, artifact.ShortCommit(cur.Commit))
	prev, err := g.Charts.AdmitPrevious(ctx, cur.Commit)
	if err != nil {
		return rep, err
	}
	rep.Previous = Version{Chart: prev.Chart, Image: prev.Want.Image, Version: prev.Want.Version, Commit: prev.Want.Commit}

	// Od pierwszego przejścia na klastrze może stać coś innego niż N.
	defer func() {
		if err != nil {
			rep.Restore = fmt.Sprintf("helm rollback %s %d --namespace %s", g.Release, revN, g.Namespace)
			cli.Warn("klaster zostaje w stanie z nieudanego przejścia (dowody); powrót do N: %s", rep.Restore)
		}
	}()
	install := func(ctx context.Context) error { return g.Charts.InstallPrevious(ctx, prev) }
	if err := g.step(ctx, &rep, "upgrade do N-1", fmt.Sprintf("helm upgrade %s --version %s", g.Release, prev.Chart), prev.Want, install); err != nil {
		return rep, err
	}
	if rep.Previous.Revision, err = g.Releases.Revision(ctx, g.Target, g.Namespace, g.Release); err != nil {
		return rep, err
	}
	for _, t := range []struct {
		what     string
		revision int
		want     artifact.Expected
	}{
		{"upgrade N-1 → N", revN, cur},
		{"rollback N → N-1", rep.Previous.Revision, prev.Want},
		{"powrót do N", revN, cur},
	} {
		rollback := func(ctx context.Context) error {
			return g.Releases.Rollback(ctx, g.Target, g.Namespace, g.Release, t.revision, g.Wait)
		}
		if err := g.step(ctx, &rep, t.what, fmt.Sprintf("helm rollback %s %d", g.Release, t.revision), t.want, rollback); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// step — przejście pod ruchem, potem tożsamość want na każdym podzie.
// Każdy błąd przejścia zaczyna się od jego nazwy.
func (g *Gate) step(ctx context.Context, rep *Report, what, command string, want artifact.Expected, action func(context.Context) error) error {
	r, err := g.Traffic.UnderTraffic(ctx, what, g.Deployment, g.Wait, func(ctx context.Context) error {
		if err := action(ctx); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	})
	s := Step{RolloutReport: r, Command: command}
	if err == nil {
		if s.Pods, err = identity.Verify(ctx, g.Pods, g.Namespace, g.Deployment, g.Container, want); err != nil {
			err = fmt.Errorf("%s: %w", what, err)
			if cli.ExitCode(err) == cli.ExitUnmet {
				s.Unmet = append(s.Unmet, err.Error())
			}
		}
	}
	rep.Steps = append(rep.Steps, s)
	return err
}
