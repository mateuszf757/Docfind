// Package identity sprawdza, że na klastrze działa dokładnie ten obraz,
// który miał trafić: obraz, wersja i commit na każdym podzie, a liczba
// sprawdzonych podów równa liczbie replik (warunek Etapu 9 w wykonywalnej
// postaci). Tym kończy się każde wdrożenie i to samo sprawdza osobna
// bramka.
package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Cluster — odczyty z klastra (adapters/kube).
type Cluster interface {
	Rollout(ctx context.Context, namespace, name string) (cluster.Rollout, error)
	Pods(ctx context.Context, namespace, selector string) ([]cluster.Pod, error)
	ProxyGet(ctx context.Context, namespace, pod, port, path string) ([]byte, error)
}

// Reported — co pod mówi o sobie na /version.
type Reported struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Pod — wynik sprawdzenia jednego poda.
type Pod struct {
	Pod      string   `json:"pod"`
	Node     string   `json:"node"`
	Image    string   `json:"image"`
	Reported Reported `json:"reported"`
	Problem  string   `json:"problem,omitempty"`
}

// Verify sprawdza pody Deploymentu.
//
// /version czytany przez proxy API servera do poda, a nie `kubectl exec`
// z Pythonem w kontenerze: nie zależy od zawartości obrazu (powłoki,
// interpretera) i trafia dokładnie we wskazany pod, nie w Service.
//
// Pody wygaszane po rolloutcie jeszcze odpowiadają starą wersją przez
// preStop, więc są pomijane.
func Verify(ctx context.Context, c Cluster, namespace, deployment, container string, want artifact.Expected) ([]Pod, error) {
	rollout, err := c.Rollout(ctx, namespace, deployment)
	if err != nil {
		return nil, err
	}
	pods, err := c.Pods(ctx, namespace, rollout.Selector)
	if err != nil {
		return nil, fmt.Errorf("pody %s: %w", rollout.Selector, err)
	}
	var results []Pod
	v := &cli.Verdict{}
	for _, p := range pods {
		if p.Terminating {
			continue
		}
		r := Pod{Pod: p.Name, Node: p.Node, Image: p.Images[container]}
		switch {
		case r.Image != want.Image:
			r.Problem = fmt.Sprintf("uruchamia %s, oczekiwano %s", r.Image, want.Image)
		default:
			raw, err := c.ProxyGet(ctx, namespace, p.Name, "8000", "/version")
			if err != nil {
				r.Problem = fmt.Sprintf("nie odpowiada na /version: %v", err)
				break
			}
			if err := json.Unmarshal(raw, &r.Reported); err != nil {
				r.Problem = fmt.Sprintf("nieczytelne /version: %v", err)
				break
			}
			// Wersja to wersja wdrożenia (DOCFIND_VERSION); pusta — wdrożenie
			// jej nie ustawia i nie ma czego porównać. Commit pochodzi z obrazu.
			if r.Reported.Commit != want.Commit || (want.Version != "" && r.Reported.Version != want.Version) {
				r.Problem = fmt.Sprintf("raportuje %s (%s), oczekiwano %s (%s)", r.Reported.Version, r.Reported.Commit, want.Version, want.Commit)
			}
		}
		if r.Problem != "" {
			v.Fail("%s: %s", r.Pod, r.Problem)
		} else {
			cli.Step("%s (%s): /version %s, commit %s", r.Pod, r.Node, r.Reported.Version, artifact.ShortCommit(r.Reported.Commit))
		}
		results = append(results, r)
	}
	if len(results) != int(rollout.Wanted) {
		v.Fail("sprawdzone pody: %d, a Deployment %s ma %d replik", len(results), deployment, rollout.Wanted)
	}
	if err := v.Err("tożsamość na podach"); err != nil {
		return results, cli.Unmet("na klastrze działa coś innego niż obraz, który miał trafić — %v", err)
	}
	return results, nil
}

// Current — tożsamość tego, co stoi teraz na podach Deploymentu: obraz
// z pierwszego niewygaszanego poda, wersja i commit z jego /version.
// Upgrade i rollback (decyzja 79) wracają do niej po przejściu przez inną
// wersję — Verify z wynikiem sprawdza wtedy każdy pod.
func Current(ctx context.Context, c Cluster, namespace, deployment, container string) (artifact.Expected, error) {
	rollout, err := c.Rollout(ctx, namespace, deployment)
	if err != nil {
		return artifact.Expected{}, err
	}
	pods, err := c.Pods(ctx, namespace, rollout.Selector)
	if err != nil {
		return artifact.Expected{}, fmt.Errorf("pody %s: %w", rollout.Selector, err)
	}
	for _, p := range pods {
		if p.Terminating {
			continue
		}
		raw, err := c.ProxyGet(ctx, namespace, p.Name, "8000", "/version")
		if err != nil {
			return artifact.Expected{}, fmt.Errorf("%s: /version: %w", p.Name, err)
		}
		var r Reported
		if err := json.Unmarshal(raw, &r); err != nil {
			return artifact.Expected{}, fmt.Errorf("%s: nieczytelne /version: %w", p.Name, err)
		}
		return artifact.Expected{Image: p.Images[container], Version: r.Version, Commit: r.Commit}, nil
	}
	return artifact.Expected{}, cli.Unmet("Deployment %s nie ma działającego poda", deployment)
}

// Registry — publikacja w rejestrze (adapters/registry).
type Registry interface {
	Inspect(ctx context.Context, ref string) (artifact.Publication, error)
}

// Commits — commit drzewa roboczego (adapters/git).
type Commits interface {
	Commit(ctx context.Context) (string, error)
}

// Expect zwraca, czego oczekiwać od podów z obrazem image i wersją
// wdrożenia version (DOCFIND_VERSION z Deploymentu). Obraz po digeście
// pochodzi z rejestru i niesie commit w etykiecie; to, co tam leży, jest
// punktem odniesienia. Obraz z tagiem lokalnym zbudowano z drzewa roboczego
// — punktem odniesienia jest git.
func Expect(ctx context.Context, image, version string, r Registry, c Commits) (artifact.Expected, error) {
	if strings.Contains(image, "@") {
		repository, digest, err := artifact.ParseRef(image)
		if err != nil {
			return artifact.Expected{}, err
		}
		published, err := r.Inspect(ctx, repository+"@"+digest)
		if err != nil {
			return artifact.Expected{}, err
		}
		cli.Step("punkt odniesienia: etykieta obrazu w rejestrze (%s)", digest)
		return artifact.Expected{Image: image, Version: version, Commit: published.Commit}, nil
	}
	commit, err := c.Commit(ctx)
	if err != nil {
		return artifact.Expected{}, err
	}
	return artifact.Expected{Image: image, Version: version, Commit: commit}, nil
}
