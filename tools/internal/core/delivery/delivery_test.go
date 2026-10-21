package delivery

import (
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
)

const repoRoot = "../../../.."

func staging(t *testing.T) environment.Definition {
	t.Helper()
	e, err := environment.Read(repoRoot, "staging")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func artifacts() Artifacts {
	d := "sha256:" + strings.Repeat("a", 64)
	return Artifacts{
		Mirror:       map[string]string{pins.EnvoyGateway: d, pins.CertManager: d, pins.ExternalSecrets: d, pins.OpenBao: d},
		MirrorRepo:   func(pin string) string { return "ghcr.io/x/charts/platform/" + pin },
		MirrorValues: map[string]map[string]any{pins.CertManager: {"crds": map[string]any{"enabled": true}}},
		PlatformRepo: "ghcr.io/x/charts/docfind-platform", PlatformDigest: d,
		AppRepo: "ghcr.io/x/charts/docfind", AppDigest: d,
	}
}

// TestCompose: komponenty w kolejności zależności przy starcie, chart
// platformy w dwóch częściach wokół OpenBao, aplikacja na końcu; wartości
// środowiska z definicji bez niczego z klastra.
func TestCompose(t *testing.T) {
	b, err := Compose(artifacts(), []environment.Definition{staging(t)})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for i, c := range b.Components {
		names = append(names, c.Name)
		if i > 0 && c.Wave <= b.Components[i-1].Wave {
			t.Errorf("%s w fali %d, poprzedni w %d — każdy komponent w swojej fali", c.Name, c.Wave, b.Components[i-1].Wave)
		}
		if !strings.HasPrefix(c.RepoURL, "oci://") || c.Values == nil {
			t.Errorf("%s: %+v", c.Name, c)
		}
	}
	if got := strings.Join(names, ","); got != "envoy-gateway,cert-manager,external-secrets,platform-pki,openbao,platform,docfind" {
		t.Errorf("kolejność: %s", got)
	}
	cm := b.Components[1]
	if !cm.ServerSideApply || cm.Prune || !cm.CreateNamespace || cm.Values["crds"] == nil {
		t.Errorf("cert-manager: %+v", cm)
	}
	pki, ingress := b.Components[3], b.Components[5]
	if pki.ReleaseName != "platform" || ingress.ReleaseName != "platform" || pki.ACME || !ingress.ACME || pki.EnvironmentValues != "platform" {
		t.Errorf("części charta platformy: %+v, %+v", pki, ingress)
	}
	if b.Components[4].CreateNamespace {
		t.Error("openbao z CreateNamespace — przestrzeń z kluczem odpieczętowania zakłada dft")
	}
	app, ok := b.App()
	if !ok || app.Namespace != "docfind" || app.CreateNamespace || app.Project != AppProject {
		t.Errorf("aplikacja: %+v", app)
	}
	env := b.Environments["staging"]
	if env.Docfind["api"].(map[string]any)["secrets"].(map[string]any)["externalSecret"].(map[string]any)["caConfigMap"] != environment.PlatformCAConfigMap {
		t.Errorf("aplikacja bez CA z ConfigMapy: %v", env.Docfind)
	}
	if env.Platform["acme"].(map[string]any)["email"] != "" {
		t.Errorf("adres ACME w pakiecie: %v", env.Platform["acme"])
	}
	// Pakiet w wartościach charta i z powrotem.
	back, err := FromValues(map[string]any{"components": b.Values()["components"], "environments": b.Values()["environments"]})
	if err != nil || len(back.Components) != len(b.Components) || back.Components[6].Digest != app.Digest {
		t.Errorf("FromValues: %+v, %v", back, err)
	}
}

func TestComposeRefuses(t *testing.T) {
	if _, err := Compose(artifacts(), nil); err == nil {
		t.Error("pakiet bez środowiska przyjęty")
	}
	a := artifacts()
	delete(a.Mirror, pins.OpenBao)
	if _, err := Compose(a, []environment.Definition{staging(t)}); err == nil || !strings.Contains(err.Error(), "openbao") {
		t.Errorf("brak digestu lustra: %v", err)
	}
	dev, err := environment.Read(repoRoot, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Compose(artifacts(), []environment.Definition{dev}); err == nil {
		t.Error("środowisko bez Argo CD w pakiecie przyjęte")
	}
	other := staging(t)
	other.Name, other.App.Namespace = "qa", "inna"
	if _, err := Compose(artifacts(), []environment.Definition{staging(t), other}); err == nil {
		t.Error("środowiska z różnymi przestrzeniami nazw przyjęte")
	}
	if _, err := FromValues(map[string]any{}); err == nil {
		t.Error("pakiet bez komponentów przyjęty")
	}
}
