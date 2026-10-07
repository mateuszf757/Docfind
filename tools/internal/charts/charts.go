// Package charts sprawdza charty Helma tak, jak trafiłyby na klaster: lint,
// testy odrzucenia przez values.schema.json, render każdej kombinacji
// chart × środowisko do pliku, a na nim kubeconform ze schematami
// z przypiętych źródeł, polityki i konfigurację aplikacji jej modelem
// (dawniej część ci/run-tests.sh).
package charts

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/crdschema"
	"github.com/mateuszf757/Docfind/tools/internal/env"
	"github.com/mateuszf757/Docfind/tools/internal/fetch"
	"github.com/mateuszf757/Docfind/tools/internal/manifest"
	"github.com/mateuszf757/Docfind/tools/internal/pins"
	"github.com/mateuszf757/Docfind/tools/internal/policy"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// Checker — sprawdzenia chartów w repozytorium o korzeniu Root.
type Checker struct {
	Root   string
	Runner proc.Runner
	Pins   pins.Pins
	Out    io.Writer
	Err    io.Writer
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
	app := filepath.Join(c.Root, "deploy", "charts", "docfind")
	platform := filepath.Join(c.Root, "deploy", "charts", "platform")

	cli.Step("helm lint docfind")
	if err := c.stream(ctx, "helm", "lint", app, "--set", "api.image.tag=lint"); err != nil {
		return err
	}
	// values.schema.json ma odrzucić literówkę — bez schematu Helm po cichu
	// ignoruje nieznany klucz. Sprawdzamy odrzucenie, a nie samo istnienie
	// pliku: schemat, który wszystko przepuszcza, też by „istniał".
	if err := c.mustReject(ctx, "chart przyjął nieznany klucz api.replica — values.schema.json niczego nie pilnuje",
		"template", "docfind", app, "--set", "api.image.tag=lint", "--set", "api.replica=3"); err != nil {
		return err
	}
	// Digest skrócony albo z literówką ma odpaść na schemacie, a nie przy
	// pobieraniu obrazu na węźle; poprawny ma trafić do referencji obrazu.
	if err := c.mustReject(ctx, "chart przyjął skrócony digest api.image.digest — values.schema.json go nie pilnuje",
		"template", "docfind", app, "--set", "api.image.tag=lint", "--set", "api.image.digest=sha256:abc"); err != nil {
		return err
	}
	digest := "sha256:" + strings.Repeat("0", 64)
	out, err := c.output(ctx, "helm", "template", "docfind", app, "--set", "api.image.tag=lint", "--set", "api.image.digest="+digest)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "docfind-api:lint@"+digest+`"`) {
		return cli.Unmet("api.image.digest nie trafił do referencji obrazu w Deploymencie")
	}

	var renders []render
	add := func(name string, requireDigest bool, args ...string) error {
		path := filepath.Join(tmp, name+".yaml")
		if err := c.renderTo(ctx, name, path, args...); err != nil {
			return err
		}
		renders = append(renders, render{name: name, path: path, requireDigest: requireDigest})
		return nil
	}
	// Chart aplikacji i chart platformy w wariancie każdego środowiska,
	// z wartościami z definicji (deploy/environments) — tą samą funkcją,
	// którą wdrożenie przekazuje Helmowi. Definicje z gita, bez nadpisań
	// z mise.local.toml: lokalnie sprawdzane jest to samo co w CI.
	envs, err := env.All(c.Root)
	if err != nil {
		return err
	}
	var appRenders []string
	for _, e := range envs {
		args := []string{"template", e.App.Release, app, "--namespace", e.App.Namespace, "--set", "api.image.tag=lint"}
		for _, v := range e.AppValues() {
			args = append(args, "--set", v)
		}
		if err := add("docfind-"+e.Name, false, args...); err != nil {
			return err
		}
		appRenders = append(appRenders, "docfind-"+e.Name)
	}

	p := c.Pins
	coredns, err := fetch.URL(ctx, c.Root, p.Get("DF_COREDNS_CHART_URL"), p.Get("DF_COREDNS_CHART_SHA256"))
	if err != nil {
		return err
	}
	if err := add("coredns", true, "template", "coredns", coredns, "--namespace", "kube-system", "--values", c.path("deploy/platform/coredns/values.yaml")); err != nil {
		return err
	}
	certManager, err := fetch.URL(ctx, c.Root, p.Get("DF_CERT_MANAGER_CHART_URL"), p.Get("DF_CERT_MANAGER_CHART_SHA256"))
	if err != nil {
		return err
	}
	if err := add("cert-manager", true, "template", "cert-manager", certManager, "--namespace", "cert-manager", "--values", c.path("deploy/platform/cert-manager/values.yaml")); err != nil {
		return err
	}
	envoy, err := fetch.OCIChart(ctx, c.Runner, c.Root, p.Get("DF_ENVOY_GATEWAY_CHART_REF"), p.Get("DF_ENVOY_GATEWAY_CHART_VERSION"), p.Get("DF_ENVOY_GATEWAY_CHART_SHA256"))
	if err != nil {
		return err
	}
	if err := add("envoy-gateway", true, "template", "envoy-gateway", envoy, "--namespace", "envoy-gateway-system", "--values", c.path("deploy/platform/envoy-gateway/values.yaml"), "--include-crds"); err != nil {
		return err
	}

	cli.Step("helm lint platform")
	if err := c.stream(ctx, "helm", "lint", platform); err != nil {
		return err
	}
	if err := c.mustReject(ctx, "chart platformy przyjął nieznany klucz gateway.hostnme — values.schema.json niczego nie pilnuje",
		"template", "platform", platform, "--set", "gateway.hostnme=x"); err != nil {
		return err
	}
	// Środowiska z Let's Encrypt (staging, prod) renderują szablony wydawców
	// ACME, środowiska na własnym CA — bez nich; oba warianty są sprawdzane.
	// Adres konta ACME nie stoi w definicjach, więc w renderze jest
	// przykładowy.
	for _, e := range envs {
		args := []string{"template", "platform", platform, "--namespace", e.Gateway.Namespace}
		for _, v := range e.PlatformValues("ci@example.com") {
			args = append(args, "--set", v)
		}
		if err := add("platform-"+e.Name, false, args...); err != nil {
			return err
		}
	}

	// Schematy zasobów z CRD — Gateway API, Envoy Gateway, cert-manager —
	// z tych samych CRD, które instalują przypięte charty (decyzja 28).
	schemaDir := filepath.Join(tmp, "crd-schemas")
	var crds []map[string]any
	for _, name := range []string{"envoy-gateway", "cert-manager"} {
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
		if err := c.kubeconform(ctx, r, schemaDir); err != nil {
			return err
		}
		cli.Step("polityki %s", r.name)
		docs, err := parseFile(r.path)
		if err != nil {
			return err
		}
		if violations := policy.Check(docs, policy.Options{RequireDigest: r.requireDigest}); len(violations) > 0 {
			for _, v := range violations {
				fmt.Fprintf(c.Err, "POLITYKA (%s): %s\n", r.name, v)
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

func (c Checker) stream(ctx context.Context, name string, args ...string) error {
	_, err := c.Runner.Run(ctx, proc.Cmd{Name: name, Args: args, Stdout: c.Out, Stderr: c.Err})
	if code := proc.ExitCode(err); code > 0 {
		return cli.Unmet("%s %s: kod %d", name, args[0], code)
	}
	return err
}

func (c Checker) output(ctx context.Context, name string, args ...string) (string, error) {
	res, err := c.Runner.Run(ctx, proc.Cmd{Name: name, Args: args})
	if code := proc.ExitCode(err); code > 0 {
		return "", cli.Unmet("%s %s: %v", name, args[0], err)
	}
	return string(res.Stdout), err
}

// mustReject uruchamia helm z wartościami, które schemat ma odrzucić.
func (c Checker) mustReject(ctx context.Context, msg string, args ...string) error {
	_, err := c.Runner.Run(ctx, proc.Cmd{Name: "helm", Args: args})
	switch {
	case err == nil:
		return cli.Unmet("%s", msg)
	case proc.ExitCode(err) > 0:
		return nil
	default:
		return err
	}
}

func (c Checker) renderTo(ctx context.Context, name, path string, args ...string) (err error) {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	var stderr bytes.Buffer
	_, err = c.Runner.Run(ctx, proc.Cmd{Name: "helm", Args: args, Stdout: f, Stderr: &stderr})
	if code := proc.ExitCode(err); code > 0 {
		fmt.Fprint(c.Err, stderr.String())
		return cli.Unmet("render %s (helm %s): kod %d — wyżej powód", name, args[0], code)
	}
	return err
}

func (c Checker) kubeconform(ctx context.Context, r render, schemaDir string) error {
	version := c.Pins.Get("DF_KUBERNETES_VERSION")
	cli.Step("kubeconform %s względem Kubernetesa %s", r.name, version)
	// Schematy z commita przypiętego w ci/pins.env zamiast domyślnej gałęzi
	// master. {{ … }} rozwija kubeconform.
	schemas := "https://raw.githubusercontent.com/yannh/kubernetes-json-schema/" + c.Pins.Get("DF_KUBECONFORM_SCHEMA_COMMIT") +
		"/{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json"
	crdSchemas := schemaDir + "/{{ .Group }}/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json"
	cache := c.path(".cache/kubeconform")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	// -strict odrzuca pola, których nie ma w schemacie: literówka w nazwie
	// pola manifestu jest inaczej po cichu ignorowana przez API server.
	// Nigdy -ignore-missing-schemas: przy CRD wyłączyłoby walidację bez
	// słowa. -skip CustomResourceDefinition: repozytorium schematów yannh nie
	// ma schematu dla samego rodzaju CRD; pominięty jest dokładnie ten jeden
	// rodzaj, a definicje CRD przychodzą wyłącznie z przypiętych chartów.
	return c.stream(ctx, "kubeconform", "-strict", "-summary", "-kubernetes-version", version,
		"-schema-location", schemas, "-schema-location", crdSchemas,
		"-skip", "CustomResourceDefinition", "-cache", cache, r.path)
}

// checkAppConfig wyciąga app.yml z ConfigMapy renderu i sprawdza go modelem
// aplikacji (python -m docfind_api.configtool check). PYTHONPATH=src, bo
// projekt nie jest instalowany do środowiska — jak w obrazie.
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
	_, err = c.Runner.Run(ctx, proc.Cmd{
		Name: "uv", Args: []string{"run", "--frozen", "python", "-m", "docfind_api.configtool", "check", path},
		Dir: c.path("services/api"), Env: []string{"PYTHONPATH=src"}, Stdout: c.Out, Stderr: c.Err,
	})
	if code := proc.ExitCode(err); code == 1 {
		return cli.Unmet("konfiguracja w renderze %s jest nieprawidłowa — wyżej pole i powód", name)
	} else if code > 1 {
		return fmt.Errorf("walidacja konfiguracji nie wykonała się: %v", err)
	}
	return err
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
