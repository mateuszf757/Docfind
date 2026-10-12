// Package charts sprawdza charty Helma tak, jak trafiłyby na klaster: lint,
// testy odrzucenia przez values.schema.json, render każdej kombinacji
// chart × środowisko do pliku, a na nim kubeconform ze schematami
// z przypiętych źródeł, polityki i konfigurację aplikacji jej modelem
// (dawniej część ci/run-tests.sh).
package charts

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/crdschema"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/core/policy"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/manifest"
)

// Helm — render i lint chartów (adapters/helm). Odmowa Helma to warunek
// niespełniony.
type Helm interface {
	Lint(ctx context.Context, chart string, set ...string) error
	Template(ctx context.Context, r cluster.Release, includeCRDs bool, w io.Writer) error
}

// ChartSource — przypięte charty platformy (adapters/helm).
type ChartSource interface {
	Fetch(ctx context.Context, chart pins.Chart) (string, error)
}

// Validator — walidacja manifestów schematami (adapters/kubeconform).
type Validator interface {
	Validate(ctx context.Context, manifests, crdSchemas string) error
}

// AppConfig — walidacja konfiguracji modelem aplikacji (adapters/uv).
type AppConfig interface {
	CheckConfig(ctx context.Context, path string) error
}

// Checker — sprawdzenia chartów w repozytorium o korzeniu Root.
type Checker struct {
	Root      string
	Pins      pins.Pins
	Helm      Helm
	Charts    ChartSource
	Validator Validator
	AppConfig AppConfig
}

// render to jeden wyrenderowany zestaw manifestów.
type render struct {
	name string
	path string
	// requireDigest — obrazy z zewnątrz (komponenty platformy). Nasz obraz
	// w renderze testowym ma tag „lint"; jego tożsamość gwarantuje build,
	// a w dostawie do klienta wejdzie digest z rejestru.
	requireDigest bool
}

// Run sprawdza wszystko i kończy się na pierwszej porażce — jak dawny skrypt:
// każdy krok zakłada, że poprzednie przeszły.
func (c Checker) Run(ctx context.Context, tmp string) error {
	app := c.path("deploy/charts/docfind")
	platform := c.path("deploy/charts/platform")

	cli.Step("helm lint docfind")
	if err := c.Helm.Lint(ctx, app, "api.image.tag=lint"); err != nil {
		return err
	}
	// values.schema.json ma odrzucić literówkę — bez schematu Helm po cichu
	// ignoruje nieznany klucz. Sprawdzamy odrzucenie, a nie samo istnienie
	// pliku: schemat, który wszystko przepuszcza, też by „istniał".
	if err := c.mustReject(ctx, "chart przyjął nieznany klucz api.replica — values.schema.json niczego nie pilnuje",
		cluster.Release{Name: "docfind", Chart: app, Set: []string{"api.image.tag=lint", "api.replica=3"}}); err != nil {
		return err
	}
	// Digest skrócony albo z literówką ma odpaść na schemacie, a nie przy
	// pobieraniu obrazu na węźle; poprawny ma trafić do referencji obrazu.
	if err := c.mustReject(ctx, "chart przyjął skrócony digest api.image.digest — values.schema.json go nie pilnuje",
		cluster.Release{Name: "docfind", Chart: app, Set: []string{"api.image.tag=lint", "api.image.digest=sha256:abc"}}); err != nil {
		return err
	}
	// Wersja wdrożenia też: tag wydania (v0.4.0) albo cokolwiek spoza SemVer
	// ma odpaść na schemacie, zanim /version zacznie to podawać.
	if err := c.mustReject(ctx, "chart przyjął api.version=v0.4.0 — values.schema.json nie pilnuje wersji",
		cluster.Release{Name: "docfind", Chart: app, Set: []string{"api.image.tag=lint"}, SetString: []string{"api.version=v0.4.0"}}); err != nil {
		return err
	}
	// Obraz i wersja tak, jak podaje je wdrożenie (--set-string): tag z samych
	// cyfr — skrócony commit — zostaje tekstem, digest trafia do referencji
	// obrazu, a wersja do DOCFIND_VERSION (decyzja 40).
	digest := "sha256:" + strings.Repeat("0", 64)
	var out strings.Builder
	if err := c.Helm.Template(ctx, cluster.Release{Name: "docfind", Chart: app,
		SetString: []string{"api.image.tag=123456789012", "api.image.digest=" + digest, "api.version=0.4.0"}}, false, &out); err != nil {
		return err
	}
	image, version, err := apiContainer(out.String())
	if err != nil {
		return err
	}
	switch {
	case !strings.HasSuffix(image, "/docfind-api:123456789012@"+digest):
		return cli.Unmet("api.image.tag i api.image.digest nie trafiły do referencji obrazu w Deploymencie (jest %q)", image)
	case version != "0.4.0":
		return cli.Unmet("api.version nie trafiło do DOCFIND_VERSION kontenera api (jest %q)", version)
	}

	var renders []render
	add := func(name string, requireDigest, includeCRDs bool, r cluster.Release) error {
		path := filepath.Join(tmp, name+".yaml")
		if err := c.renderTo(ctx, path, r, includeCRDs); err != nil {
			return err
		}
		renders = append(renders, render{name: name, path: path, requireDigest: requireDigest})
		return nil
	}
	// Chart aplikacji i chart platformy w wariancie każdego środowiska,
	// z wartościami z definicji (deploy/environments) — tą samą funkcją,
	// którą wdrożenie przekazuje Helmowi. Definicje z gita, bez nadpisań
	// z mise.local.toml: lokalnie sprawdzane jest to samo co w CI.
	envs, err := environment.All(c.Root)
	if err != nil {
		return err
	}
	var appRenders []string
	for _, e := range envs {
		r := cluster.Release{Name: e.App.Release, Chart: app, Namespace: e.App.Namespace, Set: append([]string{"api.image.tag=lint"}, e.AppValues(sampleCABundle)...)}
		if err := add("docfind-"+e.Name, false, false, r); err != nil {
			return err
		}
		appRenders = append(appRenders, "docfind-"+e.Name)
	}

	for _, pc := range []struct{ pin, namespace, values string }{
		{pins.CoreDNS, "kube-system", "deploy/platform/coredns/values.yaml"},
		{pins.CertManager, "cert-manager", "deploy/platform/cert-manager/values.yaml"},
		{pins.EnvoyGateway, "envoy-gateway-system", "deploy/platform/envoy-gateway/values.yaml"},
		{pins.OpenBao, "openbao", "deploy/platform/openbao/values.yaml"},
		{pins.ExternalSecrets, "external-secrets", "deploy/platform/external-secrets/values.yaml"},
	} {
		chart, err := c.Pins.Chart(pc.pin)
		if err != nil {
			return err
		}
		archive, err := c.Charts.Fetch(ctx, chart)
		if err != nil {
			return err
		}
		// CRD Envoy Gateway (w tym Gateway API) — źródło schematów poniżej.
		r := cluster.Release{Name: pc.pin, Chart: archive, Namespace: pc.namespace, ValuesFiles: []string{c.path(pc.values)}}
		if err := add(pc.pin, true, pc.pin == pins.EnvoyGateway || pc.pin == pins.ExternalSecrets, r); err != nil {
			return err
		}
	}

	cli.Step("helm lint platform")
	if err := c.Helm.Lint(ctx, platform); err != nil {
		return err
	}
	if err := c.mustReject(ctx, "chart platformy przyjął nieznany klucz gateway.hostnme — values.schema.json niczego nie pilnuje",
		cluster.Release{Name: "platform", Chart: platform, Set: []string{"gateway.hostnme=x"}}); err != nil {
		return err
	}
	// Środowiska z Let's Encrypt (staging, prod) renderują szablony wydawców
	// ACME, środowiska na własnym CA — bez nich; oba warianty są sprawdzane.
	// Adres konta ACME nie stoi w definicjach, więc w renderze jest
	// przykładowy.
	for _, e := range envs {
		r := cluster.Release{Name: "platform", Chart: platform, Namespace: e.Gateway.Namespace, Set: e.PlatformValues("ci@example.com")}
		if err := add("platform-"+e.Name, false, false, r); err != nil {
			return err
		}
	}

	// Schematy zasobów z CRD — Gateway API, Envoy Gateway, cert-manager,
	// External Secrets — z tych samych CRD, które instalują przypięte charty
	// (decyzja 28).
	schemaDir := filepath.Join(tmp, "crd-schemas")
	var crds []map[string]any
	for _, name := range []string{pins.EnvoyGateway, pins.CertManager, pins.ExternalSecrets} {
		docs, err := parseFile(filepath.Join(tmp, name+".yaml"))
		if err != nil {
			return err
		}
		crds = append(crds, docs...)
	}
	n, err := crdschema.Write(crds, schemaDir)
	if err != nil {
		return err
	}
	cli.Step("%d schematów CRD z przypiętych chartów", n)

	for _, r := range renders {
		cli.Step("kubeconform %s względem Kubernetesa %s", r.name, c.Pins.Get(pins.KubernetesVersion))
		if err := c.Validator.Validate(ctx, r.path, schemaDir); err != nil {
			return err
		}
		cli.Step("polityki %s", r.name)
		docs, err := parseFile(r.path)
		if err != nil {
			return err
		}
		// Nasz chart: sekrety tylko tak, żeby rotacja docierała (decyzja 44).
		opts := policy.Options{RequireDigest: r.requireDigest, RotatableSecrets: slices.Contains(appRenders, r.name)}
		if violations := policy.Check(docs, opts); len(violations) > 0 {
			for _, v := range violations {
				fmt.Fprintf(cli.Err, "POLITYKA (%s): %s\n", r.name, v)
			}
			return cli.Unmet("render %s łamie polityki", r.name)
		}
	}

	// Konfiguracja z values.yaml trafia do ConfigMapy i jest walidowana
	// dopiero przy starcie poda. Sprawdzamy ją tym samym modelem już tutaj,
	// w renderze każdego środowiska — zła konfiguracja ma zatrzymać pipeline,
	// a nie skończyć jako CrashLoopBackOff na klastrze.
	for _, name := range appRenders {
		if err := c.checkAppConfig(ctx, name, tmp); err != nil {
			return err
		}
	}
	return nil
}

func (c Checker) path(rel string) string { return filepath.Join(c.Root, rel) }

// sampleCABundle — przykładowy CA OpenBao w renderze (base64 nagłówka PEM):
// prawdziwy jest z klastra, a render ma przejść tę samą ścieżkę szablonu.
const sampleCABundle = "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t"

// apiContainer zwraca obraz i DOCFIND_VERSION kontenera api z renderu
// charta aplikacji.
func apiContainer(rendered string) (image, version string, err error) {
	docs, err := manifest.Parse(strings.NewReader(rendered))
	if err != nil {
		return "", "", err
	}
	for _, d := range docs {
		if manifest.String(d, "kind") != "Deployment" {
			continue
		}
		pod := manifest.Map(manifest.Map(manifest.Map(d, "spec"), "template"), "spec")
		for _, c := range manifest.List(pod, "containers") {
			container, _ := c.(map[string]any)
			if manifest.String(container, "name") != "api" {
				continue
			}
			image = manifest.String(container, "image")
			for _, e := range manifest.List(container, "env") {
				if env, _ := e.(map[string]any); manifest.String(env, "name") == "DOCFIND_VERSION" {
					version = manifest.String(env, "value")
				}
			}
		}
	}
	return image, version, nil
}

// mustReject renderuje chart z wartościami, które schemat ma odrzucić.
func (c Checker) mustReject(ctx context.Context, msg string, r cluster.Release) error {
	err := c.Helm.Template(ctx, r, false, io.Discard)
	switch cli.ExitCode(err) {
	case cli.ExitOK:
		return cli.Unmet("%s", msg)
	case cli.ExitUnmet:
		return nil
	default:
		return err
	}
}

func (c Checker) renderTo(ctx context.Context, path string, r cluster.Release, includeCRDs bool) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	return c.Helm.Template(ctx, r, includeCRDs, f)
}

// checkAppConfig wyciąga app.yml z ConfigMapy renderu i sprawdza go modelem
// aplikacji.
func (c Checker) checkAppConfig(ctx context.Context, name, tmp string) error {
	cli.Step("konfiguracja z renderu %s przechodzi walidację modelu", name)
	docs, err := parseFile(filepath.Join(tmp, name+".yaml"))
	if err != nil {
		return err
	}
	var config string
	for _, d := range docs {
		if manifest.String(d, "kind") != "ConfigMap" {
			continue
		}
		if v, ok := manifest.Map(d, "data")["app.yml"].(string); ok {
			config = v
		}
	}
	if config == "" {
		return cli.Unmet("render charta nie ma ConfigMapy z app.yml")
	}
	path := filepath.Join(tmp, name+".app.yml")
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		return err
	}
	if err := c.AppConfig.CheckConfig(ctx, path); err != nil {
		if cli.ExitCode(err) == cli.ExitUnmet {
			return cli.Unmet("konfiguracja w renderze %s jest nieprawidłowa — wyżej pole i powód", name)
		}
		return err
	}
	return nil
}

func parseFile(path string) ([]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	docs, err := manifest.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return docs, nil
}
