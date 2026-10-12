// Package rotation to warunek zakończenia Etapu 4: rotacja sekretu dociera
// do podów bez restartu (decyzja 44).
//
// Sekret to tokeny dostępu do /metrics. Rotacja idzie tak, jak robi się ją
// u klientów API — bez chwili, w której ważny token przestaje działać:
//
//  1. w OpenBao nowy token obok dotychczasowych — każdy pod ma go przyjąć;
//  2. w OpenBao tylko nowy — każdy pod ma odrzucić stary.
//
// Czas mierzony od zapisu w OpenBao do odpowiedzi poda, bez wymuszania
// synchronizacji: naturalna droga przez refreshInterval ExternalSecret,
// Secret i podmianę plików w wolumenie przez kubelet. Każdy pod sprawdzany
// osobno (port-forward), nie przez Service — Service odpowiadałby którymś
// podem. Na koniec te same pody, z tymi samymi UID i liczbą restartów: rotacja,
// która wymaga restartu, warunku nie spełnia.
package rotation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	store "github.com/mateuszf757/Docfind/tools/internal/core/secretstore"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

// Cluster — pody i Secret aplikacji (adapters/kube).
type Cluster interface {
	Pods(ctx context.Context, namespace, selector string) ([]cluster.Pod, error)
	SecretData(ctx context.Context, namespace, name string) (map[string][]byte, error)
	PortForward(ctx context.Context, namespace, pod string, port int) (string, func(), error)
}

// Vault — zapis sekretu w OpenBao rolą wąską (stages/secrets).
type Vault interface {
	PutSecret(ctx context.Context, account, role, path string, data map[string]sensitive.Value) (int, error)
}

// Prober — kod odpowiedzi poda na żądanie z tokenem.
type Prober interface {
	Status(ctx context.Context, addr, path string, token sensitive.Value) (int, error)
}

// Gate — bramka rotacji na API w Namespace.
type Gate struct {
	Cluster Cluster
	Vault   Vault
	HTTP    Prober
	// Namespace i Selector — pody API; Secret i Key — Secret od ESO i plik
	// z tokenami w nim; Port — port API w podzie.
	Namespace, Selector string
	Secret, Key         string
	Port                int
	// Budget — najdłuższy dopuszczalny czas fazy na jednym podzie.
	Budget time.Duration
	// Interval — co ile pytać pody.
	Interval time.Duration
	// NewToken — nowy token; Now — zegar (nil: time.Now).
	NewToken func() (sensitive.Value, error)
	Now      func() time.Time
}

// Report — wynik bramki, bez wartości tokenów.
type Report struct {
	Pods   []string `json:"pods"`
	Phases []Phase  `json:"phases"`
	// BudgetSeconds — próg na fazę i pod.
	BudgetSeconds float64 `json:"budget_seconds"`
}

// Phase — jedna faza rotacji.
type Phase struct {
	Name string `json:"name"`
	// Version — wersja sekretu w OpenBao zapisana na początku fazy.
	Version int `json:"version"`
	// Seconds — od zapisu w OpenBao do odpowiedzi poda, po podzie.
	Seconds    map[string]float64 `json:"seconds"`
	MaxSeconds float64            `json:"max_seconds"`
}

const metricsPath = "/metrics"

// Kody odpowiedzi poda — bez net/http w etapie (decyzja 39: wejście-wyjście
// należy do adaptera).
const (
	statusOK           = 200
	statusUnauthorized = 401
)

func (g Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

// Run przeprowadza rotację i sprawdza, że dotarła do każdego poda.
func (g Gate) Run(ctx context.Context) (Report, error) {
	rep := Report{BudgetSeconds: g.Budget.Seconds()}
	pods, err := g.readyPods(ctx)
	if err != nil {
		return rep, err
	}
	for _, p := range pods {
		rep.Pods = append(rep.Pods, p.Name)
	}
	current, err := g.currentTokens(ctx)
	if err != nil {
		return rep, err
	}
	fresh, err := g.NewToken()
	if err != nil {
		return rep, err
	}
	stale := current[0]

	addrs := map[string]string{}
	for _, p := range pods {
		addr, stop, err := g.Cluster.PortForward(ctx, g.Namespace, p.Name, g.Port)
		if err != nil {
			return rep, err
		}
		defer stop()
		addrs[p.Name] = addr
	}
	// Punkt wyjścia: dotychczasowy token działa, nowy jeszcze nie — inaczej
	// pomiar mierzyłby coś innego niż rotację.
	for _, p := range pods {
		if err := g.expect(ctx, p.Name, addrs[p.Name], stale, statusOK); err != nil {
			return rep, err
		}
		if err := g.expect(ctx, p.Name, addrs[p.Name], fresh, statusUnauthorized); err != nil {
			return rep, err
		}
	}

	phases := []struct {
		name   string
		tokens []sensitive.Value
		probe  sensitive.Value
		want   int
		keep   sensitive.Value
	}{
		{"nowy token obok dotychczasowych", append(slices.Clone(current), fresh), fresh, statusOK, stale},
		{"tylko nowy token", []sensitive.Value{fresh}, stale, statusUnauthorized, fresh},
	}
	for _, ph := range phases {
		phase, err := g.phase(ctx, ph.name, ph.tokens, ph.probe, ph.want, pods, addrs)
		rep.Phases = append(rep.Phases, phase)
		if err != nil {
			return rep, err
		}
		// Drugi token fazy dalej działa: w fazie 1 stary, w fazie 2 nowy.
		for _, p := range pods {
			if err := g.expect(ctx, p.Name, addrs[p.Name], ph.keep, statusOK); err != nil {
				return rep, err
			}
		}
	}

	if err := g.sameInstances(ctx, pods); err != nil {
		return rep, err
	}
	cli.Step("rotacja dotarła do %d podów bez restartu: faza 1 do %.0f s, faza 2 do %.0f s (próg %.0f s)", len(pods), rep.Phases[0].MaxSeconds, rep.Phases[1].MaxSeconds, rep.BudgetSeconds)
	return rep, nil
}

// phase zapisuje tokens w OpenBao i czeka, aż każdy pod odpowie want na
// żądanie z probe.
func (g Gate) phase(ctx context.Context, name string, tokens []sensitive.Value, probe sensitive.Value, want int, pods []cluster.Pod, addrs map[string]string) (Phase, error) {
	ph := Phase{Name: name, Seconds: map[string]float64{}}
	lines := make([]string, len(tokens))
	for i, t := range tokens {
		lines[i] = t.Reveal()
	}
	version, err := g.Vault.PutSecret(ctx, store.AppWriterAccount, store.AppWriterRole, store.AppMetricsPath,
		map[string]sensitive.Value{store.AppMetricsField: sensitive.New(strings.Join(lines, "\n"))})
	if err != nil {
		return ph, err
	}
	ph.Version = version
	written := g.now()
	cli.Step("faza „%s”: wersja %d w OpenBao, czekam na każdy pod (bez wymuszania synchronizacji)", name, version)

	err = poll.Until(ctx, g.Budget, g.Interval, func(ctx context.Context) (bool, error) {
		for _, p := range pods {
			if _, done := ph.Seconds[p.Name]; done {
				continue
			}
			code, err := g.HTTP.Status(ctx, addrs[p.Name], metricsPath, probe)
			if err != nil {
				continue
			}
			if code == want {
				ph.Seconds[p.Name] = g.now().Sub(written).Seconds()
				ph.MaxSeconds = max(ph.MaxSeconds, ph.Seconds[p.Name])
				cli.Step("  %s: po %.0f s", p.Name, ph.Seconds[p.Name])
			}
		}
		return len(ph.Seconds) == len(pods), nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		var late []string
		for _, p := range pods {
			if _, done := ph.Seconds[p.Name]; !done {
				late = append(late, p.Name)
			}
		}
		return ph, cli.Unmet("faza „%s”: rotacja nie dotarła w %v do %s — kubectl -n %s describe externalsecret %s", name, g.Budget, strings.Join(late, ", "), g.Namespace, g.Secret)
	}
	return ph, err
}

func (g Gate) expect(ctx context.Context, pod, addr string, token sensitive.Value, want int) error {
	code, err := g.HTTP.Status(ctx, addr, metricsPath, token)
	if err != nil {
		return fmt.Errorf("%s: %w", pod, err)
	}
	if code != want {
		return cli.Unmet("%s: %s z tokenem odpowiada %d, oczekiwano %d", pod, metricsPath, code, want)
	}
	return nil
}

// readyPods — gotowe pody API; bez nich nie ma czego mierzyć.
func (g Gate) readyPods(ctx context.Context) ([]cluster.Pod, error) {
	pods, err := g.Cluster.Pods(ctx, g.Namespace, g.Selector)
	if err != nil {
		return nil, err
	}
	var ready []cluster.Pod
	for _, p := range pods {
		if p.Ready && !p.Terminating {
			ready = append(ready, p)
		}
	}
	if len(ready) == 0 {
		return nil, cli.Unmet("brak gotowych podów API (%s) w %s", g.Selector, g.Namespace)
	}
	return ready, nil
}

// currentTokens — tokeny, które pody mają dziś (Secret od ESO), w pamięci.
func (g Gate) currentTokens(ctx context.Context) ([]sensitive.Value, error) {
	data, err := g.Cluster.SecretData(ctx, g.Namespace, g.Secret)
	if err != nil {
		return nil, fmt.Errorf("odczyt Secretu %s/%s: %w", g.Namespace, g.Secret, err)
	}
	var tokens []sensitive.Value
	for _, line := range strings.Split(string(data[g.Key]), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			tokens = append(tokens, sensitive.New(t))
		}
	}
	if len(tokens) == 0 {
		return nil, cli.Unmet("Secret %s/%s bez tokenów w %s", g.Namespace, g.Secret, g.Key)
	}
	return tokens, nil
}

// sameInstances — po rotacji te same pody: ten sam UID, bez restartów.
func (g Gate) sameInstances(ctx context.Context, before []cluster.Pod) error {
	after, err := g.Cluster.Pods(ctx, g.Namespace, g.Selector)
	if err != nil {
		return err
	}
	byName := map[string]cluster.Pod{}
	for _, p := range after {
		byName[p.Name] = p
	}
	v := &cli.Verdict{}
	for _, p := range before {
		q, ok := byName[p.Name]
		switch {
		case !ok || q.UID != p.UID:
			v.Fail("%s: pod odtworzony w trakcie rotacji", p.Name)
		case q.Restarts != p.Restarts:
			v.Fail("%s: kontener zrestartowany w trakcie rotacji (%d → %d)", p.Name, p.Restarts, q.Restarts)
		}
	}
	return v.Err("rotacja bez restartu")
}
