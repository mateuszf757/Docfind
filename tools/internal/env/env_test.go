package env

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

const repoRoot = "../../.."

// clearOverrides — zmienne z mise.local.toml autora nadpisałyby wartości
// w definicjach, które dopuszczają nadpisania (dev).
func clearOverrides(t *testing.T) {
	t.Helper()
	for _, key := range []string{"DOCFIND_HOSTNAME", "DOCFIND_TLS_ISSUER", "DOCFIND_ACME_EMAIL", "DOCFIND_ACME_ZONE"} {
		t.Setenv(key, "")
	}
}

func byName(t *testing.T, all []Environment) map[string]Environment {
	t.Helper()
	m := map[string]Environment{}
	for _, e := range all {
		m[e.Name] = e
	}
	return m
}

// TestRepositoryEnvironments: prawdziwe definicje przechodzą walidację
// i reguły między środowiskami, a wymagania, których nie da się wyrazić
// regułą ogólną, są sprawdzone wprost.
func TestRepositoryEnvironments(t *testing.T) {
	all, err := All(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	envs := byName(t, all)
	var names []string
	for name := range envs {
		names = append(names, name)
	}
	slices.Sort(names)
	if got := strings.Join(names, ","); got != "ci,dev,prod,staging" {
		t.Fatalf("środowiska: %s, oczekiwano ci,dev,prod,staging", got)
	}

	// Bramka na PR-ze nie ma sekretów (PR-y Dependabota ich nie widzą), więc
	// CI nie może zależeć od tokenu DNS.
	ci := envs["ci"]
	if ci.Gateway.Issuer != "docfind-internal-ca" || ci.Allows(OpDNSToken) {
		t.Errorf("ci: wydawca %s, dns-token %v — CI działa bez sekretów", ci.Gateway.Issuer, ci.Allows(OpDNSToken))
	}
	if ci.AllowsSource(SourceLocal) {
		t.Error("ci: obraz budowany od nowa zamiast artefaktu z zadania build")
	}
	// Staging z topologią i wydawcą produkcji, ale bez usuwania klastra.
	staging := envs["staging"]
	if staging.Allows(OpClusterDelete) || !staging.Allows(OpDrain) || !slices.Equal(staging.Artifact.Sources, []string{SourceRegistry}) {
		t.Errorf("staging: operacje %v, źródła %v", staging.Operations.Allowed, staging.Artifact.Sources)
	}
	prod := envs["prod"]
	if len(prod.Operations.Allowed) != 0 || !prod.Artifact.ReleasesOnly || prod.Gateway.Issuer != "letsencrypt" {
		t.Errorf("prod: operacje %v, releasesOnly %v, wydawca %s", prod.Operations.Allowed, prod.Artifact.ReleasesOnly, prod.Gateway.Issuer)
	}
}

// TestMiseEnvironmentFiles: mise.<środowisko>.toml (MISE_ENV) ustawia
// DOCFIND_ENV i KUBECONFIG zgodnie z definicją — kubectl z `mise exec` i dft
// trafiają w ten sam klaster. Dev to sam mise.toml.
func TestMiseEnvironmentFiles(t *testing.T) {
	all, err := All(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	envs := byName(t, all)
	kubeconfigLine := func(e Environment) string {
		return `KUBECONFIG = "{{config_root}}/` + e.Cluster.Kubeconfig + `"`
	}
	base, err := os.ReadFile(filepath.Join(repoRoot, "mise.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(base), "\n"+kubeconfigLine(envs[DefaultName])+"\n") {
		t.Errorf("mise.toml: brak %q (kubeconfig środowiska %s)", kubeconfigLine(envs[DefaultName]), DefaultName)
	}

	files, err := filepath.Glob(filepath.Join(repoRoot, "mise.*.toml"))
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "mise."), ".toml")
		if name == "local" || strings.HasSuffix(name, ".local") {
			continue // ustawienia osobiste, poza gitem
		}
		e, ok := envs[name]
		if !ok {
			t.Errorf("%s: nie ma definicji %s/%s.yaml", filepath.Base(f), Dir, name)
			continue
		}
		covered[name] = true
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range []string{`DOCFIND_ENV = "` + name + `"`, kubeconfigLine(e)} {
			if !strings.Contains(string(raw), "\n"+line+"\n") {
				t.Errorf("%s: brak %q", filepath.Base(f), line)
			}
		}
	}
	for _, e := range all {
		if e.Name != DefaultName && e.Cluster.Provider != ProviderNone && !covered[e.Name] {
			t.Errorf("środowisko %s ma klaster, a nie ma mise.%s.toml", e.Name, e.Name)
		}
	}
}

func TestOverrides(t *testing.T) {
	t.Setenv("DOCFIND_HOSTNAME", "local.example.com")
	t.Setenv("DOCFIND_TLS_ISSUER", "letsencrypt-staging")
	t.Setenv("DOCFIND_ACME_ZONE", "example.com")
	t.Setenv("DOCFIND_ACME_EMAIL", "autor@example.com")
	dev, err := Load(repoRoot, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if g := dev.Gateway; g.Hostname != "local.example.com" || g.Issuer != "letsencrypt-staging" || g.ACMEZone != "example.com" || g.ACMEEmail != "autor@example.com" {
		t.Errorf("dev bez nadpisań z mise.local.toml: %+v", g)
	}
	staging, err := Load(repoRoot, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if g := staging.Gateway; g.Hostname != "staging.docfind.lol" || g.ACMEZone != "docfind.lol" {
		t.Errorf("staging nadpisany zmiennymi środowiskowymi: %+v", g)
	}
	// Adres konta ACME nie stoi w żadnej definicji — przychodzi zawsze ze
	// zmiennej, także w stagingu.
	if staging.Gateway.ACMEEmail != "autor@example.com" {
		t.Errorf("staging: ACMEEmail = %q", staging.Gateway.ACMEEmail)
	}
}

func TestOverrideIsValidatedAgain(t *testing.T) {
	clearOverrides(t)
	t.Setenv("DOCFIND_TLS_ISSUER", "selfsigned")
	if _, err := Load(repoRoot, "dev"); err == nil || !strings.Contains(err.Error(), "po nadpisaniach") {
		t.Errorf("nadpisanie wydawcy spoza listy przyjęte: %v", err)
	}
}

func write(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, Dir, name+".yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func definition(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, Dir, name+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReadRejects(t *testing.T) {
	clearOverrides(t)
	tests := []struct{ base, name, from, to, want string }{
		{"dev", "literówka w polu", "  release: docfind", "  relase: docfind", "unknown field"},
		{"dev", "port uprzywilejowany", "    https: 8443", "    https: 443", "cluster.ports.https=443"},
		{"dev", "serwer API inny niż port", "apiServer: https://127.0.0.1:6550", "apiServer: https://127.0.0.1:7000", "cluster.apiServer"},
		{"dev", "serwer API poza loopbackiem", "apiServer: https://127.0.0.1:6550", "apiServer: https://0.0.0.0:6550", "cluster.apiServer"},
		{"dev", "kubeconfig poza repozytorium", "kubeconfig: .cache/kubeconfig", "kubeconfig: ../kubeconfig", "cluster.kubeconfig"},
		{"dev", "nieznany wydawca", "  issuer: docfind-internal-ca", "  issuer: selfsigned", "gateway.issuer"},
		{"dev", "nieznana operacja", "allowed: [cluster-create,", "allowed: [chaos, cluster-create,", `"chaos"`},
		{"dev", "operacja dwa razy", "allowed: [cluster-create,", "allowed: [drain, cluster-create,", "drain dwa razy"},
		{"dev", "nieznane źródło obrazu", "sources: [local, archive, registry]", "sources: [local, s3]", `"s3"`},
		{"dev", "nadpisania poza wytwarzaniem", "stage: wytwarzanie", "stage: akceptacja", "overridesFromEnvironment tylko"},
		{"ci", "archiwum na klastrze zewnętrznym", "  provider: k3d\n  name: docfind-ci", "  provider: external", "archive wymaga klastra k3d"},
		{"ci", "e-mail ACME w definicji", "  issuer: docfind-internal-ca", "  issuer: docfind-internal-ca\n  acmeEmail: ktos@example.com", "unknown field"},
		{"staging", "Let's Encrypt bez strefy", "  acmeZone: docfind.lol\n", "", "wymaga gateway.acmeZone"},
		{"staging", "host spoza strefy", "hostname: staging.docfind.lol", "hostname: staging.example.com", "spoza strefy"},
		{"prod", "operacja bez klastra", "allowed: []", "allowed: [deploy]", "środowisko bez klastra nie ma operacji"},
		{"prod", "klaster bez dostawcy z nazwą", "  provider: none", "  provider: none\n  name: prod", "cluster.provider=none"},
		{"prod", "wydanie spoza rejestru", "  sources: [registry]", "  sources: [local, registry]", "releasesOnly"},
		{"prod", "wydanie bez atestacji", "  attested: true\n", "", "releasesOnly bez artifact.attested"},
		{"ci", "atestacja bez rejestru", "  sources: [archive, registry]", "  sources: [archive]", "atestacje mają tylko obrazy z rejestru"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := definition(t, tt.base)
			content := strings.Replace(base, tt.from, tt.to, 1)
			if content == base {
				t.Fatalf("podmiana %q nie zadziałała", tt.from)
			}
			_, err := Read(write(t, map[string]string{tt.base: content}), tt.base)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("oczekiwano błędu z %q, jest %v", tt.want, err)
			}
		})
	}
}

// Reguły dla klastra zewnętrznego na etapie wydania — z definicji prod,
// w której klaster none zastąpiono zewnętrznym (Etap 10).
func TestReadRejectsExternalCluster(t *testing.T) {
	clearOverrides(t)
	external := "cluster:\n  provider: external\n  context: klient\n  kubeconfig: .cache/kubeconfig-prod\n  apiServer: https://10.0.0.1:6443"
	tests := []struct {
		name  string
		edits []string
		want  string
	}{
		{"drain na etapie wydania", []string{"allowed: []", "allowed: [drain]"}, "drain na etapie wydania"},
		{"usuwanie klastra zewnętrznego", []string{"allowed: []", "allowed: [cluster-delete]"}, "tworzenie i usuwanie tylko dla klastrów k3d"},
		{"wymuszone odnowienie z produkcyjnym LE", []string{"stage: wydanie", "stage: akceptacja", "allowed: []", "allowed: [tls-renew]"}, "tls-renew z wydawcą letsencrypt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := strings.Replace(definition(t, "prod"), "cluster:\n  provider: none", external, 1)
			if _, err := Read(write(t, map[string]string{"prod": content}), "prod"); err != nil {
				t.Fatalf("punkt wyjścia odrzucony: %v", err)
			}
			content = strings.NewReplacer(tt.edits...).Replace(content)
			_, err := Read(write(t, map[string]string{"prod": content}), "prod")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("oczekiwano błędu z %q, jest %v", tt.want, err)
			}
		})
	}
}

func TestAllRejectsCollisions(t *testing.T) {
	clearOverrides(t)
	staging := strings.NewReplacer("api: 6552", "api: 6550", "127.0.0.1:6552", "127.0.0.1:6550").Replace(definition(t, "staging"))
	_, err := All(write(t, map[string]string{"dev": definition(t, "dev"), "staging": staging}))
	if err == nil || !strings.Contains(err.Error(), "dev i staging mają ten sam port=6550") {
		t.Errorf("kolizja portów przyjęta: %v", err)
	}
	ci := strings.Replace(definition(t, "ci"), "kubeconfig: .cache/kubeconfig-ci", "kubeconfig: .cache/kubeconfig", 1)
	_, err = All(write(t, map[string]string{"dev": definition(t, "dev"), "ci": ci}))
	if err == nil || !strings.Contains(err.Error(), "cluster.kubeconfig=.cache/kubeconfig") {
		t.Errorf("wspólny kubeconfig przyjęty: %v", err)
	}
}

func TestRequire(t *testing.T) {
	clearOverrides(t)
	prod, err := Read(repoRoot, "prod")
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range operations {
		if err := prod.Require(op); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), string(op)) {
			t.Errorf("prod.Require(%s) = %v, oczekiwano odmowy (kod 1)", op, err)
		}
	}
	if err := prod.RequireCluster(); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("prod.RequireCluster() = %v", err)
	}
	dev, err := Read(repoRoot, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := dev.Require(OpDrain); err != nil {
		t.Errorf("dev.Require(drain) = %v", err)
	}
}

func TestChartValues(t *testing.T) {
	clearOverrides(t)
	dev, err := Read(repoRoot, "dev")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(dev.PlatformValues("x@example.com"), " ")
	if !strings.Contains(got, "gateway.httpsRedirectPort=8443") || strings.Contains(got, "acme.") {
		t.Errorf("dev (własne CA, k3d): %s", got)
	}
	prod, err := Read(repoRoot, "prod")
	if err != nil {
		t.Fatal(err)
	}
	got = strings.Join(prod.PlatformValues("x@example.com"), " ")
	for _, want := range []string{"gateway.issuer=letsencrypt", "gateway.httpsRedirectPort=443", "acme.email=x@example.com", "acme.dnsZone=docfind.lol"} {
		if !strings.Contains(got, want) {
			t.Errorf("prod: brak %s w %s", want, got)
		}
	}
	if got := strings.Join(prod.AppValues(), " "); !strings.Contains(got, "route.hostname=prod.docfind.lol") {
		t.Errorf("prod: %s", got)
	}
}

func TestUnknownEnvironment(t *testing.T) {
	_, err := Read(repoRoot, "qa")
	if cli.ExitCode(err) != cli.ExitFailure || !strings.Contains(err.Error(), "zdefiniowane: ci, dev, prod, staging") {
		t.Errorf("nieznane środowisko: %v", err)
	}
}
