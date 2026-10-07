// Package env czyta definicje środowisk z deploy/environments/<nazwa>.yaml.
//
// Kontekst kubeconfig, przestrzeń nazw, porty, host, wydawca i dopuszczalność
// operacji niszczących były zaszyte w każdym skrypcie osobno — porty nawet
// czytane grepem z deploy/k3d/cluster.yaml. Przy kilku środowiskach każda
// z tych stałych to miejsce, w którym bramka może trafić w nie ten klaster.
// Definicja jest jedna, a programy czytają ją zamiast stałych w kodzie.
//
// Definicja jest też listą dozwolonych: dft rozmawia tylko z klastrem, którego
// kontekst i adres serwera są w definicji, i wykonuje na nim tylko operacje
// z operations.allowed. Czego lista nie wymienia, tego program odmawia — nowe
// środowisko nie dostaje drainu ani usuwania klastra przez przeoczenie.
package env

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

// Dir — katalog definicji względem korzenia repozytorium.
const Dir = "deploy/environments"

// DefaultName — środowisko, gdy DOCFIND_ENV jest puste.
const DefaultName = "dev"

// Environment to jedna definicja środowiska.
type Environment struct {
	Name       string     `json:"name"`
	Stage      string     `json:"stage"`
	Cluster    Cluster    `json:"cluster"`
	Artifact   Artifact   `json:"artifact"`
	App        App        `json:"app"`
	Gateway    Gateway    `json:"gateway"`
	Operations Operations `json:"operations"`
}

// Etapy cyklu wytwarzania.
const (
	StageDevelopment = "wytwarzanie"
	StageIntegration = "integracja"
	StageAcceptance  = "akceptacja"
	StageRelease     = "wydanie"
)

// Dostawcy klastra.
const (
	// ProviderK3d — klaster k3d na loopbacku, tworzony i usuwany przez dft.
	ProviderK3d = "k3d"
	// ProviderExternal — istniejący klaster; dft go nie tworzy ani nie usuwa.
	ProviderExternal = "external"
	// ProviderNone — środowisko bez klastra (jeszcze): definicja służy
	// walidacji chartów i promocji, a każda operacja na klastrze jest odmawiana.
	ProviderNone = "none"
)

// Cluster opisuje klaster środowiska i dostęp do niego.
type Cluster struct {
	Provider   string `json:"provider"`
	Name       string `json:"name,omitempty"`
	Context    string `json:"context,omitempty"`
	Kubeconfig string `json:"kubeconfig,omitempty"`
	APIServer  string `json:"apiServer,omitempty"`
	Ports      Ports  `json:"ports,omitempty"`
}

// Ports — porty na 127.0.0.1 hosta.
type Ports struct {
	API   int `json:"api,omitempty"`
	HTTP  int `json:"http,omitempty"`
	HTTPS int `json:"https,omitempty"`
}

// Źródła obrazu wdrażanego na środowisko.
const (
	// SourceLocal — build z drzewa roboczego i import do węzłów.
	SourceLocal = "local"
	// SourceArchive — archiwum `docker save` z zadania build, sprawdzane
	// digestem konfiguracji (PR: obrazu nie ma w rejestrze).
	SourceArchive = "archive"
	// SourceRegistry — obraz z rejestru po digeście indeksu.
	SourceRegistry = "registry"
)

// Artifact — skąd może pochodzić obraz wdrażany na środowisko.
type Artifact struct {
	Sources []string `json:"sources"`
	// ReleasesOnly — tylko obrazy wydań (wersja X.Y.Z bez części
	// przedwydaniowej): na produkcję trafia digest wydania, nie obraz z main.
	ReleasesOnly bool `json:"releasesOnly,omitempty"`
	// Attested — obraz z rejestru musi mieć w atestacji BuildKitu pochodzenie
	// i SBOM oraz atestację GitHuba (Sigstore) z workflowu ci tego
	// repozytorium (decyzja 37); wydanie — z tagu swojej wersji.
	Attested bool `json:"attested,omitempty"`
}

// App — release aplikacji.
type App struct {
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
}

// Gateway — wejście do klastra.
type Gateway struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Hostname  string `json:"hostname"`
	Issuer    string `json:"issuer"`
	// ACMEZone — strefa DNS, w której wydawca Let's Encrypt rozwiązuje
	// wyzwania DNS-01.
	ACMEZone string `json:"acmeZone,omitempty"`
	// OverridesFromEnvironment — host, wydawcę i strefę nadpisują zmienne
	// DOCFIND_* (mise.local.toml autora). Tylko w dev: w pozostałych
	// środowiskach definicja w gicie jest jedynym źródłem.
	OverridesFromEnvironment bool `json:"overridesFromEnvironment,omitempty"`
	// ACMEEmail — adres konta ACME. Nigdy z definicji (dane osobowe w
	// publicznym repozytorium): zawsze z DOCFIND_ACME_EMAIL.
	ACMEEmail string `json:"-"`
}

// Operation — operacja zmieniająca stan klastra.
type Operation string

// Operacje, które definicja może dopuścić. Odczyty (tożsamość na podach,
// stan Gateway) nie wymagają zgody — nie zmieniają klastra.
const (
	// OpClusterCreate — utworzenie klastra k3d, którego nie ma.
	OpClusterCreate Operation = "cluster-create"
	// OpClusterDelete — usunięcie klastra k3d.
	OpClusterDelete Operation = "cluster-delete"
	// OpDeploy — instalacja platformy i aplikacji (helm upgrade --install).
	OpDeploy Operation = "deploy"
	// OpDrain — drain węzłów pod ruchem.
	OpDrain Operation = "drain"
	// OpTLSRenew — wymuszone odnowienie certyfikatu.
	OpTLSRenew Operation = "tls-renew"
	// OpDNSToken — zapis tokenu Cloudflare do Secretu.
	OpDNSToken Operation = "dns-token"
)

var operations = []Operation{OpClusterCreate, OpClusterDelete, OpDeploy, OpDrain, OpTLSRenew, OpDNSToken}

// destructive — operacje, po których klaster przez chwilę działa gorzej albo
// znika. Na etapie wydania żadna nie biegnie automatycznie.
var destructive = []Operation{OpClusterCreate, OpClusterDelete, OpDrain, OpTLSRenew}

// Operations — co dft wolno zrobić na tym środowisku.
type Operations struct {
	Allowed []Operation `json:"allowed"`
}

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	issuers     = []string{"docfind-internal-ca", "letsencrypt-staging", "letsencrypt"}
	stages      = []string{StageDevelopment, StageIntegration, StageAcceptance, StageRelease}
)

// Selected zwraca nazwę wybranego środowiska: DOCFIND_ENV albo dev.
func Selected() string {
	if name := os.Getenv("DOCFIND_ENV"); name != "" {
		return name
	}
	return DefaultName
}

// Read czyta definicję środowiska name dokładnie tak, jak leży w gicie — bez
// nadpisań ze zmiennych środowiskowych — i ją sprawdza. Tak widzi ją CI.
func Read(root, name string) (Environment, error) {
	if !namePattern.MatchString(name) {
		return Environment{}, fmt.Errorf("nazwa środowiska %q spoza [a-z][a-z0-9-]*", name)
	}
	path := filepath.Join(root, Dir, name+".yaml")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Environment{}, cli.Usage("nieznane środowisko %q (DOCFIND_ENV) — zdefiniowane: %s", name, strings.Join(Names(root), ", "))
	}
	if err != nil {
		return Environment{}, fmt.Errorf("środowisko %s: %w", name, err)
	}
	var e Environment
	// Ściśle: nieznane pole w definicji to literówka, która inaczej po cichu
	// zostawiłaby wartość domyślną.
	if err := yaml.UnmarshalStrict(raw, &e); err != nil {
		return Environment{}, fmt.Errorf("%s: %w", path, err)
	}
	if e.Name != name {
		return Environment{}, fmt.Errorf("%s: name=%q, a plik nazywa się %s", path, e.Name, name)
	}
	if err := e.Validate(); err != nil {
		return Environment{}, fmt.Errorf("%s: %w", path, err)
	}
	return e, nil
}

// Names zwraca nazwy zdefiniowanych środowisk.
func Names(root string) []string {
	files, _ := filepath.Glob(filepath.Join(root, Dir, "*.yaml"))
	var names []string
	for _, f := range files {
		names = append(names, strings.TrimSuffix(filepath.Base(f), ".yaml"))
	}
	return names
}

// Load czyta definicję jak Read, nakłada nadpisania ze zmiennych
// środowiskowych (jeśli definicja na nie pozwala) i sprawdza wynik jeszcze
// raz. Tak widzą ją programy, które działają na klastrze.
func Load(root, name string) (Environment, error) {
	e, err := Read(root, name)
	if err != nil {
		return Environment{}, err
	}
	e.applyOverrides(os.Getenv)
	if err := e.Validate(); err != nil {
		return Environment{}, fmt.Errorf("środowisko %s po nadpisaniach z DOCFIND_*: %w", name, err)
	}
	return e, nil
}

// All czyta wszystkie definicje z repozytorium (bez nadpisań) i sprawdza
// reguły między nimi: dwa środowiska nie mogą dzielić klastra, kontekstu,
// kubeconfigu ani portów — inaczej bramka jednego trafiłaby w drugie, a dwa
// klastry lokalne nie wstałyby obok siebie.
func All(root string) ([]Environment, error) {
	names := Names(root)
	if len(names) == 0 {
		return nil, fmt.Errorf("brak definicji środowisk w %s", Dir)
	}
	var all []Environment
	for _, name := range names {
		e, err := Read(root, name)
		if err != nil {
			return nil, err
		}
		all = append(all, e)
	}
	var problems []string
	seen := map[string]string{}
	claim := func(owner, kind, value string) {
		if value == "" || value == "0" {
			return
		}
		key := kind + "=" + value
		if other, ok := seen[key]; ok {
			problems = append(problems, fmt.Sprintf("%s i %s mają ten sam %s", other, owner, key))
			return
		}
		seen[key] = owner
	}
	for _, e := range all {
		claim(e.Name, "cluster.name", e.Cluster.Name)
		claim(e.Name, "cluster.context", e.Cluster.Context)
		claim(e.Name, "cluster.kubeconfig", e.Cluster.Kubeconfig)
		for _, port := range []int{e.Cluster.Ports.API, e.Cluster.Ports.HTTP, e.Cluster.Ports.HTTPS} {
			claim(e.Name, "port", fmt.Sprint(port))
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("definicje środowisk kolidują: %s", strings.Join(problems, "; "))
	}
	return all, nil
}

func (e *Environment) applyOverrides(getenv func(string) string) {
	e.Gateway.ACMEEmail = getenv("DOCFIND_ACME_EMAIL")
	if !e.Gateway.OverridesFromEnvironment {
		return
	}
	for key, target := range map[string]*string{
		"DOCFIND_HOSTNAME":   &e.Gateway.Hostname,
		"DOCFIND_TLS_ISSUER": &e.Gateway.Issuer,
		"DOCFIND_ACME_ZONE":  &e.Gateway.ACMEZone,
	} {
		if v := getenv(key); v != "" {
			*target = v
		}
	}
}

// Validate sprawdza definicję.
func (e Environment) Validate() error {
	var problems []string
	check := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	check(slices.Contains(stages, e.Stage), "stage=%q — jeden z %v", e.Stage, stages)
	c := e.Cluster
	switch c.Provider {
	case ProviderK3d, ProviderExternal:
		check(c.Context != "", "cluster.context pusty")
		check(c.Kubeconfig != "" && !filepath.IsAbs(c.Kubeconfig) && !strings.Contains(c.Kubeconfig, ".."), "cluster.kubeconfig=%q — ścieżka w repozytorium, względem jego korzenia", c.Kubeconfig)
		if u, err := url.Parse(c.APIServer); err != nil || u.Scheme != "https" || u.Host == "" {
			problems = append(problems, fmt.Sprintf("cluster.apiServer=%q — oczekiwano https://host:port", c.APIServer))
		}
	case ProviderNone:
		check(c == Cluster{Provider: ProviderNone}, "cluster.provider=none — bez nazwy, kontekstu, kubeconfigu, serwera i portów")
	default:
		problems = append(problems, fmt.Sprintf("cluster.provider=%q — k3d, external albo none", c.Provider))
	}
	if c.Provider == ProviderK3d {
		check(namePattern.MatchString(c.Name), "cluster.name=%q", c.Name)
		for name, port := range map[string]int{"api": c.Ports.API, "http": c.Ports.HTTP, "https": c.Ports.HTTPS} {
			// Porty nieuprzywilejowane: Docker bez roota nie otworzy niższych,
			// a obniżenie ip_unprivileged_port_start otwiera je dla każdego
			// procesu w systemie (decyzja 17).
			check(port >= 1024 && port <= 65535, "cluster.ports.%s=%d — port nieuprzywilejowany 1024–65535", name, port)
		}
		check(c.APIServer == fmt.Sprintf("https://127.0.0.1:%d", c.Ports.API), "cluster.apiServer=%q — klaster k3d słucha na https://127.0.0.1:%d", c.APIServer, c.Ports.API)
	}
	if c.Provider == ProviderExternal {
		check(c.Name == "" && c.Ports == Ports{}, "cluster.provider=external — nazwa i porty należą do klastrów k3d")
	}

	check(len(e.Artifact.Sources) > 0, "artifact.sources puste")
	for _, s := range e.Artifact.Sources {
		check(slices.Contains([]string{SourceLocal, SourceArchive, SourceRegistry}, s), "artifact.sources: %q — local, archive albo registry", s)
		// Obraz spoza rejestru trafia do węzłów importem k3d.
		check(s == SourceRegistry || c.Provider == ProviderK3d, "artifact.sources: %s wymaga klastra k3d (import do węzłów)", s)
	}
	check(!e.Artifact.ReleasesOnly || slices.Equal(e.Artifact.Sources, []string{SourceRegistry}), "artifact.releasesOnly — wydania przychodzą tylko z rejestru")
	check(!e.Artifact.Attested || slices.Contains(e.Artifact.Sources, SourceRegistry), "artifact.attested — atestacje mają tylko obrazy z rejestru")
	check(!e.Artifact.ReleasesOnly || e.Artifact.Attested, "artifact.releasesOnly bez artifact.attested — wydanie bez sprawdzonej atestacji")

	check(e.App.Namespace != "" && e.App.Release != "", "app.namespace i app.release wymagane")
	g := e.Gateway
	check(g.Namespace != "" && g.Name != "" && g.Hostname != "", "gateway.namespace, name i hostname wymagane")
	check(slices.Contains(issuers, g.Issuer), "gateway.issuer=%q — jeden z %v", g.Issuer, issuers)
	check(!strings.HasPrefix(g.Issuer, "letsencrypt") || g.ACMEZone != "", "gateway.issuer=%s wymaga gateway.acmeZone", g.Issuer)
	check(g.ACMEZone == "" || g.Hostname == g.ACMEZone || strings.HasSuffix(g.Hostname, "."+g.ACMEZone), "gateway.hostname=%q spoza strefy %q", g.Hostname, g.ACMEZone)
	check(!g.OverridesFromEnvironment || e.Stage == StageDevelopment, "gateway.overridesFromEnvironment tylko na etapie wytwarzania")

	allowed := e.Operations.Allowed
	for i, op := range allowed {
		check(slices.Contains(operations, op), "operations.allowed: %q — jedna z %v", op, operations)
		check(!slices.Contains(allowed[:i], op), "operations.allowed: %s dwa razy", op)
	}
	if c.Provider != ProviderK3d {
		check(!slices.Contains(allowed, OpClusterCreate) && !slices.Contains(allowed, OpClusterDelete), "operations.allowed: tworzenie i usuwanie tylko dla klastrów k3d")
	}
	if c.Provider == ProviderNone {
		check(len(allowed) == 0, "operations.allowed: środowisko bez klastra nie ma operacji")
	}
	if e.Stage == StageRelease {
		for _, op := range destructive {
			check(!slices.Contains(allowed, op), "operations.allowed: %s na etapie wydania — produkcja nie jest polem do ćwiczeń", op)
		}
	}
	// Produkcyjny Let's Encrypt wystawia najwyżej 5 identycznych certyfikatów
	// na tydzień; wymuszone odnowienie w każdym biegu bramki wyczerpałoby
	// limit. Autor może to zrobić świadomie (DOCFIND_TLS_RENEW_PRODUCTION=1),
	// definicja nie może tego robić z założenia.
	check(g.Issuer != "letsencrypt" || !slices.Contains(allowed, OpTLSRenew) || g.OverridesFromEnvironment, "operations.allowed: tls-renew z wydawcą letsencrypt (limit 5 identycznych certyfikatów na tydzień)")

	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("niepoprawna definicja: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Allows mówi, czy definicja dopuszcza operację.
func (e Environment) Allows(op Operation) bool {
	return slices.Contains(e.Operations.Allowed, op)
}

// Require odmawia (warunek niespełniony, kod 1) operacji spoza listy
// dozwolonych — zanim program połączy się z klastrem.
func (e Environment) Require(op Operation) error {
	if e.Allows(op) {
		return nil
	}
	return cli.Unmet("środowisko %s nie dopuszcza operacji %s (operations.allowed: %v) — odmowa", e.Name, op, e.Operations.Allowed)
}

// RequireCluster odmawia, gdy środowisko nie ma klastra.
func (e Environment) RequireCluster() error {
	if e.Cluster.Provider == ProviderNone {
		return cli.Unmet("środowisko %s nie ma klastra (cluster.provider=none) — nie ma z czym rozmawiać", e.Name)
	}
	return nil
}

// AllowsSource mówi, czy obraz z danego źródła może trafić na środowisko.
func (e Environment) AllowsSource(source string) bool {
	return slices.Contains(e.Artifact.Sources, source)
}

// KubeconfigPath zwraca bezwzględną ścieżkę kubeconfigu środowiska.
func (e Environment) KubeconfigPath(root string) string {
	return filepath.Join(root, e.Cluster.Kubeconfig)
}

// HTTPSRedirectPort — port w przekierowaniu HTTP→HTTPS: na klastrze k3d ten,
// na którym słucha load balancer na hoście (decyzja 17), poza nim 443.
func (e Environment) HTTPSRedirectPort() int {
	if e.Cluster.Provider == ProviderK3d {
		return e.Cluster.Ports.HTTPS
	}
	return 443
}

// AppValues — wartości charta aplikacji wynikające z definicji, jako
// klucz=wartość dla `helm --set`. Jedno miejsce dla wdrożenia i walidacji
// chartów: render sprawdzany w CI to render, który trafia na klaster.
func (e Environment) AppValues() []string {
	return []string{
		"route.hostname=" + e.Gateway.Hostname,
		"route.gateway.name=" + e.Gateway.Name,
		"route.gateway.namespace=" + e.Gateway.Namespace,
	}
}

// PlatformValues — wartości charta platformy wynikające z definicji.
// acmeEmail podaje wywołujący: na klastrze z DOCFIND_ACME_EMAIL, w walidacji
// adres przykładowy.
func (e Environment) PlatformValues(acmeEmail string) []string {
	values := []string{
		"gateway.name=" + e.Gateway.Name,
		"gateway.hostname=" + e.Gateway.Hostname,
		"gateway.issuer=" + e.Gateway.Issuer,
		fmt.Sprintf("gateway.httpsRedirectPort=%d", e.HTTPSRedirectPort()),
	}
	if strings.HasPrefix(e.Gateway.Issuer, "letsencrypt") {
		values = append(values, "acme.email="+acmeEmail, "acme.dnsZone="+e.Gateway.ACMEZone)
	}
	return values
}
