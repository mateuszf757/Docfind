package charts

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

const repoRoot = "../../../.."

const crd = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: gateways.gateway.networking.k8s.io}
spec:
  group: gateway.networking.k8s.io
  names: {kind: Gateway}
  versions:
  - name: v1
    schema:
      openAPIV3Schema: {type: object, properties: {spec: {type: object}}}
`

// deployment — Deployment zgodny z politykami; image — obraz kontenera.
func deployment(image, cpuLimit string) string {
	limits := "{memory: 64Mi}"
	if cpuLimit != "" {
		limits = "{memory: 64Mi, cpu: " + cpuLimit + "}"
	}
	return `apiVersion: apps/v1
kind: Deployment
metadata: {name: x}
spec:
  template:
    spec:
      containers:
      - name: c
        image: ` + image + `
        resources: {requests: {cpu: 10m, memory: 16Mi}, limits: ` + limits + `}
`
}

// app — Deployment aplikacji z kontenerem api i wersją wdrożenia.
func app(image, version string) string {
	return `apiVersion: apps/v1
kind: Deployment
metadata: {name: docfind-api}
spec:
  template:
    spec:
      containers:
      - name: api
        image: ` + image + `
        env: [{name: DOCFIND_VERSION, value: "` + version + `"}]
        resources: {requests: {cpu: 10m, memory: 16Mi}, limits: {memory: 64Mi}}
`
}

// helm — Helm z wartościami ocenianymi jak przez values.schema.json.
type helm struct {
	// rejectNothing — schemat, który wszystko przepuszcza.
	rejectNothing bool
	// cpuLimit — chart platformy z domyślnym limitem CPU (jak CoreDNS).
	cpuLimit string
	// badConfig — app.yml w ConfigMapie, który model odrzuci.
	badConfig bool
	// noVersionPattern — schemat bez wzorca api.version.
	noVersionPattern bool
	// dropVersion — chart, który gubi api.version.
	dropVersion bool
	// projectWithout — rodzaj zasobu, którego projekt Argo CD nie dopuszcza.
	projectWithout string
	renders        []string
}

func (h *helm) Lint(context.Context, string, ...string) error { return nil }

func (h *helm) Template(_ context.Context, r cluster.Release, includeCRDs bool, w io.Writer) error {
	h.renders = append(h.renders, r.Name)
	values := append(slices.Clone(r.Set), r.SetString...)
	for _, v := range values {
		badVersion := v == "api.version=v0.4.0" && !h.noVersionPattern
		if !h.rejectNothing && (v == "api.replica=3" || v == "api.image.digest=sha256:abc" || badVersion || v == "gateway.hostnme=x" || v == "application.version=1.0.0") {
			return cli.Unmet("helm template %s: values don't meet the specifications of the schema", r.Name)
		}
	}
	var out string
	switch {
	case strings.HasSuffix(r.Chart, "docfind"), strings.HasPrefix(filepath.Base(r.Chart), "docfind-") && strings.HasSuffix(r.Chart, ".tgz"):
		tag, digest, version := "lint", "", ""
		// Spakowany chart (decyzja 45) niesie obraz po digeście w wartościach.
		if strings.HasSuffix(r.Chart, ".tgz") {
			digest = "@sha256:" + strings.Repeat("a", 64)
		}
		for _, v := range values {
			switch key, value, _ := strings.Cut(v, "="); key {
			case "api.image.tag":
				tag = value
			case "api.image.digest":
				digest = "@" + value
			case "api.version":
				version = value
			}
		}
		if h.dropVersion {
			version = ""
		}
		config := "service: {port: 8000}"
		if h.badConfig {
			config = "service: {port: 0}"
		}
		out = app(`"ghcr.io/x/docfind-api:`+tag+digest+`"`, version) + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\ndata:\n  app.yml: \"" + config + "\"\n"
	case strings.HasSuffix(r.Chart, "platform"):
		out = "apiVersion: v1\nkind: Namespace\nmetadata: {name: n}\n"
	case strings.HasSuffix(r.Chart, "delivery"):
		// Projekt dopuszcza to, co renderuje atrapa charta aplikacji.
		var kinds []string
		for _, gk := range []string{`{group: apps, kind: Deployment}`, `{group: "", kind: ConfigMap}`} {
			if !strings.Contains(gk, "kind: "+h.projectWithout+"}") || h.projectWithout == "" {
				kinds = append(kinds, gk)
			}
		}
		out = "apiVersion: argoproj.io/v1alpha1\nkind: AppProject\nmetadata: {name: docfind}\nspec:\n  namespaceResourceWhitelist: [" + strings.Join(kinds, ", ") + "]\n"
	default:
		out = deployment("example.com/"+r.Name+"@sha256:"+strings.Repeat("a", 64), h.cpuLimit)
		if includeCRDs || r.Name == pins.CertManager {
			out += "---\n" + crd
		}
	}
	_, err := io.WriteString(w, out)
	return err
}

type source struct{}

func (source) Fetch(_ context.Context, c pins.Chart) (string, error) {
	return "/cache/" + c.Name + ".tgz", nil
}

type validator struct{ validated []string }

func (v *validator) Validate(_ context.Context, manifests, crdSchemas string) error {
	v.validated = append(v.validated, filepath.Base(manifests))
	return nil
}

type appConfig struct{}

func (appConfig) CheckConfig(_ context.Context, path string) error {
	return nil
}

type rejectingConfig struct{}

func (rejectingConfig) CheckConfig(context.Context, string) error {
	return cli.Unmet("service.port: wartość spoza zakresu")
}

func checker(t *testing.T, h *helm, v *validator, cfg AppConfig) Checker {
	t.Helper()
	p, err := pins.Load(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	return Checker{Root: repoRoot, Pins: p, Helm: h, Charts: source{}, Validator: v, AppConfig: cfg}
}

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

// TestRun: każde środowisko renderowane w obu chartach, platforma
// z przypiętych chartów, a każdy render przechodzi walidację schematem.
func TestRun(t *testing.T) {
	quiet(t)
	h, v := &helm{}, &validator{}
	if err := checker(t, h, v, appConfig{}).Run(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"docfind-dev.yaml", "docfind-prod.yaml", "platform-staging.yaml", "coredns.yaml", "envoy-gateway.yaml", "cert-manager.yaml",
		"argocd.yaml", "docfind-oci-staging.yaml", "delivery-staging.yaml"} {
		if !slices.Contains(v.validated, want) {
			t.Errorf("bez walidacji %s: %v", want, v.validated)
		}
	}
}

func TestRunRejects(t *testing.T) {
	quiet(t)
	tests := []struct {
		name string
		helm *helm
		cfg  AppConfig
		want string
	}{
		{"schemat wszystko przepuszcza", &helm{rejectNothing: true}, appConfig{}, "values.schema.json niczego nie pilnuje"},
		{"schemat bez wzorca wersji", &helm{noVersionPattern: true}, appConfig{}, "nie pilnuje wersji"},
		{"chart gubi wersję wdrożenia", &helm{dropVersion: true}, appConfig{}, "DOCFIND_VERSION"},
		{"limit CPU w charcie platformy", &helm{cpuLimit: "100m"}, appConfig{}, "render coredns łamie polityki"},
		{"zła konfiguracja w renderze", &helm{}, rejectingConfig{}, "konfiguracja w renderze docfind-ci jest nieprawidłowa"},
		// Nowy rodzaj zasobu w charcie aplikacji bez dopisania do projektu:
		// Argo odmówiłby synchronizacji dopiero na stagingu.
		{"projekt Argo CD bez rodzaju z charta", &helm{projectWithout: "ConfigMap"}, appConfig{}, "projekt Argo CD środowiska staging nie dopuszcza"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checker(t, tt.helm, &validator{}, tt.cfg).Run(context.Background(), t.TempDir())
			if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("oczekiwano niespełnionego z %q, jest %v", tt.want, err)
			}
		})
	}
}
