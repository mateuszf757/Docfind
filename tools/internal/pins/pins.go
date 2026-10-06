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

// rules — znane przypięcia. Nieznany klucz to literówka, a brak znanego —
// przypięcie, które zniknęło; oba są błędem.
var rules = map[string]rule{
	"DF_KUBERNETES_VERSION":          {semver, "wersja X.Y.Z"},
	"DF_K3S_IMAGE":                   {pinnedImage, "obraz z digestem"},
	"DF_COREDNS_CHART_VERSION":       {semver, "wersja X.Y.Z"},
	"DF_COREDNS_CHART_URL":           {regexp.MustCompile(`^https://`), "adres https"},
	"DF_COREDNS_CHART_SHA256":        {sha256Hex, "suma SHA-256"},
	"DF_CERT_MANAGER_CHART_VERSION":  {regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`), "wersja vX.Y.Z"},
	"DF_CERT_MANAGER_CHART_URL":      {regexp.MustCompile(`^https://`), "adres https"},
	"DF_CERT_MANAGER_CHART_SHA256":   {sha256Hex, "suma SHA-256"},
	"DF_ENVOY_GATEWAY_CHART_REF":     {regexp.MustCompile(`^oci://`), "referencja oci://"},
	"DF_ENVOY_GATEWAY_CHART_VERSION": {regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`), "wersja vX.Y.Z"},
	"DF_ENVOY_GATEWAY_CHART_SHA256":  {sha256Hex, "suma SHA-256"},
	"DF_CURL_IMAGE":                  {pinnedImage, "obraz z digestem"},
	"DF_BUILDKIT_IMAGE":              {pinnedImage, "obraz z digestem"},
	"DF_KUBECONFORM_SCHEMA_COMMIT":   {gitCommit, "pełny SHA commita"},
	"DF_BASE_ALPINE":                 {minorVersion, "wydanie X.Y"},
}

// chartURLs — URL charta musi zawierać jego wersję. W Bashu URL był
// składany z wersji; w pliku danych oba pola są wpisane wprost, więc mogą
// się rozjechać przy ręcznej aktualizacji.
var chartURLs = map[string]string{
	"DF_COREDNS_CHART_URL":      "DF_COREDNS_CHART_VERSION",
	"DF_CERT_MANAGER_CHART_URL": "DF_CERT_MANAGER_CHART_VERSION",
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

// Get zwraca przypięcie, które musi istnieć — validate to gwarantuje.
func (p Pins) Get(key string) string {
	value, ok := p[key]
	if !ok {
		panic(fmt.Sprintf("pins: odwołanie do nieznanego przypięcia %s", key))
	}
	return value
}

// ImageTag zwraca wersję obrazu z przypięcia „nazwa:tag@sha256:…".
func ImageTag(image string) string {
	ref, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		return ref[i+1:]
	}
	return ""
}
