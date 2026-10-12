// Package pins czyta przypięcia z ci/pins.env i wersje zapisane w innych
// plikach repozytorium (Dockerfile, .python-version, go.mod), których żaden
// automat nie synchronizuje między sobą.
package pins

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// File — ścieżka pliku przypięć względem korzenia repozytorium.
const File = "ci/pins.env"

// Pins to zawartość ci/pins.env.
type Pins map[string]string

var (
	keyPattern = regexp.MustCompile(`^DF_[A-Z0-9_]+$`)
	// Wartość, którą Bash po `source` i Go czytają tak samo: bez cudzysłowów,
	// spacji, rozwijania zmiennych i podstawiania poleceń.
	valuePattern = regexp.MustCompile(`^[A-Za-z0-9._:/@+=-]+$`)

	semver       = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	minorVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	sha256Hex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	gitCommit    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// Obraz przypięty digestem indeksu: nazwa[:tag]@sha256:<64 hex>.
	pinnedImage = regexp.MustCompile(`^[a-z0-9./_-]+(:[A-Za-z0-9._-]+)?@sha256:[0-9a-f]{64}$`)
)

// rule opisuje jedno przypięcie: czego wymaga jego wartość.
type rule struct {
	pattern *regexp.Regexp
	what    string
}

// Klucze przypięć. Wywołujący odwołują się do stałych, a nie do napisów:
// literówka w nazwie przypięcia to błąd kompilacji, a nie panika w Get.
const (
	KubernetesVersion        = "DF_KUBERNETES_VERSION"
	K3sImage                 = "DF_K3S_IMAGE"
	CoreDNSChartVersion      = "DF_COREDNS_CHART_VERSION"
	CoreDNSChartURL          = "DF_COREDNS_CHART_URL"
	CoreDNSChartSHA256       = "DF_COREDNS_CHART_SHA256"
	CertManagerChartVersion  = "DF_CERT_MANAGER_CHART_VERSION"
	CertManagerChartURL      = "DF_CERT_MANAGER_CHART_URL"
	CertManagerChartSHA256   = "DF_CERT_MANAGER_CHART_SHA256"
	EnvoyGatewayChartRef     = "DF_ENVOY_GATEWAY_CHART_REF"
	EnvoyGatewayChartVersion = "DF_ENVOY_GATEWAY_CHART_VERSION"
	EnvoyGatewayChartSHA256  = "DF_ENVOY_GATEWAY_CHART_SHA256"
	OpenBaoChartVersion      = "DF_OPENBAO_CHART_VERSION"
	OpenBaoChartURL          = "DF_OPENBAO_CHART_URL"
	OpenBaoChartSHA256       = "DF_OPENBAO_CHART_SHA256"
	ExternalSecretsVersion   = "DF_EXTERNAL_SECRETS_CHART_VERSION"
	ExternalSecretsURL       = "DF_EXTERNAL_SECRETS_CHART_URL"
	ExternalSecretsSHA256    = "DF_EXTERNAL_SECRETS_CHART_SHA256"
	BuildKitImage            = "DF_BUILDKIT_IMAGE"
	SBOMGeneratorImage       = "DF_SBOM_GENERATOR_IMAGE"
	RegistryImage            = "DF_REGISTRY_IMAGE"
	KubeconformSchemaCommit  = "DF_KUBECONFORM_SCHEMA_COMMIT"
	BaseAlpine               = "DF_BASE_ALPINE"
)

// rules — znane przypięcia. Nieznany klucz to literówka, a brak znanego —
// przypięcie, które zniknęło; oba są błędem.
var rules = map[string]rule{
	KubernetesVersion:        {semver, "wersja X.Y.Z"},
	K3sImage:                 {pinnedImage, "obraz z digestem"},
	CoreDNSChartVersion:      {semver, "wersja X.Y.Z"},
	CoreDNSChartURL:          {regexp.MustCompile(`^https://`), "adres https"},
	CoreDNSChartSHA256:       {sha256Hex, "suma SHA-256"},
	CertManagerChartVersion:  {regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`), "wersja vX.Y.Z"},
	CertManagerChartURL:      {regexp.MustCompile(`^https://`), "adres https"},
	CertManagerChartSHA256:   {sha256Hex, "suma SHA-256"},
	EnvoyGatewayChartRef:     {regexp.MustCompile(`^oci://`), "referencja oci://"},
	EnvoyGatewayChartVersion: {regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`), "wersja vX.Y.Z"},
	EnvoyGatewayChartSHA256:  {sha256Hex, "suma SHA-256"},
	OpenBaoChartVersion:      {semver, "wersja X.Y.Z"},
	OpenBaoChartURL:          {regexp.MustCompile(`^https://`), "adres https"},
	OpenBaoChartSHA256:       {sha256Hex, "suma SHA-256"},
	ExternalSecretsVersion:   {semver, "wersja X.Y.Z"},
	ExternalSecretsURL:       {regexp.MustCompile(`^https://`), "adres https"},
	ExternalSecretsSHA256:    {sha256Hex, "suma SHA-256"},
	BuildKitImage:            {pinnedImage, "obraz z digestem"},
	SBOMGeneratorImage:       {pinnedImage, "obraz z digestem"},
	RegistryImage:            {pinnedImage, "obraz z digestem"},
	KubeconformSchemaCommit:  {gitCommit, "pełny SHA commita"},
	BaseAlpine:               {minorVersion, "wydanie X.Y"},
}

// chartURLs — URL charta musi zawierać jego wersję. W Bashu URL był
// składany z wersji; w pliku danych oba pola są wpisane wprost, więc mogą
// się rozjechać przy ręcznej aktualizacji.
var chartURLs = map[string]string{
	CoreDNSChartURL:     CoreDNSChartVersion,
	CertManagerChartURL: CertManagerChartVersion,
	OpenBaoChartURL:     OpenBaoChartVersion,
	ExternalSecretsURL:  ExternalSecretsVersion,
}

// Load czyta i sprawdza ci/pins.env w repozytorium o korzeniu root.
func Load(root string) (Pins, error) {
	path := filepath.Join(root, File)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("przypięcia: %w", err)
	}
	defer f.Close()
	p, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	return p, nil
}

// Parse czyta przypięcia w formacie KEY=VALUE i sprawdza każde z nich.
func Parse(r io.Reader) (Pins, error) {
	p := Pins{}
	scanner := bufio.NewScanner(r)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("linia %d: oczekiwano KEY=VALUE", line)
		}
		if !keyPattern.MatchString(key) {
			return nil, fmt.Errorf("linia %d: nazwa %q spoza postaci DF_[A-Z0-9_]+", line, key)
		}
		if !valuePattern.MatchString(value) {
			return nil, fmt.Errorf("linia %d: %s=%q — wartość ze znakami, które Bash zinterpretowałby inaczej niż Go", line, key, value)
		}
		if _, dup := p[key]; dup {
			return nil, fmt.Errorf("linia %d: %s zdefiniowane drugi raz", line, key)
		}
		p[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return p, p.validate()
}

func (p Pins) validate() error {
	var problems []string
	for key := range p {
		if _, known := rules[key]; !known {
			problems = append(problems, fmt.Sprintf("nieznane przypięcie %s", key))
		}
	}
	for key, r := range rules {
		value, ok := p[key]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("brak przypięcia %s", key))
		case !r.pattern.MatchString(value):
			problems = append(problems, fmt.Sprintf("%s=%s — oczekiwano: %s", key, value, r.what))
		}
	}
	for urlKey, versionKey := range chartURLs {
		if url, version := p[urlKey], p[versionKey]; url != "" && version != "" && !strings.Contains(url, version) {
			problems = append(problems, fmt.Sprintf("%s nie zawiera wersji %s=%s", urlKey, versionKey, version))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// Get zwraca przypięcie o kluczu z tego pakietu. Load i Parse gwarantują,
// że każde znane przypięcie istnieje, a klucze są stałymi — test pilnuje,
// że każda stała ma regułę.
func (p Pins) Get(key string) string {
	return p[key]
}

// Charty platformy.
const (
	CoreDNS         = "coredns"
	CertManager     = "cert-manager"
	EnvoyGateway    = "envoy-gateway"
	OpenBao         = "openbao"
	ExternalSecrets = "external-secrets"
)

// Chart — przypięty chart platformy.
type Chart struct {
	Name    string
	Version string
	// Source — https://…/chart.tgz albo oci://… (helm pull).
	Source string
	// SHA256 — suma archiwum charta z ci/pins.env, nie z serwera, z którego
	// idzie plik: suma z tego samego miejsca chroni tylko przed uszkodzeniem
	// w transferze, a nie przed podmianą.
	SHA256 string
}

// OCI mówi, czy chart pochodzi z rejestru OCI.
func (c Chart) OCI() bool { return strings.HasPrefix(c.Source, "oci://") }

// Chart zwraca przypięty chart platformy (CoreDNS, CertManager,
// EnvoyGateway, OpenBao, ExternalSecrets).
func (p Pins) Chart(name string) (Chart, error) {
	switch name {
	case CoreDNS:
		return Chart{Name: name, Version: p[CoreDNSChartVersion], Source: p[CoreDNSChartURL], SHA256: p[CoreDNSChartSHA256]}, nil
	case CertManager:
		return Chart{Name: name, Version: p[CertManagerChartVersion], Source: p[CertManagerChartURL], SHA256: p[CertManagerChartSHA256]}, nil
	case EnvoyGateway:
		return Chart{Name: name, Version: p[EnvoyGatewayChartVersion], Source: p[EnvoyGatewayChartRef], SHA256: p[EnvoyGatewayChartSHA256]}, nil
	case OpenBao:
		return Chart{Name: name, Version: p[OpenBaoChartVersion], Source: p[OpenBaoChartURL], SHA256: p[OpenBaoChartSHA256]}, nil
	case ExternalSecrets:
		return Chart{Name: name, Version: p[ExternalSecretsVersion], Source: p[ExternalSecretsURL], SHA256: p[ExternalSecretsSHA256]}, nil
	}
	return Chart{}, fmt.Errorf("nieznany chart platformy %q", name)
}

// ImageTag zwraca wersję obrazu z przypięcia „nazwa:tag@sha256:…".
func ImageTag(image string) string {
	ref, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		return ref[i+1:]
	}
	return ""
}
