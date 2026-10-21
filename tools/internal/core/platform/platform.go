// Package platform opisuje komponenty platformy z przypiętych chartów
// (ci/pins.env): gdzie stają, z jakimi wartościami i kto nimi zarządza tam,
// gdzie platformę przejmuje Argo CD. Jedno miejsce dla wdrożenia przez dft,
// renderu w `dft test` i lustra chartów w rejestrze (decyzja 80) — wcześniej
// każde z nich trzymało własną listę.
package platform

import (
	"fmt"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/core/secretstore"
)

// Component — komponent platformy z przypiętego charta.
type Component struct {
	// Pin — chart w ci/pins.env (pins.CertManager, …); też nazwa wydania.
	Pin   string
	Label string
	// Namespace i CreateNamespace — przestrzeń wydania; kube-system już jest,
	// a openbao i argocd zakłada dft przed chartem (klucz odpieczętowania,
	// etykiety Pod Security).
	Namespace       string
	CreateNamespace bool
	// Values — plik wartości względem korzenia repozytorium.
	Values string
	// Timeout — czekanie Helma przy instalacji przez dft.
	Timeout time.Duration
	// CRDDir — chart trzyma CRD w katalogu crds/: render w `dft test`
	// z --include-crds.
	CRDDir bool
	// Schemas — z CRD tego charta powstają schematy kubeconform (decyzja 28).
	Schemas bool
	// Bootstrap — dft instaluje go zawsze, także tam, gdzie platformą
	// zarządza Argo CD: bez CoreDNS Argo nie rozwiąże nazwy rejestru, a Argo
	// CD nie zainstaluje sam siebie. Pozostałe trafiają do lustra w rejestrze.
	Bootstrap bool
}

// Components — w kolejności instalacji przez dft. Uzasadnienie kolejności
// jest przy instalacji (stages/deploy).
var Components = []Component{
	{Pin: pins.CoreDNS, Label: "CoreDNS", Namespace: "kube-system", Values: "deploy/platform/coredns/values.yaml", Timeout: 3 * time.Minute, Bootstrap: true},
	{Pin: pins.EnvoyGateway, Label: "Envoy Gateway", Namespace: "envoy-gateway-system", CreateNamespace: true, Values: "deploy/platform/envoy-gateway/values.yaml", Timeout: 5 * time.Minute, CRDDir: true, Schemas: true},
	{Pin: pins.CertManager, Label: "cert-manager", Namespace: "cert-manager", CreateNamespace: true, Values: "deploy/platform/cert-manager/values.yaml", Timeout: 5 * time.Minute, Schemas: true},
	{Pin: pins.ExternalSecrets, Label: "External Secrets Operator", Namespace: "external-secrets", CreateNamespace: true, Values: "deploy/platform/external-secrets/values.yaml", Timeout: 5 * time.Minute, CRDDir: true, Schemas: true},
	{Pin: pins.OpenBao, Label: "OpenBao", Namespace: secretstore.Namespace, Values: secretstore.Dir + "/values.yaml", Timeout: 5 * time.Minute},
	{Pin: pins.ArgoCD, Label: "Argo CD", Namespace: environment.ArgoCDNamespace, Values: "deploy/platform/argocd/values.yaml", Timeout: 5 * time.Minute, Schemas: true, Bootstrap: true},
}

// Get zwraca komponent o przypięciu pin.
func Get(pin string) (Component, error) {
	for _, c := range Components {
		if c.Pin == pin {
			return c, nil
		}
	}
	return Component{}, fmt.Errorf("nieznany komponent platformy %q", pin)
}

// Mirrored — komponenty, których charty trafiają do lustra w rejestrze: te,
// którymi zarządza Argo CD tam, gdzie platformę przejmuje.
func Mirrored() []Component {
	var out []Component
	for _, c := range Components {
		if !c.Bootstrap {
			out = append(out, c)
		}
	}
	return out
}
