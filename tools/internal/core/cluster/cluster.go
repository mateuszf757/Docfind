// Package cluster to słownik stanu klastra, którym etapy rozmawiają
// z adapterami Kubernetesa, k3d i Helma: węzeł, pod, rollout,
// PodDisruptionBudget, blokada, Gateway, trasa, certyfikat, klaster
// lokalny, release Helma. Etapy decydują na tych typach; jak je odczytać
// i zmienić (client-go, kubectl, cmctl, k3d, helm), wie adapter.
package cluster

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Target — klaster, z którym program ma rozmawiać.
type Target struct {
	// Kubeconfig — ścieżka pliku; nigdy KUBECONFIG z powłoki ani
	// ~/.kube/config (decyzja 26).
	Kubeconfig string
	// Context — nazwa kontekstu z definicji środowiska.
	Context string
	// APIServer — adres serwera, który ten kontekst musi wskazywać.
	APIServer string
}

// Ports — porty klastra lokalnego na 127.0.0.1 hosta (decyzja 17).
type Ports struct {
	API   int `json:"api,omitempty"`
	HTTP  int `json:"http,omitempty"`
	HTTPS int `json:"https,omitempty"`
}

// Node — węzeł.
type Node struct {
	Name          string
	Ready         bool
	Unschedulable bool
	Annotations   map[string]string
}

// Pod — pod z tym, czego potrzebują etapy.
type Pod struct {
	Namespace string
	Name      string
	Node      string
	Labels    map[string]string
	// Images — obraz każdego kontenera, po nazwie kontenera.
	Images map[string]string
	// Running — faza Running; Terminating — ma deletionTimestamp (po
	// rolloucie jeszcze odpowiada starą wersją przez preStop).
	Running     bool
	Terminating bool
	// Ready — warunek PodReady: pod jest w endpointach Service.
	Ready bool
	// UID — tożsamość tej instancji poda; pod odtworzony pod tą samą nazwą
	// (StatefulSet) ma inny UID.
	UID string
	// Restarts — suma restartów kontenerów.
	Restarts int32
	// Restarted — kontenery, które się restartowały: tylko one mają log
	// poprzedniej instancji.
	Restarted []string
}

// StatefulSet — stan StatefulSetu: ile replik chce spec, ile jest gotowych
// i która rewizja szablonu jest bieżąca (przy OnDelete pody na starszej
// rewizji czekają na wymianę).
type StatefulSet struct {
	Wanted, Ready  int32
	UpdateRevision string
}

// Rollout — stan Deploymentu.
type Rollout struct {
	// Wanted — spec.replicas; Updated, Available, Total — ze statusu.
	Wanted, Updated, Available, Total int32
	// Observed — kontroler widział najnowszą generację.
	Observed bool
	// Selector — selektor podów w postaci etykieta=wartość,…
	Selector string
	// TemplateLabels — etykiety szablonu poda.
	TemplateLabels map[string]string
}

// Complete — wszystkie repliki zaktualizowane i dostępne.
func (r Rollout) Complete() bool {
	return r.Observed && r.Updated == r.Wanted && r.Available == r.Wanted && r.Total == r.Wanted
}

// PDB — PodDisruptionBudget obejmujący pody.
type PDB struct {
	Name               string
	DisruptionsAllowed int32
}

// LeaseAttempt — wynik próby wzięcia blokady (Lease).
type LeaseAttempt struct {
	Acquired bool
	// TookOver — blokada po poprzednim biegu wygasła i została przejęta.
	TookOver bool
	// Conflict — ktoś przejął wygasłą blokadę w tej samej chwili.
	Conflict bool
	// Holder i Expires — żywa blokada innego biegu.
	Holder  string
	Expires time.Time
}

// Gateway — wejście do klastra (Gateway API).
type Gateway struct {
	// HTTPSHostname — nazwa hosta listenera https.
	HTTPSHostname string
	// Issuer — ClusterIssuer z adnotacji cert-manager.io/cluster-issuer.
	Issuer     string
	Programmed bool
}

// Route — warunki HTTPRoute nadane przez Gateway.
type Route struct {
	Accepted, ResolvedRefs string
}

// Certificate — certyfikat cert-managera.
type Certificate struct {
	Revision int64
	Ready    bool
}

// ExternalSecret — stan synchronizacji ExternalSecret (External Secrets
// Operator).
type ExternalSecret struct {
	Ready bool
	// Reason i Message — z warunku Ready (SecretSynced, SecretSyncedError
	// i powód od dostawcy).
	Reason, Message string
	// RefreshTime — ostatnia udana synchronizacja.
	RefreshTime time.Time
}

// SecretStore — stan magazynu ESO (walidacja połączenia z dostawcą).
type SecretStore struct {
	Ready           bool
	Reason, Message string
}

// Application — stan aplikacji Argo CD (decyzja 46).
type Application struct {
	// Sync — Synced, OutOfSync albo Unknown: czy manifesty na klastrze
	// zgadzają się z chartem w wersji Revision.
	Sync string
	// Revision — wersja charta, którą Argo wybrał z zakresu.
	Revision string
	// Health — Healthy, Progressing, Degraded, Missing, Suspended, Unknown.
	Health, HealthMessage string
	// Operation — ostatnia operacja synchronizacji (pusta faza — nie było).
	Operation AppOperation
	// Conditions — warunki aplikacji: typ i komunikat (ComparisonError,
	// SyncError, InvalidSpecError, …).
	Conditions []AppCondition
}

// AppOperation — operacja synchronizacji Application.
type AppOperation struct {
	// Phase — Running, Succeeded, Failed, Error, Terminating.
	Phase    string
	Revision string
	Message  string
	// Requested — pole operation ustawione: operacja zlecona albo w toku,
	// Argo CD jeszcze jej nie zakończył (po zakończeniu je czyści).
	Requested bool
}

// AppCondition — warunek Application.
type AppCondition struct {
	Type, Message string
}

// LocalCluster — klaster k3d do utworzenia.
type LocalCluster struct {
	Name string
	// Config — konfiguracja k3d (JSON) z nazwą i portami środowiska, na
	// 127.0.0.1 (decyzja 17).
	Config []byte
	// Image — obraz k3s przypięty digestem.
	Image string
	// UserNamespace — kubelet w przestrzeni nazw użytkownika (Docker
	// rootless, decyzja 19).
	UserNamespace bool
	Timeout       time.Duration
	// LogDir — katalog na logi węzłów z tworzenia: gdy węzeł nie wstanie
	// w limicie, k3d wycofuje klaster razem z kontenerami, a więc z jedynym
	// dowodem przyczyny.
	LogDir string
}

// Release — instalacja charta Helma (`helm upgrade --install`).
type Release struct {
	Name  string
	Chart string
	// Version — wersja charta z repozytorium OCI (--version); pusta przy
	// charcie z katalogu albo archiwum.
	Version         string
	Namespace       string
	CreateNamespace bool
	ValuesFiles     []string
	// Set — wartości klucz=wartość (--set).
	Set []string
	// SetString — wartości zawsze tekstowe (--set-string): tag obrazu,
	// digest, wersja. --set zgaduje typ, więc tag z samych cyfr (commit
	// 123456789012) zostałby liczbą, a schemat charta odrzuciłby wdrożenie.
	SetString []string
	Timeout   time.Duration
	// HooksOnly — helm czeka tylko na hooki, nie na gotowość zasobów. Dla
	// komponentu, który staje się gotowy dopiero po kroku po instalacji:
	// pody OpenBao są poza Service, dopóki `dft` go nie zainicjalizuje.
	HooksOnly bool
}

// String — do komunikatów.
func (r Release) String() string { return fmt.Sprintf("%s/%s", r.Namespace, r.Name) }

// PSAVersion zwraca wersję profilu Pod Security dla wersji Kubernetesa
// X.Y.Z: vX.Y (decyzja 31). „latest" zmieniałby reguły razem z aktualizacją
// klastra, bez przeglądu.
func PSAVersion(kubernetesVersion string) (string, error) {
	parts := strings.Split(kubernetesVersion, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("wersja Kubernetesa %q spoza X.Y.Z", kubernetesVersion)
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return "", fmt.Errorf("wersja Kubernetesa %q: %w", kubernetesVersion, err)
	}
	return "v" + parts[0] + "." + parts[1], nil
}

// SystemImages — obrazy podów, które łamią przypięcie (decyzje 20 i 56):
// obraz bez digestu spoza listy obrazów wydania k3s (airgap) i obraz z tej
// listy pobierany z sieci (pulled — obrazy ze zdarzeń Pulling) zamiast
// zaimportowany z archiwum. Obrazy z przedrostkiem own — nasze, z lokalnego
// builda albo archiwum zadania build, wdrażane po tagu — sprawdza tożsamość
// na podach i przyjęcie obrazu, nie ta reguła. Nazwy porównywane w pełnej
// postaci (docker.io/library/…).
func SystemImages(pods []Pod, pulled, airgap []string, own string) []string {
	allowed := map[string]bool{}
	for _, ref := range airgap {
		allowed[FullImage(ref)] = true
	}
	var found []string
	seen := map[string]bool{}
	for _, pod := range pods {
		for _, container := range slices.Sorted(maps.Keys(pod.Images)) {
			ref := pod.Images[container]
			if ref == "" || strings.Contains(ref, "@sha256:") || allowed[FullImage(ref)] || seen[ref] || (own != "" && strings.HasPrefix(ref, own)) {
				continue
			}
			seen[ref] = true
			found = append(found, fmt.Sprintf("%s/%s (%s): obraz %s bez digestu i spoza archiwum airgap k3s", pod.Namespace, pod.Name, container, ref))
		}
	}
	for _, ref := range pulled {
		if allowed[FullImage(ref)] && !seen["pull "+ref] {
			seen["pull "+ref] = true
			found = append(found, fmt.Sprintf("obraz %s z archiwum airgap pobrany z sieci — import na węźle nie zadziałał", ref))
		}
	}
	return found
}

// FullImage — referencja obrazu w pełnej postaci: busybox:1 →
// docker.io/library/busybox:1, rancher/x:1 → docker.io/rancher/x:1.
func FullImage(ref string) string {
	first, rest, ok := strings.Cut(ref, "/")
	switch {
	case !ok:
		return "docker.io/library/" + ref
	case strings.ContainsAny(first, ".:") || first == "localhost":
		return ref
	default:
		return "docker.io/" + first + "/" + rest
	}
}

// TerminationBudgetAnnotation — adnotacja poda aplikacji z X w sekundach:
// preStop, dokańczanie żądań i 1 s zapasu, liczone przez chart (decyzja 58).
const TerminationBudgetAnnotation = "docfind.io/termination-budget-seconds"

// Termination — zakończenie kontenera usuniętego poda: prośba o usunięcie
// (deletionTimestamp minus okres łaski), koniec kontenera, kod wyjścia
// i budżet X z adnotacji poda (zero — bez adnotacji).
type Termination struct {
	Pod       string        `json:"pod"`
	Container string        `json:"container"`
	Requested time.Time     `json:"requested"`
	Finished  time.Time     `json:"finished"`
	ExitCode  int32         `json:"exit_code"`
	Reason    string        `json:"reason,omitempty"`
	Budget    time.Duration `json:"budget"`
}

// Took — od prośby o usunięcie do końca kontenera.
func (t Termination) Took() time.Duration { return t.Finished.Sub(t.Requested) }

// Problem — zakończenie, które łamie §5.2 kontraktu: SIGKILL (kod 137
// albo OOMKilled), czas ponad X, brak X na podzie. Pusty — w porządku.
func (t Termination) Problem() string {
	switch {
	case t.ExitCode == 137 || t.Reason == "OOMKilled":
		return fmt.Sprintf("pod %s (%s) zabity SIGKILL (kod %d, %s) — nie zmieścił się w okresie łaski", t.Pod, t.Container, t.ExitCode, t.Reason)
	case t.Budget <= 0:
		return fmt.Sprintf("pod %s bez adnotacji %s — chart nie podaje X", t.Pod, TerminationBudgetAnnotation)
	case t.Took() > t.Budget:
		return fmt.Sprintf("pod %s (%s) kończył się %v, a X = %v (preStop + dokańczanie + 1 s)", t.Pod, t.Container, t.Took(), t.Budget)
	}
	return ""
}
