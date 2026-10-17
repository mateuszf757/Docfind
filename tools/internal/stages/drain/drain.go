// Package drain to warunek zakończenia Etapu 2: drain każdego węzła z repliką
// API bez ani jednego nieudanego żądania (dawniej ci/check-drain.sh).
//
// Pętla żądań (sonda, cmd/drain-probe) biegnie w klastrze, na innym węźle
// niż drenowany, i woła Service po nazwie DNS — tak jak każdy klient
// w klastrze. Z hosta przez `kubectl port-forward` ten test byłby
// bezwartościowy: port-forward trzyma się jednego poda i zrywa się razem
// z nim, niezależnie od Service i PDB.
package drain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/probe"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
)

// Adnotacje na węźle, który bramka odcięła (cordon). Bez nich kolejny bieg
// nie odróżni węzła zostawionego przez przerwaną bramkę (SIGKILL, OOM,
// utracona maszyna — przypadki, w których żadne sprzątanie nie biegnie) od
// węzła, który ktoś odciął świadomie.
const (
	AnnotationCordonedBy  = "docfind.io/cordoned-by"
	AnnotationCordonedRun = "docfind.io/cordoned-run"
	AnnotationCordonedAt  = "docfind.io/cordoned-at"
	CordonedByGate        = "drain-gate"

	// LeaseName — blokada przed dwoma biegami bramki na jednym klastrze:
	// drenowałyby sobie nawzajem węzły i kasowały sondy.
	LeaseName = "docfind-drain-gate"

	probeNameLabel = "app.kubernetes.io/name"
	probeNameValue = "drain-probe"
	runLabel       = "docfind.io/run"
)

var cordonAnnotations = []string{AnnotationCordonedBy, AnnotationCordonedRun, AnnotationCordonedAt}

// Cluster — klaster środowiska (adapters/kube).
type Cluster interface {
	AcquireLease(ctx context.Context, namespace, name, holder string, ttl time.Duration) (cluster.LeaseAttempt, error)
	RenewLease(ctx context.Context, namespace, name, holder string) error
	ReleaseLease(ctx context.Context, namespace, name, holder string) error
	Nodes(ctx context.Context) ([]cluster.Node, error)
	Cordon(ctx context.Context, node string, annotations map[string]string) error
	Uncordon(ctx context.Context, node string, annotations []string) error
	Pods(ctx context.Context, namespace, selector string) ([]cluster.Pod, error)
	PodsOnNode(ctx context.Context, node string) ([]cluster.Pod, error)
	CreatePod(ctx context.Context, pod *corev1.Pod) error
	PodLog(ctx context.Context, namespace, pod, container string) ([]byte, error)
	DeletePod(ctx context.Context, namespace, pod string) error
	Rollout(ctx context.Context, namespace, name string) (cluster.Rollout, error)
	PDBFor(ctx context.Context, namespace string, podLabels map[string]string) (cluster.PDB, bool, error)
	// Drain — eksmisja z poszanowaniem PDB i ponawianiem przy 429, jak
	// `kubectl drain` (decyzja 33).
	Drain(ctx context.Context, node string, timeout time.Duration) error
	// WatchTerminations — zakończenia kontenerów usuwanych podów, od
	// wywołania do wywołania zwróconej funkcji (decyzja 58).
	WatchTerminations(ctx context.Context, namespace, selector string) (func() []cluster.Termination, error)
}

// Config — parametry bramki.
type Config struct {
	Namespace  string
	Deployment string
	// ServiceURL — adres żądań sondy.
	ServiceURL string
	ProbeImage string
	// Node — tylko ten węzeł (DRAIN_NODE); pusty — każdy węzeł z repliką.
	Node string
	// MinRequests — poniżej tej liczby pętla nie biegła naprawdę i zero
	// błędów nic nie znaczy.
	MinRequests  int
	DrainTimeout time.Duration
	RolloutWait  time.Duration
	SettleAfter  time.Duration
	// LeaseDuration i LeaseRenew — blokada żyje krótko i jest odnawiana
	// w tle. Bieg zabity SIGKILL-em (bez sprzątania) przestaje ją odnawiać,
	// więc po LeaseDuration kolejny bieg ją przejmuje i naprawia, co
	// zostało; przy długim czasie życia bez odnawiania czekałby pół godziny.
	LeaseDuration time.Duration
	LeaseRenew    time.Duration
	RunID         string
	// ProbeLogDir — katalog na pełny log sondy z każdego drainu (linia JSON
	// na żądanie, z czasami faz); pusty — log tylko w podsumowaniu raportu.
	// Dowód po porażce w CI, gdzie sondy już nie ma.
	ProbeLogDir string
}

// Gate to jeden bieg bramki.
type Gate struct {
	Cluster Cluster
	Cfg     Config

	cordoned []string
	probes   []string
	leased   bool
	selector string
	// probeSeq numeruje sondy biegu. Licznik, a nie długość listy żywych
	// sond: usunięta sonda jeszcze się wygasza, a nowa o tej samej nazwie
	// dostałaby od API servera „already exists".
	probeSeq int
}

// Report — wynik bramki w JSON.
type Report struct {
	Run      string        `json:"run"`
	Repaired []string      `json:"repaired,omitempty"`
	Drains   []DrainResult `json:"drains"`
	Unmet    []string      `json:"unmet"`
	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished"`
}

// DrainResult — jeden drain.
type DrainResult struct {
	Node         string        `json:"node"`
	ProbeNode    string        `json:"probe_node"`
	ReplicasOn   []string      `json:"replicas_before"`
	ReplicasMove []string      `json:"replicas_after"`
	DrainSeconds float64       `json:"drain_seconds"`
	Summary      probe.Summary `json:"requests"`
	// Terminations — zakończenia eksmitowanych podów aplikacji: czas od
	// prośby o usunięcie do końca kontenera i kod wyjścia (§5.2, X).
	Terminations []cluster.Termination `json:"terminations"`
}

// Run przeprowadza bramkę i zwraca raport. Stan zewnętrzny — cordon,
// sondy, blokada — jest sprzątany także po SIGINT i SIGTERM (kontekst
// anulowany, sprzątanie na własnym kontekście).
func (g *Gate) Run(ctx context.Context) (rep Report, err error) {
	rep = Report{Run: g.Cfg.RunID, Started: time.Now().UTC(), Unmet: []string{}}
	defer func() {
		// Niespełniony warunek trafia do raportu, skądkolwiek przyszedł —
		// także odmowa blokady i porażka drainu.
		if cli.ExitCode(err) == cli.ExitUnmet {
			rep.Unmet = append(rep.Unmet, err.Error())
		}
		cleanupCtx, cancel := cli.CleanupContext(60 * time.Second)
		defer cancel()
		err = errors.Join(err, g.cleanup(cleanupCtx))
		rep.Finished = time.Now().UTC()
	}()
	fail := func(format string, args ...any) error { return cli.Unmet(format, args...) }

	if err := g.acquireLease(ctx); err != nil {
		return rep, err
	}
	// Defer zarejestrowany po sprzątaniu biegnie przed nim: odnawianie
	// staje, zanim blokada zostanie zwolniona.
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go g.heartbeat(heartbeatCtx)

	repaired, err := g.repairPrevious(ctx)
	rep.Repaired = repaired
	if err != nil {
		return rep, err
	}
	if foreign, err := g.foreignCordons(ctx); err != nil {
		return rep, err
	} else if len(foreign) > 0 {
		return rep, fail("węzły odcięte spoza bramki: %s — drain sprawdzałby inną topologię niż produkcyjna; `kubectl uncordon` albo poczekaj na koniec prac", strings.Join(foreign, ", "))
	}

	rollout, err := g.Cluster.Rollout(ctx, g.Cfg.Namespace, g.Cfg.Deployment)
	if err != nil {
		return rep, err
	}
	g.selector = rollout.Selector
	if err := g.waitRollout(ctx); err != nil {
		return rep, err
	}
	if err := g.checkPDB(ctx, rollout.TemplateLabels, fail); err != nil {
		return rep, err
	}

	drained := map[string]bool{}
	for {
		nodes, err := g.replicaNodes(ctx)
		if err != nil {
			return rep, err
		}
		if len(nodes) < 2 {
			return rep, fail("repliki API na %d węźle (%s) — drain zabrałby je naraz; to błąd rozkładu podów (topologySpread ScheduleAnyway nie przywraca rozkładu po drainie)", len(nodes), strings.Join(nodes, ", "))
		}
		target := ""
		if g.Cfg.Node != "" {
			if len(drained) > 0 {
				break
			}
			if !slices.Contains(nodes, g.Cfg.Node) {
				return rep, fail("na węźle %s nie ma repliki API (repliki: %s) — drain niczego by nie sprawdził", g.Cfg.Node, strings.Join(nodes, ", "))
			}
			target = g.Cfg.Node
		} else {
			for _, n := range nodes {
				if !drained[n] {
					target = n
					break
				}
			}
			if target == "" {
				break
			}
		}
		result, err := g.drainOne(ctx, target, nodes)
		if result != nil {
			rep.Drains = append(rep.Drains, *result)
		}
		if err != nil {
			return rep, err
		}
		drained[target] = true
	}
	total := 0
	for _, d := range rep.Drains {
		total += d.Summary.Total
	}
	cli.Step("warunek zakończenia Etapu 2 spełniony: %d drainy, %d żądań, zero nieudanych", len(rep.Drains), total)
	return rep, nil
}

func (g *Gate) drainOne(ctx context.Context, target string, replicaNodes []string) (*DrainResult, error) {
	probeNode, err := g.probeNode(ctx, target)
	if err != nil {
		return nil, err
	}
	result := &DrainResult{Node: target, ProbeNode: probeNode, ReplicasOn: replicaNodes}
	cli.Step("drenowany węzeł: %s; pętla żądań na: %s", target, probeNode)
	// Co jeszcze stoi na drenowanym węźle — gdy coś zawiedzie, najpierw
	// trzeba wiedzieć, czy razem z API nie wyjechał na przykład jedyny pod
	// CoreDNS (decyzja 18).
	if pods, err := g.Cluster.PodsOnNode(ctx, target); err == nil {
		for _, p := range pods {
			fmt.Fprintf(cli.Out, "     %-22s %s\n", p.Namespace, p.Name)
		}
	}

	probePod, err := g.startProbe(ctx, probeNode)
	if err != nil {
		return result, err
	}

	// Zakończenia podów aplikacji obserwowane od przed cordonem do końca
	// rolloutu: eksmitowany pod ma skończyć w X bez SIGKILL (§5.2).
	stopWatch, err := g.Cluster.WatchTerminations(ctx, g.Cfg.Namespace, g.selector)
	if err != nil {
		return result, err
	}
	started := time.Now()
	if err := g.cordon(ctx, target); err != nil {
		stopWatch()
		return result, err
	}
	cli.Step("kubectl drain %s", target)
	if err := g.Cluster.Drain(ctx, target, g.Cfg.DrainTimeout); err != nil {
		stopWatch()
		return result, err
	}
	if err := g.waitRollout(ctx); err != nil {
		stopWatch()
		return result, err
	}
	result.Terminations = stopWatch()
	result.DrainSeconds = time.Since(started).Seconds()
	cli.Step("drain zakończony, repliki odtworzone w %.1f s", result.DrainSeconds)

	// Chwila po drainie: błędy potrafią pojawić się dopiero wtedy, gdy nowy
	// pod dostaje pierwszy ruch.
	if err := poll.Sleep(ctx, g.Cfg.SettleAfter); err != nil {
		return result, err
	}
	raw, err := g.probeLog(ctx, probePod)
	if err != nil {
		return result, err
	}
	if g.Cfg.ProbeLogDir != "" {
		if err := os.MkdirAll(g.Cfg.ProbeLogDir, 0o755); err != nil {
			return result, err
		}
		if err := os.WriteFile(filepath.Join(g.Cfg.ProbeLogDir, "drain-probe-"+target+".jsonl"), raw, 0o644); err != nil {
			return result, err
		}
	}
	results, err := probe.Parse(raw)
	if err != nil {
		return result, err
	}
	result.Summary = probe.Summarize(results)
	if err := g.deleteProbe(ctx, probePod); err != nil {
		return result, err
	}
	if err := g.uncordon(ctx, target); err != nil {
		return result, err
	}
	if after, err := g.replicaNodes(ctx); err == nil {
		result.ReplicasMove = after
	}

	s := result.Summary
	cli.Step("żądań: %d, nieudanych: %d (repliki teraz na: %s)", s.Total, s.Failed, strings.Join(result.ReplicasMove, ", "))
	if s.Total < g.Cfg.MinRequests {
		return result, cli.Unmet("drain %s: tylko %d żądań — pętla nie biegła wystarczająco długo (minimum %d)", target, s.Total, g.Cfg.MinRequests)
	}
	if s.Failed > 0 {
		fmt.Fprintln(cli.Err, "Nieudane żądania (czas, kod, błąd, DNS i połączenie w ms):")
		for _, f := range s.Failures {
			fmt.Fprintf(cli.Err, "  %s  %d  %s  dns=%.1f connect=%.1f połączono=%v\n", f.Time.Format(time.RFC3339Nano), f.Code, f.Error, f.DNSMs, f.ConnectMs, f.Connected)
		}
		fmt.Fprintf(cli.Err, "Z tego bez nawiązanego połączenia (DNS albo brak trasy): %d\n", s.NotConnected)
		return result, cli.Unmet("drain %s: %d z %d żądań nie powiodło się", target, s.Failed, s.Total)
	}
	return result, terminations(target, result.Terminations)
}

// terminations — eksmitowane pody aplikacji skończyły w X, bez SIGKILL
// (§5.2 kontraktu, decyzja 58). Drain węzła z repliką bez żadnego
// zaobserwowanego zakończenia to pomiar, który nie zadziałał.
func terminations(target string, terms []cluster.Termination) error {
	if len(terms) == 0 {
		return cli.Unmet("drain %s: nie zaobserwowano zakończenia żadnego poda aplikacji — pomiar X nie zadziałał", target)
	}
	var problems []string
	for _, t := range terms {
		if p := t.Problem(); p != "" {
			problems = append(problems, p)
			continue
		}
		cli.Step("pod %s zakończony w %v (X = %v), kod %d", t.Pod, t.Took(), t.Budget, t.ExitCode)
	}
	if len(problems) > 0 {
		return cli.Unmet("drain %s: %s", target, strings.Join(problems, "; "))
	}
	return nil
}

// probeNode wybiera węzeł dla sondy: gotowy, przyjmujący pody, inny niż
// drenowany. Sonda to goły pod, bez kontrolera — `kubectl drain` odmówiłby
// usunięcia go bez --force, więc nie może stać na drenowanym węźle.
func (g *Gate) probeNode(ctx context.Context, target string) (string, error) {
	nodes, err := g.Cluster.Nodes(ctx)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, n := range nodes {
		if n.Name == target || n.Unschedulable || !n.Ready {
			continue
		}
		candidates = append(candidates, n.Name)
	}
	slices.Sort(candidates)
	if len(candidates) == 0 {
		return "", cli.Unmet("brak gotowego węzła innego niż %s dla pętli żądań", target)
	}
	return candidates[0], nil
}

// startProbe tworzy sondę o nazwie unikalnej dla biegu i czeka, aż dostanie
// pierwszą odpowiedź 200 — bez tego punktu odniesienia zero błędów w trakcie
// drainu nic by nie znaczyło.
func (g *Gate) startProbe(ctx context.Context, node string) (string, error) {
	g.probeSeq++
	name := fmt.Sprintf("drain-probe-%s-%d", g.Cfg.RunID, g.probeSeq)
	pod := ProbePod(name, g.Cfg.Namespace, g.Cfg.RunID, node, g.Cfg.ProbeImage, g.Cfg.ServiceURL)
	if err := g.Cluster.CreatePod(ctx, pod); err != nil {
		return "", fmt.Errorf("sonda %s: %w", name, err)
	}
	g.probes = append(g.probes, name)

	err := poll.Until(ctx, 90*time.Second, time.Second, func(ctx context.Context) (bool, error) {
		raw, err := g.probeLog(ctx, name)
		if err != nil {
			return false, nil
		}
		results, err := probe.Parse(raw)
		if err != nil {
			return false, nil
		}
		return slices.ContainsFunc(results, probe.Result.OK), nil
	})
	if errors.Is(err, poll.ErrTimeout) {
		return name, cli.Unmet("pętla żądań nie dostała odpowiedzi 200 w 90 s jeszcze przed drainem — test nie ma punktu odniesienia (kubectl -n %s logs %s)", g.Cfg.Namespace, name)
	}
	return name, err
}

func (g *Gate) probeLog(ctx context.Context, pod string) ([]byte, error) {
	raw, err := g.Cluster.PodLog(ctx, g.Cfg.Namespace, pod, "probe")
	if err != nil {
		return nil, fmt.Errorf("log sondy %s: %w", pod, err)
	}
	return raw, nil
}

func (g *Gate) deleteProbe(ctx context.Context, pod string) error {
	if err := g.Cluster.DeletePod(ctx, g.Cfg.Namespace, pod); err != nil {
		return fmt.Errorf("usunięcie sondy %s: %w", pod, err)
	}
	g.probes = slices.DeleteFunc(g.probes, func(p string) bool { return p == pod })
	return nil
}

func (g *Gate) cordon(ctx context.Context, node string) error {
	if err := g.Cluster.Cordon(ctx, node, map[string]string{
		AnnotationCordonedBy:  CordonedByGate,
		AnnotationCordonedRun: g.Cfg.RunID,
		AnnotationCordonedAt:  time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	g.cordoned = append(g.cordoned, node)
	return nil
}

func (g *Gate) uncordon(ctx context.Context, node string) error {
	if err := g.Cluster.Uncordon(ctx, node, cordonAnnotations); err != nil {
		return err
	}
	g.cordoned = slices.DeleteFunc(g.cordoned, func(n string) bool { return n == node })
	return nil
}

// repairPrevious przywraca węzły odcięte przez poprzedni bieg, który nie
// posprzątał (SIGKILL, OOM, utrata maszyny), i usuwa jego sondy. Blokada
// gwarantuje, że żaden inny bieg nie trwa, więc to wszystko są resztki.
func (g *Gate) repairPrevious(ctx context.Context) ([]string, error) {
	var repaired []string
	nodes, err := g.Cluster.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.Annotations[AnnotationCordonedBy] != CordonedByGate {
			continue
		}
		cli.Warn("węzeł %s odcięty przez bieg %s (%s), który nie posprzątał — przywracam", n.Name, n.Annotations[AnnotationCordonedRun], n.Annotations[AnnotationCordonedAt])
		if err := g.uncordon(ctx, n.Name); err != nil {
			return repaired, err
		}
		repaired = append(repaired, "węzeł "+n.Name)
	}
	pods, err := g.Cluster.Pods(ctx, g.Cfg.Namespace, probeNameLabel+"="+probeNameValue)
	if err != nil {
		return repaired, err
	}
	for _, p := range pods {
		cli.Warn("sonda %s z biegu %s została — usuwam", p.Name, p.Labels[runLabel])
		if err := g.Cluster.DeletePod(ctx, g.Cfg.Namespace, p.Name); err != nil {
			return repaired, err
		}
		repaired = append(repaired, "sonda "+p.Name)
	}
	return repaired, nil
}

func (g *Gate) foreignCordons(ctx context.Context) ([]string, error) {
	nodes, err := g.Cluster.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var foreign []string
	for _, n := range nodes {
		if n.Unschedulable && n.Annotations[AnnotationCordonedBy] != CordonedByGate {
			foreign = append(foreign, n.Name)
		}
	}
	return foreign, nil
}

func (g *Gate) replicaNodes(ctx context.Context) ([]string, error) {
	pods, err := g.Cluster.Pods(ctx, g.Cfg.Namespace, g.selector)
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, p := range pods {
		if p.Running && !p.Terminating && !slices.Contains(nodes, p.Node) {
			nodes = append(nodes, p.Node)
		}
	}
	slices.Sort(nodes)
	return nodes, nil
}

// checkPDB — drain bez PodDisruptionBudget przeszedłby w tym teście, bo
// rolling update z maxUnavailable: 0 i tak dokłada repliki, ale u klienta
// równoległy drain dwóch węzłów zabrałby obie (U6). Bramka wymaga PDB, który
// obejmuje pody API i dopuszcza zakłócenie.
func (g *Gate) checkPDB(ctx context.Context, podLabels map[string]string, fail func(string, ...any) error) error {
	pdb, found, err := g.Cluster.PDBFor(ctx, g.Cfg.Namespace, podLabels)
	if err != nil {
		return err
	}
	if !found {
		return fail("brak PodDisruptionBudget dla podów %s — drain dwóch węzłów naraz zabrałby wszystkie repliki", g.Cfg.Deployment)
	}
	if pdb.DisruptionsAllowed < 1 {
		return fail("PodDisruptionBudget %s nie dopuszcza teraz zakłócenia (disruptionsAllowed=%d) — drain by stanął", pdb.Name, pdb.DisruptionsAllowed)
	}
	cli.Step("PodDisruptionBudget %s: dopuszczalne zakłócenia %d", pdb.Name, pdb.DisruptionsAllowed)
	return nil
}

// waitRollout czeka, aż Deployment ma wszystkie repliki zaktualizowane
// i dostępne.
func (g *Gate) waitRollout(ctx context.Context) error {
	var last cluster.Rollout
	err := poll.Until(ctx, g.Cfg.RolloutWait, time.Second, func(ctx context.Context) (bool, error) {
		r, err := g.Cluster.Rollout(ctx, g.Cfg.Namespace, g.Cfg.Deployment)
		last = r
		return err == nil && r.Complete(), err
	})
	if errors.Is(err, poll.ErrTimeout) {
		return cli.Unmet("Deployment %s nie odtworzył %d dostępnych replik w %v (dostępne: %d)", g.Cfg.Deployment, last.Wanted, g.Cfg.RolloutWait, last.Available)
	}
	return err
}

func (g *Gate) acquireLease(ctx context.Context) error {
	attempt, err := g.Cluster.AcquireLease(ctx, g.Cfg.Namespace, LeaseName, g.Cfg.RunID, g.Cfg.LeaseDuration)
	switch {
	case err != nil:
		return err
	case attempt.Conflict:
		return cli.Unmet("inny bieg przejął blokadę %s w tej samej chwili", LeaseName)
	case !attempt.Acquired:
		return cli.Unmet("trwa inny bieg bramki drainu (%s, blokada %s/%s do %s)", attempt.Holder, g.Cfg.Namespace, LeaseName, attempt.Expires.UTC().Format(time.RFC3339))
	case attempt.TookOver:
		cli.Warn("blokada %s po poprzednim biegu wygasła — przejęta", LeaseName)
	}
	g.leased = true
	return nil
}

// heartbeat odnawia blokadę, dopóki bieg trwa. Gorutyna czyta tylko port
// i konfigurację, które się nie zmieniają, więc nie dzieli stanu z resztą
// bramki.
func (g *Gate) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(g.Cfg.LeaseRenew)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		_ = g.Cluster.RenewLease(ctx, g.Cfg.Namespace, LeaseName, g.Cfg.RunID)
	}
}

func (g *Gate) cleanup(ctx context.Context) error {
	var errs []error
	for _, node := range slices.Clone(g.cordoned) {
		cli.Warn("przywracam węzeł %s (uncordon) po przerwanym biegu", node)
		errs = append(errs, g.uncordon(ctx, node))
	}
	for _, pod := range slices.Clone(g.probes) {
		errs = append(errs, g.deleteProbe(ctx, pod))
	}
	if g.leased {
		errs = append(errs, g.Cluster.ReleaseLease(ctx, g.Cfg.Namespace, LeaseName, g.Cfg.RunID))
		g.leased = false
	}
	return errors.Join(errs...)
}
