// Package drain to warunek zakończenia Etapu 2: drain każdego węzła z repliką
// API bez ani jednego nieudanego żądania (dawniej ci/check-drain.sh).
//
// Pętla żądań biegnie w klastrze, na innym węźle niż drenowany, i woła
// Service po nazwie DNS — tak jak każdy klient w klastrze. Z hosta przez
// `kubectl port-forward` ten test byłby bezwartościowy: port-forward trzyma
// się jednego poda i zrywa się razem z nim, niezależnie od Service i PDB.
package drain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kube"
	"github.com/mateuszf757/Docfind/tools/internal/probeline"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
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

// Config — parametry bramki.
type Config struct {
	Namespace  string
	Deployment string
	// Container — kontener API w podzie (do raportu).
	Container string
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
	Core kubernetes.Interface
	// Kubectl uruchamia `kubectl drain` — eksmisja z poszanowaniem PDB
	// i ponawianiem przy 429 to ten sam kod co w kubectl, bez wciągania
	// k8s.io/kubectl do narzędzi (decyzja 33).
	Kubectl proc.Runner
	Target  kube.Target
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
	Node         string            `json:"node"`
	ProbeNode    string            `json:"probe_node"`
	ReplicasOn   []string          `json:"replicas_before"`
	ReplicasMove []string          `json:"replicas_after"`
	DrainSeconds float64           `json:"drain_seconds"`
	Summary      probeline.Summary `json:"requests"`
}

// Run przeprowadza bramkę i zwraca raport. Stan zewnętrzny — cordon,
// sondy, blokada — jest sprzątany także po SIGINT i SIGTERM (kontekst
// anulowany, sprzątanie na własnym kontekście).
func (g *Gate) Run(ctx context.Context) (rep Report, err error) {
	rep = Report{Run: g.Cfg.RunID, Started: time.Now().UTC(), Unmet: []string{}}
	defer func() {
		cleanupCtx, cancel := cli.CleanupContext(60 * time.Second)
		defer cancel()
		err = errors.Join(err, g.cleanup(cleanupCtx))
		rep.Finished = time.Now().UTC()
	}()
	fail := func(format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		rep.Unmet = append(rep.Unmet, msg)
		return cli.Unmet("%s", msg)
	}

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

	dep, err := g.Core.AppsV1().Deployments(g.Cfg.Namespace).Get(ctx, g.Cfg.Deployment, metav1.GetOptions{})
	if err != nil {
		return rep, fmt.Errorf("odczyt Deploymentu %s/%s: %w", g.Cfg.Namespace, g.Cfg.Deployment, err)
	}
	g.selector = labels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String()
	if err := g.waitRollout(ctx); err != nil {
		return rep, err
	}
	if err := g.checkPDB(ctx, dep.Spec.Template.Labels, fail); err != nil {
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
			if cli.ExitCode(err) == cli.ExitUnmet {
				rep.Unmet = append(rep.Unmet, err.Error())
			}
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
	if pods, err := g.Core.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + target}); err == nil {
		for _, p := range pods.Items {
			fmt.Fprintf(cli.Out, "     %-22s %s\n", p.Namespace, p.Name)
		}
	}

	probe, err := g.startProbe(ctx, probeNode)
	if err != nil {
		return result, err
	}

	started := time.Now()
	if err := g.cordon(ctx, target); err != nil {
		return result, err
	}
	cli.Step("kubectl drain %s", target)
	args := append([]string{"drain", target, "--ignore-daemonsets", "--delete-emptydir-data", fmt.Sprintf("--timeout=%s", g.Cfg.DrainTimeout)}, g.Target.KubectlArgs()...)
	if _, err := g.Kubectl.Run(ctx, proc.Cmd{Name: "kubectl", Args: args}); err != nil {
		return result, fmt.Errorf("kubectl drain %s: %w", target, err)
	}
	if err := g.waitRollout(ctx); err != nil {
		return result, err
	}
	result.DrainSeconds = time.Since(started).Seconds()
	cli.Step("drain zakończony, repliki odtworzone w %.1f s", result.DrainSeconds)

	// Chwila po drainie: błędy potrafią pojawić się dopiero wtedy, gdy nowy
	// pod dostaje pierwszy ruch.
	if err := sleep(ctx, g.Cfg.SettleAfter); err != nil {
		return result, err
	}
	raw, err := g.probeLog(ctx, probe)
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
	results, err := probeline.Parse(raw)
	if err != nil {
		return result, err
	}
	result.Summary = probeline.Summarize(results)
	if err := g.deleteProbe(ctx, probe); err != nil {
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
	return result, nil
}

// probeNode wybiera węzeł dla sondy: gotowy, przyjmujący pody, inny niż
// drenowany. Sonda to goły pod, bez kontrolera — `kubectl drain` odmówiłby
// usunięcia go bez --force, więc nie może stać na drenowanym węźle.
func (g *Gate) probeNode(ctx context.Context, target string) (string, error) {
	nodes, err := g.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, n := range nodes.Items {
		if n.Name == target || n.Spec.Unschedulable || !nodeReady(n) {
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

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// ProbePod zwraca specyfikację sondy. Przestrzeń nazw ma profil Pod Security
// restricted (decyzja 31), więc sonda spełnia go jak każdy inny pod: bez
// tokenu konta usługi, nie jako root, bez eskalacji i uprawnień, z seccomp.
// Zasoby według decyzji 16: requesty i limit pamięci, bez limitu CPU.
func ProbePod(name, namespace, runID, node, image, url string) *corev1.Pod {
	nonRoot := true
	user := int64(ProbeUser)
	noEscalation := false
	readOnly := true
	noToken := false
	grace := int64(1)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{probeNameLabel: probeNameValue, runLabel: runID},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &grace,
			AutomountServiceAccountToken:  &noToken,
			NodeSelector:                  map[string]string{"kubernetes.io/hostname": node},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &nonRoot,
				RunAsUser:      &user,
				RunAsGroup:     &user,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "probe",
				Image: image,
				// Obraz jest tylko w węzłach (k3d image import), nie w rejestrze.
				ImagePullPolicy: corev1.PullNever,
				Args:            []string{"-url", url},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					ReadOnlyRootFilesystem:   &readOnly,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
			}},
		},
	}
}

// startProbe tworzy sondę o nazwie unikalnej dla biegu i czeka, aż dostanie
// pierwszą odpowiedź 200 — bez tego punktu odniesienia zero błędów w trakcie
// drainu nic by nie znaczyło.
func (g *Gate) startProbe(ctx context.Context, node string) (string, error) {
	g.probeSeq++
	name := fmt.Sprintf("drain-probe-%s-%d", g.Cfg.RunID, g.probeSeq)
	pod := ProbePod(name, g.Cfg.Namespace, g.Cfg.RunID, node, g.Cfg.ProbeImage, g.Cfg.ServiceURL)
	if _, err := g.Core.CoreV1().Pods(g.Cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("sonda %s: %w", name, err)
	}
	g.probes = append(g.probes, name)

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		results, err := g.probeResults(ctx, name)
		if err == nil {
			for _, r := range results {
				if r.OK() {
					return name, nil
				}
			}
		}
		if err := sleep(ctx, time.Second); err != nil {
			return name, err
		}
	}
	return name, cli.Unmet("pętla żądań nie dostała odpowiedzi 200 w 90 s jeszcze przed drainem — test nie ma punktu odniesienia (kubectl -n %s logs %s)", g.Cfg.Namespace, name)
}

func (g *Gate) probeLog(ctx context.Context, pod string) ([]byte, error) {
	raw, err := g.Core.CoreV1().Pods(g.Cfg.Namespace).GetLogs(pod, &corev1.PodLogOptions{Container: "probe"}).DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("log sondy %s: %w", pod, err)
	}
	return raw, nil
}

func (g *Gate) probeResults(ctx context.Context, pod string) ([]probeline.Result, error) {
	raw, err := g.probeLog(ctx, pod)
	if err != nil {
		return nil, err
	}
	return probeline.Parse(raw)
}

func (g *Gate) deleteProbe(ctx context.Context, pod string) error {
	err := g.Core.CoreV1().Pods(g.Cfg.Namespace).Delete(ctx, pod, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("usunięcie sondy %s: %w", pod, err)
	}
	g.probes = slices.DeleteFunc(g.probes, func(p string) bool { return p == pod })
	return nil
}

func (g *Gate) cordon(ctx context.Context, node string) error {
	patch := map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{
			AnnotationCordonedBy:  CordonedByGate,
			AnnotationCordonedRun: g.Cfg.RunID,
			AnnotationCordonedAt:  time.Now().UTC().Format(time.RFC3339),
		}},
		"spec": map[string]any{"unschedulable": true},
	}
	if err := g.patchNode(ctx, node, patch); err != nil {
		return fmt.Errorf("cordon %s: %w", node, err)
	}
	g.cordoned = append(g.cordoned, node)
	return nil
}

func (g *Gate) uncordon(ctx context.Context, node string) error {
	patch := map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{
			AnnotationCordonedBy: nil, AnnotationCordonedRun: nil, AnnotationCordonedAt: nil,
		}},
		"spec": map[string]any{"unschedulable": nil},
	}
	if err := g.patchNode(ctx, node, patch); err != nil {
		return fmt.Errorf("uncordon %s: %w", node, err)
	}
	g.cordoned = slices.DeleteFunc(g.cordoned, func(n string) bool { return n == node })
	return nil
}

func (g *Gate) patchNode(ctx context.Context, node string, patch map[string]any) error {
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = g.Core.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, data, metav1.PatchOptions{})
	return err
}

// repairPrevious przywraca węzły odcięte przez poprzedni bieg, który nie
// posprzątał (SIGKILL, OOM, utrata maszyny), i usuwa jego sondy. Blokada
// gwarantuje, że żaden inny bieg nie trwa, więc to wszystko są resztki.
func (g *Gate) repairPrevious(ctx context.Context) ([]string, error) {
	var repaired []string
	nodes, err := g.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, n := range nodes.Items {
		if n.Annotations[AnnotationCordonedBy] != CordonedByGate {
			continue
		}
		cli.Warn("węzeł %s odcięty przez bieg %s (%s), który nie posprzątał — przywracam", n.Name, n.Annotations[AnnotationCordonedRun], n.Annotations[AnnotationCordonedAt])
		if err := g.uncordon(ctx, n.Name); err != nil {
			return repaired, err
		}
		repaired = append(repaired, "węzeł "+n.Name)
	}
	pods, err := g.Core.CoreV1().Pods(g.Cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: probeNameLabel + "=" + probeNameValue})
	if err != nil {
		return repaired, err
	}
	for _, p := range pods.Items {
		cli.Warn("sonda %s z biegu %s została — usuwam", p.Name, p.Labels[runLabel])
		if err := g.Core.CoreV1().Pods(g.Cfg.Namespace).Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return repaired, err
		}
		repaired = append(repaired, "sonda "+p.Name)
	}
	return repaired, nil
}

func (g *Gate) foreignCordons(ctx context.Context) ([]string, error) {
	nodes, err := g.Core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var foreign []string
	for _, n := range nodes.Items {
		if n.Spec.Unschedulable && n.Annotations[AnnotationCordonedBy] != CordonedByGate {
			foreign = append(foreign, n.Name)
		}
	}
	return foreign, nil
}

func (g *Gate) replicaNodes(ctx context.Context) ([]string, error) {
	pods, err := g.Core.CoreV1().Pods(g.Cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: g.selector})
	if err != nil {
		return nil, err
	}
	var nodes []string
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.DeletionTimestamp == nil && !slices.Contains(nodes, p.Spec.NodeName) {
			nodes = append(nodes, p.Spec.NodeName)
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
	pdbs, err := g.Core.PolicyV1().PodDisruptionBudgets(g.Cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, pdb := range pdbs.Items {
		sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil || !sel.Matches(labels.Set(podLabels)) {
			continue
		}
		if pdb.Status.DisruptionsAllowed < 1 {
			return fail("PodDisruptionBudget %s nie dopuszcza teraz zakłócenia (disruptionsAllowed=%d) — drain by stanął", pdb.Name, pdb.Status.DisruptionsAllowed)
		}
		cli.Step("PodDisruptionBudget %s: dopuszczalne zakłócenia %d", pdb.Name, pdb.Status.DisruptionsAllowed)
		return nil
	}
	return fail("brak PodDisruptionBudget dla podów %s — drain dwóch węzłów naraz zabrałby wszystkie repliki", g.Cfg.Deployment)
}

// waitRollout czeka, aż Deployment ma wszystkie repliki zaktualizowane
// i dostępne.
func (g *Gate) waitRollout(ctx context.Context) error {
	deadline := time.Now().Add(g.Cfg.RolloutWait)
	for {
		dep, err := g.Core.AppsV1().Deployments(g.Cfg.Namespace).Get(ctx, g.Cfg.Deployment, metav1.GetOptions{})
		if err != nil {
			return err
		}
		want := int32(1)
		if dep.Spec.Replicas != nil {
			want = *dep.Spec.Replicas
		}
		s := dep.Status
		if s.ObservedGeneration >= dep.Generation && s.UpdatedReplicas == want && s.AvailableReplicas == want && s.Replicas == want {
			return nil
		}
		if time.Now().After(deadline) {
			return cli.Unmet("Deployment %s nie odtworzył %d dostępnych replik w %v (dostępne: %d)", g.Cfg.Deployment, want, g.Cfg.RolloutWait, s.AvailableReplicas)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (g *Gate) acquireLease(ctx context.Context) error {
	leases := g.Core.CoordinationV1().Leases(g.Cfg.Namespace)
	now := metav1.NewMicroTime(time.Now())
	seconds := int32(g.Cfg.LeaseDuration.Seconds())
	holder := g.Cfg.RunID
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: LeaseName, Namespace: g.Cfg.Namespace},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &seconds, AcquireTime: &now, RenewTime: &now},
	}
	_, err := leases.Create(ctx, lease, metav1.CreateOptions{})
	if err == nil {
		g.leased = true
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("blokada %s: %w", LeaseName, err)
	}
	existing, err := leases.Get(ctx, LeaseName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("blokada %s: %w", LeaseName, err)
	}
	if existing.Spec.RenewTime != nil && existing.Spec.LeaseDurationSeconds != nil {
		expires := existing.Spec.RenewTime.Add(time.Duration(*existing.Spec.LeaseDurationSeconds) * time.Second)
		if time.Now().Before(expires) {
			other := "?"
			if existing.Spec.HolderIdentity != nil {
				other = *existing.Spec.HolderIdentity
			}
			return cli.Unmet("trwa inny bieg bramki drainu (%s, blokada %s/%s do %s)", other, g.Cfg.Namespace, LeaseName, expires.UTC().Format(time.RFC3339))
		}
	}
	// Wygasła blokada po biegu, który nie posprzątał — przejęcie
	// z resourceVersion, więc dwa biegi nie przejmą jej naraz.
	existing.Spec = lease.Spec
	if _, err := leases.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return cli.Unmet("inny bieg przejął blokadę %s w tej samej chwili", LeaseName)
		}
		return fmt.Errorf("blokada %s: %w", LeaseName, err)
	}
	cli.Warn("blokada %s po poprzednim biegu wygasła — przejęta", LeaseName)
	g.leased = true
	return nil
}

// heartbeat odnawia blokadę, dopóki bieg trwa. Gorutyna czyta tylko
// klienta i konfigurację, które się nie zmieniają, więc nie dzieli stanu
// z resztą bramki.
func (g *Gate) heartbeat(ctx context.Context) {
	ticker := time.NewTicker(g.Cfg.LeaseRenew)
	defer ticker.Stop()
	leases := g.Core.CoordinationV1().Leases(g.Cfg.Namespace)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		lease, err := leases.Get(ctx, LeaseName, metav1.GetOptions{})
		if err != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != g.Cfg.RunID {
			continue
		}
		now := metav1.NewMicroTime(time.Now())
		lease.Spec.RenewTime = &now
		// Konflikt wersji przy odnowieniu nie jest groźny — następny takt
		// odnowi z aktualnej wersji.
		_, _ = leases.Update(ctx, lease, metav1.UpdateOptions{})
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
		holder := g.Cfg.RunID
		lease, err := g.Core.CoordinationV1().Leases(g.Cfg.Namespace).Get(ctx, LeaseName, metav1.GetOptions{})
		if err == nil && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == holder {
			err = g.Core.CoordinationV1().Leases(g.Cfg.Namespace).Delete(ctx, LeaseName, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{ResourceVersion: &lease.ResourceVersion},
			})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("zwolnienie blokady: %w", err))
		}
		g.leased = false
	}
	return errors.Join(errs...)
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
