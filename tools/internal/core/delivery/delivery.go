// Package delivery to rdzeń pakietu dostawy (decyzja 81): projekty
// i Applications Argo CD dla platformy i aplikacji, po digestach artefaktów
// z jednego commita, w falach, z wartościami środowisk z definicji. Pakiet
// publikuje CI po bramkach klastra, a na środowisku śledzi go główna
// Application.
package delivery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"sigs.k8s.io/yaml"

	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/core/platform"
)

// Component — Application w pakiecie dostawy (decyzja 81): chart z rejestru
// OCI po digeście, w fali, z wartościami komponentu i środowiska.
type Component struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Wave    int    `json:"wave"`
	RepoURL string `json:"repoURL"`
	Digest  string `json:"digest"`
	// ReleaseName — nazwa wydania Helma, jak przy instalacji przez dft:
	// etykiety zasobów zostają te same, więc Argo przejmuje zasoby wydania
	// bez ich zmiany.
	ReleaseName     string         `json:"releaseName"`
	Namespace       string         `json:"namespace"`
	CreateNamespace bool           `json:"createNamespace"`
	ServerSideApply bool           `json:"serverSideApply"`
	Prune           bool           `json:"prune"`
	Values          map[string]any `json:"values"`
	// EnvironmentValues — klucz w Environments domieszany do Values:
	// "platform", "docfind" albo pusty.
	EnvironmentValues string `json:"environmentValues"`
	// ACME — domieszać adres konta ACME z głównej Application.
	ACME bool `json:"acme"`
}

// EnvironmentValues — wartości środowiska w pakiecie: z definicji, bez
// niczego z klastra.
type EnvironmentValues struct {
	Platform map[string]any `json:"platform"`
	Docfind  map[string]any `json:"docfind"`
}

// Bundle — to, co dft wpisuje do pakietu przy pakowaniu.
type Bundle struct {
	Components   []Component                  `json:"components"`
	Environments map[string]EnvironmentValues `json:"environments"`
}

// Projekty Argo CD w pakiecie.
const (
	PlatformProject = "platform"
	AppProject      = "docfind"
)

// Nazwy Applications charta platformy: dwie części jednego artefaktu
// (parts.pki i parts.ingress w deploy/charts/platform).
const (
	PlatformPKI = "platform-pki"
	Platform    = "platform"
)

// mirrored — jak Argo CD stosuje charty z lustra: fala (kolejność zależności
// przy starcie, jak przy instalacji przez dft), Server-Side Apply dla CRD
// większych niż limit adnotacji last-applied-configuration, i bez usuwania
// zasobów, które znikną z nowej wersji cudzego charta — CRD razem z ich
// instancjami albo dane OpenBao.
var mirrored = map[string]struct {
	wave       int
	ssa, prune bool
}{
	pins.EnvoyGateway:    {wave: 0, ssa: true},
	pins.CertManager:     {wave: 1, ssa: true},
	pins.ExternalSecrets: {wave: 2, ssa: true},
	pins.OpenBao:         {wave: 4},
}

// Fale charta platformy i aplikacji: własne CA przed OpenBao (wystawia mu
// certyfikat), reszta platformy po nim (SecretStore i ExternalSecret tokenu
// DNS), aplikacja na końcu.
const (
	pkiWave      = 3
	platformWave = 5
	appWave      = 6
)

// Artifacts — artefakty jednego commita, z których powstaje pakiet:
// digesty lustra (po przypięciu), chart platformy i chart aplikacji
// (repozytorium bez schematu i digest) oraz wartości komponentów lustra
// (plik values.yaml z deploy/platform, po przypięciu).
type Artifacts struct {
	Mirror         map[string]string
	MirrorRepo     func(pin string) string
	MirrorValues   map[string]map[string]any
	PlatformRepo   string
	PlatformDigest string
	AppRepo        string
	AppDigest      string
}

// Compose składa pakiet dla środowisk z GitOps envs: komponenty lustra,
// dwie części charta platformy i aplikację, w falach, z wartościami każdego
// środowiska z definicji. Środowiska muszą dzielić przestrzenie nazw
// i nazwy wydań — pakiet jest jeden dla wszystkich.
func Compose(a Artifacts, envs []environment.Definition) (Bundle, error) {
	if len(envs) == 0 {
		return Bundle{}, fmt.Errorf("pakiet dostawy bez środowiska z delivery.argocd")
	}
	first := envs[0]
	b := Bundle{Environments: map[string]EnvironmentValues{}}
	for _, e := range envs {
		if !e.GitOps() {
			return Bundle{}, fmt.Errorf("środowisko %s bez delivery.argocd w pakiecie dostawy", e.Name)
		}
		if e.App != first.App || e.Gateway.Namespace != first.Gateway.Namespace {
			return Bundle{}, fmt.Errorf("środowiska %s i %s: różne przestrzenie nazw albo wydania — pakiet dostawy jest jeden dla wszystkich", first.Name, e.Name)
		}
		pv, err := environment.SetValues(e.PlatformValues(""))
		if err != nil {
			return Bundle{}, err
		}
		av, err := environment.SetValues(e.AppValues(""))
		if err != nil {
			return Bundle{}, err
		}
		b.Environments[e.Name] = EnvironmentValues{Platform: pv, Docfind: av}
	}
	for _, pc := range platform.Mirrored() {
		m, ok := mirrored[pc.Pin]
		if !ok {
			return Bundle{}, fmt.Errorf("komponent %s w lustrze bez ustawień Argo CD (core/gitops)", pc.Pin)
		}
		digest := a.Mirror[pc.Pin]
		if digest == "" {
			return Bundle{}, fmt.Errorf("brak digestu lustra dla %s", pc.Pin)
		}
		b.Components = append(b.Components, Component{
			Name: pc.Pin, Project: PlatformProject, Wave: m.wave,
			RepoURL: "oci://" + a.MirrorRepo(pc.Pin), Digest: digest, ReleaseName: pc.Pin,
			Namespace: pc.Namespace, CreateNamespace: pc.CreateNamespace, ServerSideApply: m.ssa, Prune: m.prune,
			Values: nonNil(a.MirrorValues[pc.Pin]),
		})
	}
	part := func(name string, wave int, pki, ingress bool) Component {
		return Component{
			Name: name, Project: PlatformProject, Wave: wave,
			RepoURL: "oci://" + a.PlatformRepo, Digest: a.PlatformDigest, ReleaseName: "platform",
			Namespace: first.Gateway.Namespace, CreateNamespace: true, Prune: true,
			Values:            map[string]any{"parts": map[string]any{"pki": pki, "ingress": ingress}},
			EnvironmentValues: "platform", ACME: ingress,
		}
	}
	b.Components = append(b.Components,
		part(PlatformPKI, pkiWave, true, false),
		part(Platform, platformWave, false, true),
		Component{
			Name: first.App.Release, Project: AppProject, Wave: appWave,
			RepoURL: "oci://" + a.AppRepo, Digest: a.AppDigest, ReleaseName: first.App.Release,
			Namespace: first.App.Namespace, Prune: true, Values: map[string]any{}, EnvironmentValues: "docfind",
		},
	)
	slices.SortStableFunc(b.Components, func(x, y Component) int { return x.Wave - y.Wave })
	return b, nil
}

// App zwraca komponent aplikacji z pakietu.
func (b Bundle) App() (Component, bool) {
	for _, c := range b.Components {
		if c.Project == AppProject {
			return c, true
		}
	}
	return Component{}, false
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Values — gałęzie values.yaml pakietu, które wypełnia dft.
func (b Bundle) Values() map[string]any {
	return map[string]any{"components": b.Components, "environments": b.Environments}
}

// FromValues czyta pakiet z values.yaml charta pobranego z rejestru.
func FromValues(values map[string]any) (Bundle, error) {
	raw, err := json.Marshal(map[string]any{"components": values["components"], "environments": values["environments"]})
	if err != nil {
		return Bundle{}, err
	}
	var b Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return Bundle{}, fmt.Errorf("wartości pakietu dostawy: %w", err)
	}
	if len(b.Components) == 0 {
		return Bundle{}, fmt.Errorf("pakiet dostawy bez komponentów")
	}
	return b, nil
}

// MirrorValues czyta wartości komponentów lustra z repozytorium o korzeniu
// root (deploy/platform/<komponent>/values.yaml) — te same pliki, które dft
// podaje Helmowi przy instalacji bez Argo CD.
func MirrorValues(root string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	for _, pc := range platform.Mirrored() {
		raw, err := os.ReadFile(filepath.Join(root, pc.Values))
		if err != nil {
			return nil, err
		}
		var v map[string]any
		if err := yaml.Unmarshal(raw, &v); err != nil {
			return nil, fmt.Errorf("%s: %w", pc.Values, err)
		}
		out[pc.Pin] = v
	}
	return out, nil
}
