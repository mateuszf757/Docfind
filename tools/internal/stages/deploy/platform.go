package deploy

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/poll"
	"github.com/mateuszf757/Docfind/tools/internal/stages/identity"
)

// platformChart — komponent platformy z przypiętego charta.
type platformChart struct {
	pin             string
	label           string
	namespace       string
	createNamespace bool
	values          string
	timeout         time.Duration
}

// platform instaluje komponenty platformy w kolejności wynikającej z tego,
// czego każdy potrzebuje przy starcie, a nie z wygody: CoreDNS przed
// wszystkim (bez niego żaden pod nie rozwiąże nazwy), Envoy Gateway przed
// cert-managerem (jego chart instaluje CRD Gateway API, a cert-manager
// z obsługą Gateway API bez nich nie czeka, tylko wpada w CrashLoopBackOff),
// na końcu chart platformy, który tworzy zasoby obu.
func (d Deployer) platform(ctx context.Context, c Cluster) error {
	for _, pc := range []platformChart{
		{pins.CoreDNS, "CoreDNS", "kube-system", false, "deploy/platform/coredns/values.yaml", 3 * time.Minute},
		{pins.EnvoyGateway, "Envoy Gateway", "envoy-gateway-system", true, "deploy/platform/envoy-gateway/values.yaml", 5 * time.Minute},
		// startupapicheck w charcie sprawia, że --wait kończy się dopiero
		// wtedy, gdy webhook cert-managera naprawdę przyjmuje zapisy.
		{pins.CertManager, "cert-manager", "cert-manager", true, "deploy/platform/cert-manager/values.yaml", 5 * time.Minute},
	} {
		chart, err := d.Pins.Chart(pc.pin)
		if err != nil {
			return err
		}
		archive, err := d.Charts.Fetch(ctx, chart)
		if err != nil {
			return err
		}
		cli.Step("%s: chart %s (suma zweryfikowana)", pc.label, chart.Version)
		if err := d.Releases.Install(ctx, d.target(), cluster.Release{
			Name: pc.pin, Chart: archive, Namespace: pc.namespace, CreateNamespace: pc.createNamespace,
			ValuesFiles: []string{d.path(pc.values)}, Timeout: pc.timeout,
		}); err != nil {
			return err
		}
	}

	gw := d.Env.Gateway
	// Let's Encrypt wymaga tokenu Cloudflare w klastrze. Bez niego wydawca by
	// powstał, a wyzwanie DNS-01 wisiałoby bez końca z błędem tylko w
	// statusie Challenge.
	if strings.HasPrefix(gw.Issuer, "letsencrypt") {
		exists, err := c.SecretExists(ctx, "cert-manager", "cloudflare-api-token")
		if err != nil {
			return err
		}
		if !exists {
			return cli.Unmet("wydawca %s wymaga tokenu Cloudflare w klastrze — %s", gw.Issuer, d.Env.Task("dns-token"))
		}
	}
	cli.Step("platforma: Gateway dla %s, wydawca %s", gw.Hostname, gw.Issuer)
	// Wartości z definicji tą samą funkcją, którą `dft test` renderuje
	// i sprawdza chart dla każdego środowiska.
	if err := d.Releases.Install(ctx, d.target(), cluster.Release{
		Name: "platform", Chart: d.path("deploy/charts/platform"), Namespace: gw.Namespace, CreateNamespace: true,
		Set: d.Env.PlatformValues(gw.ACMEEmail), Timeout: 3 * time.Minute,
	}); err != nil {
		return err
	}
	if err := waitReady(ctx, "Certificate cert-manager/docfind-root-ca (Ready)", 120*time.Second, func(ctx context.Context) (bool, error) {
		cert, err := c.Certificate(ctx, "cert-manager", "docfind-root-ca")
		return err == nil && cert.Ready, nil
	}); err != nil {
		return err
	}
	if err := waitReady(ctx, "Gateway "+gw.Namespace+"/"+gw.Name+" (Programmed)", 180*time.Second, func(ctx context.Context) (bool, error) {
		g, err := c.Gateway(ctx, gw.Namespace, gw.Name)
		return err == nil && g.Programmed, nil
	}); err != nil {
		return err
	}
	cli.Step("Gateway zaprogramowany")
	return nil
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
// (decyzja 31) z wersją profilu przypiętą do wersji klastra.
func (d Deployer) namespace(ctx context.Context, c Cluster) error {
	psa, err := cluster.PSAVersion(d.Pins.Get(pins.KubernetesVersion))
	if err != nil {
		return err
	}
	return c.EnsureNamespace(ctx, d.Env.App.Namespace, map[string]string{
		"docfind.io/gateway-routes":                  "allowed",
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/enforce-version": psa,
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/warn-version":    psa,
	})
}

// app instaluje chart aplikacji z obrazem img i sprawdza, że to, co stoi na
// klastrze, zgadza się z tym, co wdrażano — obraz, wersja i commit na każdym
// podzie, a liczba podów z liczbą replik.
func (d Deployer) app(ctx context.Context, c Cluster, img image) error {
	release := d.Env.App.Release
	cli.Step("helm upgrade --install %s", release)
	values := append(d.Env.AppValues(), "api.image.tag="+img.tag)
	if img.digest != "" {
		values = append(values, "api.image.digest="+img.digest)
	}
	if err := d.Releases.Install(ctx, d.target(), cluster.Release{
		Name: release, Chart: d.path("deploy/charts/docfind"), Namespace: d.Env.App.Namespace, Set: values, Timeout: 3 * time.Minute,
	}); err != nil {
		return err
	}
	_, err := identity.Verify(ctx, c, d.Env.App.Namespace, release+"-api", "api", img.want)
	return err
}
