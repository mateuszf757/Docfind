// Package cluster to słownik stanu klastra, którym etapy rozmawiają
// z adapterami Kubernetesa, k3d i Helma: węzeł, pod, rollout,
// PodDisruptionBudget, blokada, Gateway, trasa, certyfikat, klaster
// lokalny, release Helma. Etapy decydują na tych typach; jak je odczytać
// i zmienić (client-go, kubectl, cmctl, k3d, helm), wie adapter.
package cluster

import (
	"fmt"
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
	Name            string
	Chart           string
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
