package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
	"github.com/mateuszf757/Docfind/tools/internal/stages/build"
	"github.com/mateuszf757/Docfind/tools/internal/stages/secrets"
)

const (
	repoRoot = "../../../.."
	apiImage = "ghcr.io/mateuszf757/docfind-api"
)

var commit = strings.Repeat("c", 40)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

func definition(t *testing.T, name string) environment.Definition {
	t.Helper()
	for _, key := range []string{"DOCFIND_HOSTNAME", "DOCFIND_TLS_ISSUER", "DOCFIND_ACME_EMAIL", "DOCFIND_ACME_ZONE"} {
		t.Setenv(key, "")
	}
	e, err := environment.Load(repoRoot, name)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestK3dConfig(t *testing.T) {
	e := environment.Definition{Cluster: environment.Cluster{Name: "docfind-ci", Ports: environment.Ports{API: 6551, HTTP: 8081, HTTPS: 8444}}}
	raw, err := K3dConfig(filepath.Join(repoRoot, "deploy", "k3d", "cluster.yaml"), e)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{`"name": "docfind-ci"`, `"hostPort": "6551"`, `"127.0.0.1:8081:80"`, `"127.0.0.1:8444:443"`, `"--disable=traefik"`} {
		if !strings.Contains(out, want) {
			t.Errorf("konfiguracja bez %s:\n%s", want, out)
		}
	}
}

// TestK3dConfigRejectsWildcard: mapowanie na 0.0.0.0 to klaster osiągalny
// z sieci — odrzucone, a nie przepisane po cichu (decyzja 17).
func TestK3dConfigRejectsWildcard(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "k3d", "cluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ from, to string }{
		{"port: 127.0.0.1:8080:80", "port: 0.0.0.0:8080:80"},
		{"  hostIP: 127.0.0.1", "  hostIP: 0.0.0.0"},
	} {
		path := filepath.Join(t.TempDir(), "cluster.yaml")
		bad := strings.Replace(string(src), mutation.from, mutation.to, 1)
		if bad == string(src) {
			t.Fatalf("podmiana %q nie zadziałała", mutation.from)
		}
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := K3dConfig(path, environment.Definition{Cluster: environment.Cluster{Name: "x", Ports: environment.Ports{API: 6550, HTTP: 8080, HTTPS: 8443}}})
		if cli.ExitCode(err) != cli.ExitUnmet {
			t.Errorf("%s: przyjęte (%v)", mutation.to, err)
		}
	}
}

func labelled(t *testing.T, labels map[string]string) v1.Image {
	t.Helper()
	img, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Config(img, v1.Config{Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// saved zapisuje obraz jak `docker save` i zwraca ścieżkę i digest konfiguracji.
func saved(t *testing.T, img v1.Image) (string, string) {
	t.Helper()
	tag, err := name.NewTag(apiImage + ":0.0.0-dev.1-abc1234")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "image.tar")
	if err := tarball.WriteToFile(path, tag, img); err != nil {
		t.Fatal(err)
	}
	config, err := img.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	return path, config.String()
}

func TestArchiveImage(t *testing.T) {
	quiet(t)
	d := Deployer{Env: definition(t, "ci"), Image: apiImage, Versions: versions{}}
	img := labelled(t, map[string]string{artifact.RevisionLabel: commit})
	path, config := saved(t, img)

	got, err := d.archiveImage(context.Background(), Source{Kind: environment.SourceArchive, Archive: path, Config: config}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wantRef := apiImage + ":local-" + strings.TrimPrefix(config, "sha256:")[:12]
	// Wersja wdrożenia z historii repozytorium, nie z obrazu (decyzja 40).
	if got.want.Image != wantRef || got.want.Version != "0.3.1-dev.4+ccccccc" || got.want.Commit != commit || got.digest != "" {
		t.Errorf("obraz z archiwum: %+v, oczekiwano %s", got, wantRef)
	}
	// Archiwum do importu niesie nowy tag, a obraz zostaje ten sam.
	loaded, err := tarball.ImageFromPath(got.load, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := loaded.ConfigName(); c.String() != config {
		t.Errorf("archiwum do importu: konfiguracja %s, oczekiwano %s", c, config)
	}

	t.Run("inna konfiguracja niż sprawdzona w zadaniu build", func(t *testing.T) {
		_, err := d.archiveImage(context.Background(), Source{Kind: environment.SourceArchive, Archive: path, Config: "sha256:" + strings.Repeat("0", 64)}, t.TempDir())
		if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "zadanie build sprawdziło") {
			t.Errorf("podmienione archiwum przyjęte: %v", err)
		}
	})
	t.Run("obraz bez etykiety commita", func(t *testing.T) {
		path, config := saved(t, labelled(t, nil))
		_, err := d.archiveImage(context.Background(), Source{Kind: environment.SourceArchive, Archive: path, Config: config}, t.TempDir())
		if cli.ExitCode(err) != cli.ExitUnmet {
			t.Errorf("obraz bez wersji przyjęty: %v", err)
		}
	})
}

// recorder zapisuje wywołania portów w kolejności — wspólny dla wszystkich
// atrap jednego biegu.
type recorder struct {
	calls []string
	// fail — wywołanie, które ma zawieść.
	fail string
	// version — api.version z instalacji charta aplikacji: to podaje
	// aplikacja na /version (DOCFIND_VERSION).
	version string
}

func (r *recorder) call(format string, args ...any) error {
	c := fmt.Sprintf(format, args...)
	r.calls = append(r.calls, c)
	if r.fail != "" && strings.HasPrefix(c, r.fail) {
		return errors.New("awaria " + c)
	}
	return nil
}

type fakeClusters struct {
	*recorder
	exists bool
}

func (f fakeClusters) Exists(context.Context, string) (bool, error) {
	return f.exists, f.call("k3d exists")
}
func (f fakeClusters) Create(_ context.Context, c cluster.LocalCluster) error {
	return f.call("k3d create %s userns=%v", c.Name, c.UserNamespace)
}
func (f fakeClusters) Delete(_ context.Context, name string) error {
	return f.call("k3d delete %s", name)
}
func (f fakeClusters) Kubeconfig(context.Context, string) ([]byte, error) {
	return []byte("kubeconfig"), f.call("k3d kubeconfig")
}
func (f fakeClusters) Import(_ context.Context, _, image string) error {
	return f.call("k3d import %s", image)
}

type fakeEngine struct{ *recorder }

func (f fakeEngine) CgroupVersion(context.Context) (string, error) { return "2", f.call("cgroup") }
func (f fakeEngine) Rootless(context.Context) (bool, error)        { return true, f.call("rootless") }
func (f fakeEngine) ReadFiles(context.Context, string, ...string) ([]byte, error) {
	return []byte("cpuset cpu io memory pids"), f.call("cgroup.controllers")
}
func (f fakeEngine) ImageID(context.Context, string) (string, error) {
	return strings.Repeat("a", 64), f.call("image id")
}
func (f fakeEngine) Tag(_ context.Context, _, dst string) error { return f.call("tag %s", dst) }

type fakeHost struct{}

func (fakeHost) InotifyLimit() (int, error) { return 8192, nil }
func (fakeHost) InotifyUsed() int           { return 10 }
func (fakeHost) PortFree(int) (bool, error) { return true, nil }

type fakeCharts struct{ *recorder }

func (f fakeCharts) Fetch(_ context.Context, c pins.Chart) (string, error) {
	return "/cache/" + c.Name + ".tgz", f.call("fetch %s", c.Name)
}
func (f fakeCharts) Install(_ context.Context, _ cluster.Target, r cluster.Release) error {
	for _, v := range r.SetString {
		if version, ok := strings.CutPrefix(v, "api.version="); ok {
			f.version = version
		}
	}
	values := append(r.Set, r.SetString...)
	if r.HooksOnly {
		values = append(values, "hooksOnly")
	}
	return f.call("install %s %s", r.Name, strings.Join(values, " "))
}

type fakeCluster struct {
	*recorder
	image string
}

func (f fakeCluster) Nodes(context.Context) ([]cluster.Node, error) {
	return []cluster.Node{{Name: "s", Ready: true}, {Name: "a", Ready: true}}, f.call("nodes")
}
func (f fakeCluster) Certificate(context.Context, string, string) (cluster.Certificate, error) {
	return cluster.Certificate{Ready: true}, nil
}
func (f fakeCluster) Gateway(context.Context, string, string) (cluster.Gateway, error) {
	return cluster.Gateway{Programmed: true}, nil
}
func (f fakeCluster) EnsureNamespace(_ context.Context, name string, labels map[string]string) error {
	return f.call("namespace %s %s", name, labels["pod-security.kubernetes.io/enforce-version"])
}
func (f fakeCluster) Rollout(context.Context, string, string) (cluster.Rollout, error) {
	return cluster.Rollout{Wanted: 1, Selector: "app=api"}, nil
}
func (f fakeCluster) Pods(context.Context, string, string) ([]cluster.Pod, error) {
	return []cluster.Pod{{Name: "api-1", Images: map[string]string{"api": f.image}}}, nil
}
func (f fakeCluster) ProxyGet(context.Context, string, string, string, string) ([]byte, error) {
	return []byte(`{"version":"` + f.recorder.version + `","commit":"` + commit + `"}`), f.call("identity")
}

type fakeAdmission struct{ *recorder }

func (f fakeAdmission) Admit(_ context.Context, ref string, p admission.Policy) (admission.Admitted, error) {
	repository, digest, _ := artifact.ParseRef(ref)
	return admission.Admitted{Publication: artifact.Publication{Artifact: artifact.Artifact{
		Repository: repository, Digest: digest, Commit: commit,
	}}}, f.call("admit %s attested=%v", p.Environment, p.Attested)
}

// fakeSecretStore — magazyn sekretów; zapisuje kolejność kroków.
type fakeSecretStore struct{ *recorder }

func (f fakeSecretStore) Prepare(context.Context) error { return f.call("openbao prepare") }
func (f fakeSecretStore) Bootstrap(context.Context) (secrets.Status, error) {
	return secrets.Status{}, f.call("openbao bootstrap")
}

// versions — wersja commita z historii repozytorium.
type versions struct{}

func (versions) VersionOf(_ context.Context, c string) (string, error) {
	return "0.3.1-dev.4+" + c[:7], nil
}

type fakeBuilder struct{ *recorder }

func (f fakeBuilder) Build(context.Context, build.Options) (build.Result, error) {
	return build.Result{Artifact: artifact.Artifact{Repository: apiImage, Commit: commit}, Version: "0.3.1-dev.4+ccccccc-dirty", Dirty: true}, f.call("build")
}

func deployer(t *testing.T, env string, rec *recorder, exists bool, podImage string) Deployer {
	t.Helper()
	p, err := pins.Load(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	// Ścieżki do chartów i konfiguracji k3d z repozytorium; kubeconfig ląduje
	// w katalogu tymczasowym.
	for _, dir := range []string{"deploy"} {
		if err := os.Symlink(filepath.Join(must(filepath.Abs(repoRoot)), dir), filepath.Join(root, dir)); err != nil {
			t.Fatal(err)
		}
	}
	return Deployer{
		Root: root, Env: definition(t, env), Pins: p, Service: "api", Image: apiImage, LogDir: t.TempDir(),
		Clusters: fakeClusters{rec, exists}, Engine: fakeEngine{rec}, Host: fakeHost{}, Charts: fakeCharts{rec}, Releases: fakeCharts{rec},
		Connect: func(cluster.Target) (Cluster, error) { return fakeCluster{rec, podImage}, rec.call("connect") },
		Builder: fakeBuilder{rec}, Admission: fakeAdmission{rec}, Versions: versions{},
		SecretStores: func(cluster.Target) (SecretStore, error) { return fakeSecretStore{rec}, nil },
		DNSTokens:    func(cluster.Target) (DNSToken, error) { return fakeDNSToken{rec}, nil },
	}
}

// fakeDNSToken — token Cloudflare; zapisuje wywołanie z podpowiedzią.
type fakeDNSToken struct{ *recorder }

func (f fakeDNSToken) Ensure(_ context.Context, hint string) error { return f.call("dns-token ensure") }

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

// TestUpFromRegistry: obraz wpuszczony przed jakąkolwiek zmianą na
// klastrze, platforma w kolejności zależności, aplikacja z digestem,
// a na końcu tożsamość na podach.
func TestUpFromRegistry(t *testing.T) {
	quiet(t)
	t.Setenv("DOCFIND_ACME_EMAIL", "")
	digest := "sha256:" + strings.Repeat("d", 64)
	rec := &recorder{}
	d := deployer(t, "dev", rec, false, apiImage+":cccccccccccc@"+digest)
	d.Env.Artifact.Sources = []string{environment.SourceRegistry}
	if err := d.Up(context.Background(), Source{Kind: environment.SourceRegistry, Ref: apiImage + "@" + digest}); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rec.calls, "\n"))
	}
	want := []string{
		"admit dev attested=false",
		"k3d exists", "cgroup", "cgroup.controllers", "rootless", "k3d create docfind userns=true",
		"k3d kubeconfig", "connect", "nodes",
		"fetch coredns", "install coredns ", "fetch envoy-gateway", "install envoy-gateway ", "fetch cert-manager", "install cert-manager ",
	}
	if !slices.Equal(rec.calls[:len(want)], want) {
		t.Errorf("wywołania:\n%s\noczekiwano początku:\n%s", strings.Join(rec.calls, "\n"), strings.Join(want, "\n"))
	}
	joined := strings.Join(rec.calls, "\n")
	// Obraz commita: tag z commita, digest, a wersja wdrożenia z historii
	// repozytorium (DOCFIND_VERSION), nie z obrazu.
	// OpenBao po charcie platformy (certyfikat z wewnętrznego CA): klucz
	// odpieczętowania przed chartem, inicjalizacja po nim, bez czekania
	// Helma na gotowość.
	if i, j, k := slices.Index(rec.calls, "openbao prepare"), slices.Index(rec.calls, "install openbao hooksOnly"), slices.Index(rec.calls, "openbao bootstrap"); i < 0 || !(i < j && j < k) || j < slices.IndexFunc(rec.calls, func(c string) bool { return strings.HasPrefix(c, "install platform") }) {
		t.Errorf("kolejność OpenBao:\n%s", joined)
	}
	for _, wantCall := range []string{"install platform gateway.name=", "namespace docfind v1.36", "install docfind route.hostname=", "api.image.tag=cccccccccccc", "api.version=0.3.1-dev.4+ccccccc", "api.image.digest=" + digest, "identity"} {
		if !strings.Contains(joined, wantCall) {
			t.Errorf("brak %q w:\n%s", wantCall, joined)
		}
	}
	if strings.Contains(joined, "k3d import") {
		t.Error("obraz z rejestru importowany do węzłów — węzły pobierają go same")
	}
	if strings.Contains(joined, "dns-token") {
		t.Error("token Cloudflare sprawdzany w środowisku z wewnętrznym CA")
	}
	if raw, err := os.ReadFile(d.Env.KubeconfigPath(d.Root)); err != nil || string(raw) != "kubeconfig" {
		t.Errorf("kubeconfig: %q, %v", raw, err)
	}
}

// TestUpLetsEncrypt: w środowisku z Let's Encrypt token Cloudflare jest
// sprawdzany po OpenBao (żyje w nim), a nie przed chartem platformy — na
// nowym klastrze `dns-token` ma gdzie go zapisać dopiero po OpenBao.
func TestUpLetsEncrypt(t *testing.T) {
	quiet(t)
	digest := "sha256:" + strings.Repeat("d", 64)
	rec := &recorder{}
	d := deployer(t, "staging", rec, true, apiImage+":cccccccccccc@"+digest)
	d.Env.Gateway.ACMEEmail = "ops@example.com"
	if err := d.Up(context.Background(), Source{Kind: environment.SourceRegistry, Ref: apiImage + "@" + digest}); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rec.calls, "\n"))
	}
	if i, j := slices.Index(rec.calls, "openbao bootstrap"), slices.Index(rec.calls, "dns-token ensure"); i < 0 || j < i {
		t.Errorf("token przed OpenBao albo wcale:\n%s", strings.Join(rec.calls, "\n"))
	}
}

// TestUpLocalBuild: obraz z drzewa dostaje tag z identyfikatora obrazu
// i trafia do węzłów importem; zła tożsamość na podach to warunek
// niespełniony.
func TestUpLocalBuild(t *testing.T) {
	quiet(t)
	local := apiImage + ":local-" + strings.Repeat("a", 12)
	rec := &recorder{}
	if err := deployer(t, "dev", rec, true, local).Up(context.Background(), Source{Kind: environment.SourceLocal}); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rec.calls, "\n"))
	}
	joined := strings.Join(rec.calls, "\n")
	for _, wantCall := range []string{"build", "tag " + local, "k3d import " + local, "api.image.tag=local-aaaaaaaaaaaa", "api.version=0.3.1-dev.4+ccccccc-dirty"} {
		if !strings.Contains(joined, wantCall) {
			t.Errorf("brak %q w:\n%s", wantCall, joined)
		}
	}
	if strings.Contains(joined, "k3d create") || strings.Contains(joined, "api.image.digest") {
		t.Errorf("istniejący klaster tworzony albo obraz lokalny z digestem:\n%s", joined)
	}

	rec = &recorder{}
	err := deployer(t, "dev", rec, true, apiImage+":inny").Up(context.Background(), Source{Kind: environment.SourceLocal})
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "tożsamość na podach") {
		t.Errorf("inny obraz na podzie: %v", err)
	}
}

// TestUpStopsOnBadArtifact: zły artefakt zatrzymuje wdrożenie, zanim
// cokolwiek dotknie klastra.
func TestUpStopsOnBadArtifact(t *testing.T) {
	quiet(t)
	rec := &recorder{fail: "build"}
	err := deployer(t, "dev", rec, true, "").Up(context.Background(), Source{Kind: environment.SourceLocal})
	if err == nil || !slices.Equal(rec.calls, []string{"build"}) {
		t.Errorf("po porażce builda: %v, wywołania %v", err, rec.calls)
	}
}

// TestUpRefusesBeforeTouchingAnything: odmowy z definicji padają, zanim
// cokolwiek zostanie wywołane.
func TestUpRefusesBeforeTouchingAnything(t *testing.T) {
	quiet(t)
	tests := []struct {
		name, env string
		src       Source
		want      string
	}{
		{"build z drzewa na ci", "ci", Source{Kind: environment.SourceLocal}, "nie przyjmuje obrazu ze źródła local"},
		{"archiwum na staging", "staging", Source{Kind: environment.SourceArchive, Archive: "x.tar", Config: "sha256:x"}, "nie przyjmuje obrazu ze źródła archive"},
		{"Let's Encrypt bez adresu ACME", "staging", Source{Kind: environment.SourceRegistry, Ref: apiImage + "@sha256:" + strings.Repeat("d", 64)}, "DOCFIND_ACME_EMAIL"},
		{"produkcja bez klastra", "prod", Source{Kind: environment.SourceRegistry}, "tylko klastry k3d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			err := deployer(t, tt.env, rec, true, "").Up(context.Background(), tt.src)
			if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("oczekiwano odmowy z %q, jest %v", tt.want, err)
			}
			if len(rec.calls) > 0 {
				t.Errorf("przed odmową wywołano %v", rec.calls)
			}
		})
	}
}

func TestDownRequiresPermission(t *testing.T) {
	rec := &recorder{}
	d := deployer(t, "staging", rec, true, "")
	if err := d.Down(context.Background()); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "cluster-delete") {
		t.Errorf("usunięcie stagingu: %v", err)
	}
	if len(rec.calls) > 0 {
		t.Errorf("wywołano %v", rec.calls)
	}
	dev := deployer(t, "dev", rec, true, "")
	if err := dev.Down(context.Background()); err != nil || !slices.Equal(rec.calls, []string{"k3d delete docfind"}) {
		t.Errorf("usunięcie dev: %v, %v", err, rec.calls)
	}
}
