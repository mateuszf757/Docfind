// Package env czyta definicje środowisk z deploy/environments/<nazwa>.yaml.
//
// Kontekst kubeconfig, przestrzeń nazw, porty, host, wydawca i dopuszczalność
// operacji niszczących były zaszyte w każdym skrypcie osobno — porty nawet
// czytane grepem z deploy/k3d/cluster.yaml. Przy kilku środowiskach każda
// z tych stałych to miejsce, w którym bramka może trafić w nie ten klaster.
// Definicja jest jedna, a programy czytają ją zamiast stałych w kodzie.
package env

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"sigs.k8s.io/yaml"
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

// Cluster opisuje klaster środowiska i dostęp do niego.
type Cluster struct {
	// Provider: k3d (klaster tworzy dft) albo external (istniejący).
	Provider   string `json:"provider"`
	Name       string `json:"name"`
	Context    string `json:"context"`
	Kubeconfig string `json:"kubeconfig"`
	APIServer  string `json:"apiServer"`
	Ports      Ports  `json:"ports"`
}

// Ports — porty na 127.0.0.1 hosta.
type Ports struct {
	API   int `json:"api"`
	HTTP  int `json:"http"`
	HTTPS int `json:"https"`
}

// Artifact — skąd pochodzi obraz wdrażany na środowisko.
type Artifact struct {
	// Source: local (build z drzewa i import) albo registry (digest).
	Source string `json:"source"`
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
	ACMEEmail string `json:"acmeEmail,omitempty"`
	ACMEZone  string `json:"acmeZone,omitempty"`
	// OverridesFromEnvironment — host, wydawcę i ACME nadpisują zmienne
	// DOCFIND_* (mise.local.toml autora). Tylko w dev: w pozostałych
	// środowiskach definicja w gicie jest jedynym źródłem.
	OverridesFromEnvironment bool `json:"overridesFromEnvironment,omitempty"`
}

// Operations — co wolno bramkom na tym środowisku.
type Operations struct {
	// Destructive: allowed, locked (z blokadą przed równoległym biegiem)
	// albo forbidden.
	Destructive string `json:"destructive"`
}

// Dopuszczalność operacji niszczących.
const (
	DestructiveAllowed   = "allowed"
	DestructiveLocked    = "locked"
	DestructiveForbidden = "forbidden"
)

var (
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	issuers     = []string{"docfind-internal-ca", "letsencrypt-staging", "letsencrypt"}
)

// Selected zwraca nazwę wybranego środowiska: DOCFIND_ENV albo dev.
func Selected() string {
	if name := os.Getenv("DOCFIND_ENV"); name != "" {
		return name
	}
	return DefaultName
}

// Load czyta definicję środowiska name z repozytorium o korzeniu root,
// nakłada nadpisania ze zmiennych środowiskowych (jeśli definicja na nie
// pozwala) i sprawdza całość.
func Load(root, name string) (Environment, error) {
	if !namePattern.MatchString(name) {
		return Environment{}, fmt.Errorf("nazwa środowiska %q spoza [a-z][a-z0-9-]*", name)
	}
	path := filepath.Join(root, Dir, name+".yaml")
	raw, err := os.ReadFile(path)
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
	e.applyOverrides(os.Getenv)
	if err := e.Validate(); err != nil {
		return Environment{}, fmt.Errorf("%s: %w", path, err)
	}
	return e, nil
}

func (e *Environment) applyOverrides(getenv func(string) string) {
	if !e.Gateway.OverridesFromEnvironment {
		return
	}
	for key, target := range map[string]*string{
		"DOCFIND_HOSTNAME":   &e.Gateway.Hostname,
		"DOCFIND_TLS_ISSUER": &e.Gateway.Issuer,
		"DOCFIND_ACME_EMAIL": &e.Gateway.ACMEEmail,
		"DOCFIND_ACME_ZONE":  &e.Gateway.ACMEZone,
	} {
		if v := getenv(key); v != "" {
			*target = v
		}
	}
}

// Validate sprawdza definicję po nałożeniu nadpisań.
func (e Environment) Validate() error {
	var problems []string
	check := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	check(slices.Contains([]string{"wytwarzanie", "integracja", "akceptacja", "wydanie"}, e.Stage), "stage=%q", e.Stage)
	check(slices.Contains([]string{"k3d", "external"}, e.Cluster.Provider), "cluster.provider=%q", e.Cluster.Provider)
	check(namePattern.MatchString(e.Cluster.Name), "cluster.name=%q", e.Cluster.Name)
	check(e.Cluster.Context != "", "cluster.context pusty")
	check(e.Cluster.Kubeconfig != "" && !filepath.IsAbs(e.Cluster.Kubeconfig), "cluster.kubeconfig=%q — ścieżka względem korzenia repozytorium", e.Cluster.Kubeconfig)
	if u, err := url.Parse(e.Cluster.APIServer); err != nil || u.Scheme != "https" || u.Host == "" {
		problems = append(problems, fmt.Sprintf("cluster.apiServer=%q — oczekiwano https://host:port", e.Cluster.APIServer))
	}
	if e.Cluster.Provider == "k3d" {
		for name, port := range map[string]int{"api": e.Cluster.Ports.API, "http": e.Cluster.Ports.HTTP, "https": e.Cluster.Ports.HTTPS} {
			// Porty nieuprzywilejowane: Docker bez roota nie otworzy niższych,
			// a obniżenie ip_unprivileged_port_start otwiera je dla każdego
			// procesu w systemie (decyzja 17).
			check(port >= 1024 && port <= 65535, "cluster.ports.%s=%d — port nieuprzywilejowany 1024–65535", name, port)
		}
		check(e.Cluster.APIServer == fmt.Sprintf("https://127.0.0.1:%d", e.Cluster.Ports.API), "cluster.apiServer=%q — klaster k3d słucha na https://127.0.0.1:%d", e.Cluster.APIServer, e.Cluster.Ports.API)
	}
	check(slices.Contains([]string{"local", "registry"}, e.Artifact.Source), "artifact.source=%q", e.Artifact.Source)
	check(e.App.Namespace != "" && e.App.Release != "", "app.namespace i app.release wymagane")
	check(e.Gateway.Namespace != "" && e.Gateway.Name != "" && e.Gateway.Hostname != "", "gateway.namespace, name i hostname wymagane")
	check(slices.Contains(issuers, e.Gateway.Issuer), "gateway.issuer=%q — jeden z %v", e.Gateway.Issuer, issuers)
	check(slices.Contains([]string{DestructiveAllowed, DestructiveLocked, DestructiveForbidden}, e.Operations.Destructive), "operations.destructive=%q", e.Operations.Destructive)
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("niepoprawna definicja: %v", problems)
	}
	return nil
}

// KubeconfigPath zwraca bezwzględną ścieżkę kubeconfigu środowiska.
func (e Environment) KubeconfigPath(root string) string {
	return filepath.Join(root, e.Cluster.Kubeconfig)
}

// AllowsDestructive mówi, czy bramki mogą drenować węzły i wymuszać
// odnowienia na tym środowisku.
func (e Environment) AllowsDestructive() bool {
	return e.Operations.Destructive != DestructiveForbidden
}
