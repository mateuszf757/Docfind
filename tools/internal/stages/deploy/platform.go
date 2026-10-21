package deploy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/core/platform"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
	"github.com/mateuszf757/Docfind/tools/internal/stages/identity"
)

// platform instaluje komponenty platformy w kolejności wynikającej z tego,
// czego każdy potrzebuje przy starcie, a nie z wygody: CoreDNS przed
// wszystkim (bez niego żaden pod nie rozwiąże nazwy), Envoy Gateway przed
// cert-managerem (jego chart instaluje CRD Gateway API, a cert-manager
// z obsługą Gateway API bez nich nie czeka, tylko wpada w CrashLoopBackOff),
// External Secrets Operator (jego CRD są w charcie platformy), potem chart
// platformy, który tworzy zasoby wszystkich, i na końcu OpenBao — jego
// certyfikat wystawia wewnętrzne CA z charta platformy.
func (d Deployer) platform(ctx context.Context, c Cluster) error {
	for _, pin := range []string{
		pins.CoreDNS,
		pins.EnvoyGateway,
		// startupapicheck w charcie sprawia, że --wait kończy się dopiero
		// wtedy, gdy webhook cert-managera naprawdę przyjmuje zapisy.
		pins.CertManager,
		// Przed chartem platformy, który tworzy SecretStore i ExternalSecret —
		// bez CRD ESO Helm odrzuciłby cały chart. --wait czeka na webhook.
		pins.ExternalSecrets,
	} {
		if err := d.installComponent(ctx, pin, false); err != nil {
			return err
		}
	}

	gw := d.Env.Gateway
	cli.Step("platforma: Gateway dla %s, wydawca %s", gw.Hostname, gw.Issuer)
	// Bez czekania Helma na gotowość. Helm 4 czeka też na zasoby
	// niestandardowe, a w tym charcie są takie, które gotowe będą dopiero po
	// OpenBao: SecretStore i ExternalSecret tokenu DNS oraz certyfikat Gateway
	// od Let's Encrypt, który tego tokenu potrzebuje. OpenBao instaluje się po
	// chartcie (jego certyfikat wystawia CA z tego charta), więc czekanie
	// Helma kończyło się po 3 minutach na stagingu — pierwszym klastrze
	// z Let's Encrypt i świeżym OpenBao. Gotowości pilnuje dft niżej,
	// w kolejności zależności.
	// Wartości z definicji tą samą funkcją, którą `dft test` renderuje
	// i sprawdza chart dla każdego środowiska.
	if err := d.Releases.Install(ctx, d.target(), cluster.Release{
		Name: "platform", Chart: d.path("deploy/charts/platform"), Namespace: gw.Namespace, CreateNamespace: true,
		Set: d.Env.PlatformValues(gw.ACMEEmail), Timeout: 3 * time.Minute, HooksOnly: true,
	}); err != nil {
		return err
	}
	if err := waitReady(ctx, "Certificate cert-manager/docfind-root-ca (Ready)", 120*time.Second, func(ctx context.Context) (bool, error) {
		cert, err := c.Certificate(ctx, "cert-manager", "docfind-root-ca")
		return err == nil && cert.Ready, nil
	}); err != nil {
		return err
	}
	if err := d.secretStore(ctx); err != nil {
		return err
	}
	// Let's Encrypt wymaga tokenu Cloudflare. Bez niego wydawca powstaje,
	// a wyzwanie DNS-01 wisi bez końca z błędem tylko w statusie Challenge.
	// Sprawdzane dopiero po OpenBao: token żyje w OpenBao, a na nowym
	// klastrze `dns-token` ma gdzie go zapisać dopiero teraz.
	if strings.HasPrefix(gw.Issuer, "letsencrypt") {
		tokens, err := d.DNSTokens(d.target())
		if err != nil {
			return err
		}
		if err := tokens.Ensure(ctx, d.Env.Task("dns-token")); err != nil {
			return err
		}
	}
	if err := waitReady(ctx, "Gateway "+gw.Namespace+"/"+gw.Name+" (Programmed)", 180*time.Second, func(ctx context.Context) (bool, error) {
		g, err := c.Gateway(ctx, gw.Namespace, gw.Name)
		return err == nil && g.Programmed, nil
	}); err != nil {
		return err
	}
	// Certyfikat listenera HTTPS (cert-manager nazywa go od sekretu
	// w certificateRefs). Dawniej czekał na niego Helm; przy Let's Encrypt
	// to wyzwanie DNS-01 — do kilku minut.
	tls := gw.Name + "-tls"
	if err := waitReady(ctx, "Certificate "+gw.Namespace+"/"+tls+" (Ready)", 5*time.Minute, func(ctx context.Context) (bool, error) {
		cert, err := c.Certificate(ctx, gw.Namespace, tls)
		return err == nil && cert.Ready, nil
	}); err != nil {
		return err
	}
	cli.Step("Gateway zaprogramowany, certyfikat %s gotowy", tls)
	// Argo CD na końcu: niczego z platformy nie potrzebuje do startu, a to,
	// co wdroży (aplikacja), potrzebuje jej całej.
	if d.Env.GitOps() {
		return d.argoCD(ctx, c)
	}
	return nil
}

// secretStore instaluje OpenBao (decyzja 42): klucz odpieczętowania
// i przestrzeń nazw przed chartem, inicjalizacja i konfiguracja po nim.
// Helm nie czeka na gotowość podów — staną się gotowe dopiero po
// inicjalizacji, której Helm nie zrobi.
func (d Deployer) secretStore(ctx context.Context) error {
	ss, err := d.SecretStores(d.target())
	if err != nil {
		return err
	}
	if err := ss.Prepare(ctx); err != nil {
		return err
	}
	if err := d.installComponent(ctx, pins.OpenBao, true); err != nil {
		return err
	}
	_, err = ss.Bootstrap(ctx)
	return err
}

// installComponent instaluje komponent platformy z przypiętego charta,
// pobranego i sprawdzonego sumą z ci/pins.env. hooksOnly — Helm nie czeka
// na gotowość zasobów (OpenBao: pody gotowe dopiero po inicjalizacji).
func (d Deployer) installComponent(ctx context.Context, pin string, hooksOnly bool) error {
	pc, err := platform.Get(pin)
	if err != nil {
		return err
	}
	chart, err := d.Pins.Chart(pin)
	if err != nil {
		return err
	}
	archive, err := d.Charts.Fetch(ctx, chart)
	if err != nil {
		return err
	}
	cli.Step("%s: chart %s (suma zweryfikowana)", pc.Label, chart.Version)
	return d.Releases.Install(ctx, d.target(), cluster.Release{
		Name: pc.Pin, Chart: archive, Namespace: pc.Namespace, CreateNamespace: pc.CreateNamespace,
		ValuesFiles: []string{d.path(pc.Values)}, Timeout: pc.Timeout, HooksOnly: hooksOnly,
	})
}

// waitReady czeka na warunek zasobu; obiekt, którego jeszcze nie ma, to
// kolejna próba, a nie błąd.
func waitReady(ctx context.Context, what string, timeout time.Duration, ready func(context.Context) (bool, error)) error {
	err := poll.Until(ctx, timeout, 2*time.Second, ready)
	if errors.Is(err, poll.ErrTimeout) {
		return cli.Unmet("%s: warunek niespełniony po %v", what, timeout)
	}
	return err
}

// namespace zakłada przestrzeń nazw aplikacji z etykietami, które są zgodą
// platformy, a nie aplikacji samej sobie: trasa może podpiąć się pod
// listener HTTPS (gateway.routeNamespaceLabel) i Pod Security restricted
// (decyzja 31).
func (d Deployer) namespace(ctx context.Context, c Cluster) error {
	labels, err := d.restricted()
	if err != nil {
		return err
	}
	labels["docfind.io/gateway-routes"] = "allowed"
	return c.EnsureNamespace(ctx, d.Env.App.Namespace, labels)
}

// restricted — etykiety przestrzeni nazw z Pod Security restricted
// (decyzja 31), z wersją profilu przypiętą do wersji klastra.
func (d Deployer) restricted() (map[string]string, error) {
	psa, err := cluster.PSAVersion(d.Pins.Get(pins.KubernetesVersion))
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/enforce-version": psa,
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/warn-version":    psa,
	}, nil
}

// appSecrets przygotowuje sekrety aplikacji z OpenBao przez ESO (decyzja
// 44) i zwraca CA, którym ESO sprawdza OpenBao (PEM w base64). Sekret musi
// być w OpenBao przed chartem — pody czekają na Secret przy montowaniu
// wolumenu. Klaster spoza k3d (klient) dostarcza Secret sam: bez CA.
func (d Deployer) appSecrets(ctx context.Context, c Cluster) (string, error) {
	if d.Env.Cluster.Provider != environment.ProviderK3d {
		return "", nil
	}
	ss, err := d.SecretStores(d.target())
	if err != nil {
		return "", err
	}
	if err := ss.EnsureAppSecrets(ctx); err != nil {
		return "", err
	}
	ca, err := c.SecretData(ctx, "cert-manager", "docfind-root-ca")
	if err != nil {
		return "", fmt.Errorf("CA platformy cert-manager/docfind-root-ca: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ca["ca.crt"]), nil
}

// app instaluje chart aplikacji z obrazem img i sprawdza, że to, co stoi na
// klastrze, zgadza się z tym, co wdrażano — obraz, wersja i commit na każdym
// podzie, a liczba podów z liczbą replik.
func (d Deployer) app(ctx context.Context, c Cluster, img image, packaged bool) error {
	release := d.Env.App.Release
	caBundle, err := d.appSecrets(ctx, c)
	if err != nil {
		return err
	}
	cli.Step("helm upgrade --install %s", release)
	// Wersja wdrożenia trafia do aplikacji zmienną środowiskową
	// (DOCFIND_VERSION) — różnica między wdrożeniami idzie przez wdrożenie,
	// a nie przez obraz (decyzja 40).
	image := []string{"api.image.tag=" + img.tag, "api.version=" + img.want.Version}
	if img.digest != "" {
		image = append(image, "api.image.digest="+img.digest)
	}
	chartSource := d.path("deploy/charts/docfind")
	if packaged {
		// Ten sam artefakt co do publikacji: obraz i wersja wdrożenia są
		// w wartościach archiwum, nie w --set (decyzja 61).
		if img.digest == "" {
			return cli.Usage("spakowany chart tylko z obrazem z rejestru (--image)")
		}
		archive, digest, err := d.Package(ctx, d.Image+"@"+img.digest)
		if err != nil {
			return err
		}
		cli.Step("chart spakowany jak do publikacji: %s (%s)", filepath.Base(archive), digest)
		chartSource, image = archive, nil
	}
	if err := d.Releases.Install(ctx, d.target(), cluster.Release{
		Name: release, Chart: chartSource, Namespace: d.Env.App.Namespace,
		Set: d.Env.AppValues(caBundle), SetString: image, Timeout: 3 * time.Minute,
	}); err != nil {
		return err
	}
	_, err = identity.Verify(ctx, c, d.Env.App.Namespace, release+"-api", "api", img.want)
	return err
}
