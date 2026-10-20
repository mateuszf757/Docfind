// Package deploy stawia środowisko na klastrze k3d: klaster (jeśli go nie
// ma), komponenty platformy w kolejności zależności, obraz i chart
// aplikacji, a na końcu sprawdza tożsamość na każdym podzie (dawniej
// ci/deploy-local.sh).
//
// Obraz jest rozstrzygany najpierw — build, sprawdzenie archiwum albo
// wpuszczenie z rejestru — a klaster zmieniany dopiero potem: zły artefakt
// nie zostawia klastra z nową platformą i starą aplikacją.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/ociarchive"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
	"github.com/mateuszf757/Docfind/tools/internal/stages/build"
	"github.com/mateuszf757/Docfind/tools/internal/stages/identity"
	"github.com/mateuszf757/Docfind/tools/internal/stages/secrets"
)

// Clusters — klastry k3d (adapters/k3d).
type Clusters interface {
	Exists(ctx context.Context, name string) (bool, error)
	Create(ctx context.Context, c cluster.LocalCluster) error
	Delete(ctx context.Context, name string) error
	Kubeconfig(ctx context.Context, name string) ([]byte, error)
	Import(ctx context.Context, name, image string) error
}

// Engine — silnik kontenerów, na którym stoją węzły (adapters/docker).
type Engine interface {
	CgroupVersion(ctx context.Context) (string, error)
	Rootless(ctx context.Context) (bool, error)
	ReadFiles(ctx context.Context, ref string, paths ...string) ([]byte, error)
	ImageID(ctx context.Context, ref string) (string, error)
	Tag(ctx context.Context, src, dst string) error
}

// Host — maszyna, na której stoi klaster (adapters/host).
type Host interface {
	InotifyLimit() (int, error)
	InotifyUsed() int
	PortFree(port int) (bool, error)
}

// ChartSource — przypięte charty platformy (adapters/helm).
type ChartSource interface {
	Fetch(ctx context.Context, chart pins.Chart) (string, error)
}

// Releases — instalacja chartów na klastrze (adapters/helm).
type Releases interface {
	Install(ctx context.Context, t cluster.Target, r cluster.Release) error
}

// Cluster — klaster po połączeniu (adapters/kube).
type Cluster interface {
	identity.Cluster
	Nodes(ctx context.Context) ([]cluster.Node, error)
	Certificate(ctx context.Context, namespace, name string) (cluster.Certificate, error)
	SecretData(ctx context.Context, namespace, name string) (map[string][]byte, error)
	Gateway(ctx context.Context, namespace, name string) (cluster.Gateway, error)
	EnsureNamespace(ctx context.Context, name string, labels map[string]string) error
	// Argo CD (decyzja 46): stan i odświeżenie Application, zapis wydania
	// Helma aplikacji do usunięcia po przejęciu przez Argo.
	Application(ctx context.Context, namespace, name string) (cluster.Application, error)
	RefreshApplication(ctx context.Context, namespace, name string) error
	HelmReleaseRecords(ctx context.Context, namespace, release string) ([]string, error)
	DeleteSecret(ctx context.Context, namespace, name string) error
	// PullEvents — obrazy ze zdarzeń Pulling we wszystkich przestrzeniach.
	PullEvents(ctx context.Context) ([]string, error)
}

// Connect łączy się z klastrem przez strażnika kontekstu (kube.Connect).
type Connect func(t cluster.Target) (Cluster, error)

// Builder — build obrazu z drzewa roboczego (stages/build).
type Builder interface {
	Build(ctx context.Context, opts build.Options) (build.Result, error)
}

// Admission — wpuszczanie obrazu z rejestru (stages/admission).
type Admission interface {
	Admit(ctx context.Context, ref string, p admission.Policy) (admission.Admitted, error)
}

// Versions — wersja commita z historii repozytorium (adapters/git). Obraz
// nie niesie wersji (decyzja 40); wdrożenie podaje ją aplikacji przez
// DOCFIND_VERSION.
type Versions interface {
	VersionOf(ctx context.Context, commit string) (string, error)
}

// SecretStore — magazyn sekretów platformy (stages/secrets): przygotowanie
// przed instalacją charta OpenBao i doprowadzenie go do konfiguracji z gita
// po instalacji.
type SecretStore interface {
	Prepare(ctx context.Context) error
	Bootstrap(ctx context.Context) (secrets.Status, error)
	// EnsureAppSecrets — sekrety aplikacji w OpenBao, wygenerowane, jeśli ich
	// nie ma (token metryk).
	EnsureAppSecrets(ctx context.Context) error
}

// DNSToken — token Cloudflare dla wydawców ACME w OpenBao i jego Secret od
// ESO (stages/dnstoken); hint — jak go zapisać, gdy go nie ma.
type DNSToken interface {
	Ensure(ctx context.Context, hint string) error
}

// Deployer — wdrożenie jednego środowiska.
type Deployer struct {
	Root string
	Env  environment.Definition
	Pins pins.Pins
	// Service i Image — usługa i nazwa jej obrazu bez tagu.
	Service string
	Image   string
	// LogDir — katalog na logi węzłów z tworzenia klastra; ten sam co
	// raporty bramek, więc w CI trafiają razem do artefaktu z dowodami.
	LogDir string

	Clusters  Clusters
	Engine    Engine
	Host      Host
	Charts    ChartSource
	Releases  Releases
	Connect   Connect
	Builder   Builder
	Admission Admission
	Versions  Versions
	// SecretStores — magazyn sekretów na klastrze środowiska.
	SecretStores func(t cluster.Target) (SecretStore, error)
	// DNSTokens — token Cloudflare w środowisku z Let's Encrypt.
	DNSTokens func(t cluster.Target) (DNSToken, error)
	// ChartRegistry — chart aplikacji w rejestrze, który śledzi Argo CD.
	ChartRegistry ChartRegistry
	// AppChart — repozytorium charta aplikacji w rejestrze (<rejestr>/charts/
	// docfind): stamtąd upgrade pod ruchem bierze N-1 (decyzja 79).
	AppChart string
	// Download — plik spoza repozytorium w pamięci podręcznej, po sprawdzeniu
	// sumy z ci/pins.env (kit/fetch): archiwum airgap i lista obrazów k3s.
	Download func(ctx context.Context, url, sha string) (string, error)
	// Package — chart aplikacji spakowany dla obrazu (repozytorium@digest)
	// jak do publikacji (stages/chartpub): ścieżka archiwum i digest
	// manifestu OCI (decyzja 61).
	Package func(ctx context.Context, image string) (archive, digest string, err error)
	// Poll i DeliveryTimeout — odstęp sprawdzania stanu Argo CD i limit
	// czekania na wdrożenie; zero — 5 s i 10 min.
	Poll, DeliveryTimeout time.Duration
}

// Source — skąd wziąć obraz.
type Source struct {
	// PackageChart — chart z obrazem z rejestru spakowany jak do publikacji
	// i zainstalowany z archiwum, zamiast z katalogu z --set obrazu: na
	// klastrze stoi wtedy ten sam artefakt, który zadanie chart opublikuje
	// (decyzja 61). Tylko z Kind = SourceRegistry.
	PackageChart bool
	// Kind — environment.SourceLocal, SourceArchive albo SourceRegistry;
	// pusty — bez wskazania: build z drzewa, a w środowisku z Argo CD obraz
	// z charta, który Argo wybierze.
	Kind string
	// Archive — archiwum `docker save` z zadania build; Config — digest
	// konfiguracji obrazu, który to zadanie sprawdziło.
	Archive string
	Config  string
	// Ref — repozytorium@sha256:… indeksu w rejestrze.
	Ref string
	// Release — numer wydania, gdy środowisko przyjmuje tylko wydania.
	Release string
}

// image — obraz rozstrzygnięty przed jakąkolwiek zmianą na klastrze.
type image struct {
	// tag i digest dla charta (api.image.tag, api.image.digest).
	tag, digest string
	// load — co zaimportować do węzłów (obraz z demona Dockera albo
	// archiwum); puste, gdy węzły pobierają obraz z rejestru same.
	load string
	want artifact.Expected
}

func (d Deployer) target() cluster.Target { return d.Env.Target(d.Root) }

func (d Deployer) path(rel string) string { return filepath.Join(d.Root, rel) }

// Down usuwa klaster środowiska — tylko k3d, tylko ten z definicji i tylko
// tam, gdzie definicja na to pozwala.
func (d Deployer) Down(ctx context.Context) error {
	if d.Env.Cluster.Provider != environment.ProviderK3d {
		return cli.Unmet("środowisko %s nie jest klastrem k3d — dft go nie usuwa", d.Env.Name)
	}
	if err := d.Env.Require(environment.OpClusterDelete); err != nil {
		return err
	}
	if err := d.Clusters.Delete(ctx, d.Env.Cluster.Name); err != nil {
		return err
	}
	if err := os.Remove(d.Env.KubeconfigPath(d.Root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Up stawia środowisko z obrazem ze źródła src. W środowisku z Argo CD
// (delivery.argocd) obraz wybiera Argo z charta w rejestrze, a dft stawia
// platformę z Argo CD, zakłada Application i sprawdza, co Argo wdrożył.
func (d Deployer) Up(ctx context.Context, src Source) error {
	if d.Env.Cluster.Provider != environment.ProviderK3d {
		return cli.Unmet("środowisko %s (cluster.provider=%s): wdrożenie przez dft obsługuje tylko klastry k3d", d.Env.Name, d.Env.Cluster.Provider)
	}
	if err := d.Env.Require(environment.OpDeploy); err != nil {
		return err
	}
	switch {
	case d.Env.GitOps() && src.Kind != "":
		return cli.Unmet("środowisko %s wdraża Argo CD z charta %s w zakresie %s — obraz wybiera zakres, nie źródło obrazu (%s); cofnięcie to przypięcie wersji w definicji", d.Env.Name, d.Env.Delivery.ArgoCD.Chart, d.Env.Delivery.ArgoCD.Versions, src.Kind)
	case src.Kind == "" && !d.Env.GitOps():
		src.Kind = environment.SourceLocal
	}
	if src.Kind != "" && !d.Env.AllowsSource(src.Kind) {
		return cli.Unmet("środowisko %s nie przyjmuje obrazu ze źródła %s (artifact.sources: %v)", d.Env.Name, src.Kind, d.Env.Artifact.Sources)
	}
	if err := d.requireACMEEmail(); err != nil {
		return err
	}
	pre := &preflight{}
	d.checkInotify(pre)
	if err := pre.err(); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "dft-up-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	// Artefakt rozstrzygnięty przed zmianą na klastrze: obraz albo — przy
	// Argo CD — wersja charta, którą Argo wybierze, z obrazem wpuszczonym
	// tą samą polityką.
	var (
		img image
		rel release
	)
	if d.Env.GitOps() {
		rel, err = d.resolveChart(ctx)
	} else {
		img, err = d.resolve(ctx, src, tmp)
	}
	if err != nil {
		return err
	}

	exists, err := d.Clusters.Exists(ctx, d.Env.Cluster.Name)
	switch {
	case err != nil:
		return err
	case exists:
		cli.Step("klaster %s istnieje", d.Env.Cluster.Name)
	default:
		if err := d.Env.Require(environment.OpClusterCreate); err != nil {
			return err
		}
		if err := d.createCluster(ctx); err != nil {
			return err
		}
	}

	if err := d.writeKubeconfig(ctx); err != nil {
		return err
	}
	c, err := d.Connect(d.target())
	if err != nil {
		return err
	}
	if err := waitNodes(ctx, c); err != nil {
		return err
	}
	if err := d.platform(ctx, c); err != nil {
		return err
	}
	if err := d.systemImages(ctx, c); err != nil {
		return err
	}
	if img.load != "" {
		cli.Step("import %s do węzłów", img.want.Image)
		if err := d.Clusters.Import(ctx, d.Env.Cluster.Name, img.load); err != nil {
			return err
		}
	}
	if err := d.namespace(ctx, c); err != nil {
		return err
	}
	if d.Env.GitOps() {
		return d.deliver(ctx, c, rel)
	}
	return d.app(ctx, c, img, src.PackageChart)
}

// requireACMEEmail: wydawca Let's Encrypt potrzebuje adresu konta ACME,
// którego nie ma w żadnej definicji (dane osobowe w publicznym repozytorium).
func (d Deployer) requireACMEEmail() error {
	gw := d.Env.Gateway
	if strings.HasPrefix(gw.Issuer, "letsencrypt") && gw.ACMEEmail == "" {
		return cli.Unmet("wydawca %s wymaga adresu konta ACME — DOCFIND_ACME_EMAIL w mise.local.toml", gw.Issuer)
	}
	return nil
}

func (d Deployer) resolve(ctx context.Context, src Source, tmp string) (image, error) {
	switch src.Kind {
	case environment.SourceLocal:
		return d.localImage(ctx)
	case environment.SourceArchive:
		return d.archiveImage(ctx, src, tmp)
	case environment.SourceRegistry:
		return d.registryImage(ctx, src.Ref, src.Release)
	default:
		return image{}, fmt.Errorf("nieznane źródło obrazu %q", src.Kind)
	}
}

// localImage buduje obraz z drzewa roboczego.
//
// Tag wyliczony z zawartości obrazu, nie z wersji. Build z brudnego drzewa
// dostaje za każdym razem ten sam tag „…-dirty"; przy IfNotPresent
// i niezmienionej specyfikacji poda Helm nie zrobiłby rolloutu, a na
// klastrze dalej chodziłby stary kod pod nową nazwą.
func (d Deployer) localImage(ctx context.Context) (image, error) {
	res, err := d.Builder.Build(ctx, build.Options{Root: d.Root, Service: d.Service, Image: d.Image, Out: cli.Out, Err: cli.Err})
	if err != nil {
		return image{}, err
	}
	id, err := d.Engine.ImageID(ctx, res.LocalRef())
	if err != nil {
		return image{}, err
	}
	tag := "local-" + id[:12]
	ref := d.Image + ":" + tag
	if err := d.Engine.Tag(ctx, res.LocalRef(), ref); err != nil {
		return image{}, err
	}
	return image{tag: tag, load: ref, want: artifact.Expected{Image: ref, Version: res.Version, Commit: res.Commit}}, nil
}

// archiveImage sprawdza archiwum `docker save` z zadania build: konfiguracja
// obrazu to ta, którą zadanie sprawdziło (artefakt nie został podmieniony ani
// uszkodzony po drodze), a commit bierze z etykiety obrazu. Archiwum do
// importu dostaje tag z digestu konfiguracji — z tego samego powodu co obraz
// lokalny: ten sam tag zawsze znaczy tę samą zawartość.
func (d Deployer) archiveImage(ctx context.Context, src Source, tmp string) (image, error) {
	if src.Archive == "" || src.Config == "" {
		return image{}, cli.Usage("obraz z archiwum wymaga ścieżki archiwum i digestu konfiguracji z zadania build")
	}
	img, err := ociarchive.Open(src.Archive)
	if err != nil {
		return image{}, err
	}
	config, err := img.Config()
	if err != nil {
		return image{}, fmt.Errorf("archiwum %s: %w", src.Archive, err)
	}
	if config != src.Config {
		return image{}, cli.Unmet("archiwum %s: obraz z konfiguracją %s, a zadanie build sprawdziło %s", src.Archive, config, src.Config)
	}
	labels, err := img.Labels()
	if err != nil {
		return image{}, fmt.Errorf("archiwum %s: %w", src.Archive, err)
	}
	commit := labels[artifact.RevisionLabel]
	if commit == "" {
		return image{}, cli.Unmet("archiwum %s: obraz bez etykiety commita", src.Archive)
	}
	version, err := d.Versions.VersionOf(ctx, commit)
	if err != nil {
		return image{}, err
	}
	tag := "local-" + strings.TrimPrefix(config, "sha256:")[:12]
	ref := d.Image + ":" + tag
	load := filepath.Join(tmp, "image.tar")
	if err := img.Write(ref, load); err != nil {
		return image{}, err
	}
	cli.Step("archiwum zgodne z obrazem sprawdzonym w zadaniu build (%s): %s, commit %s", config, version, artifact.ShortCommit(commit))
	return image{tag: tag, load: load, want: artifact.Expected{Image: ref, Version: version, Commit: commit}}, nil
}

// registryImage wpuszcza obraz z rejestru według definicji środowiska.
// Węzły pobierają go po digeście — tag w rejestrze da się nadpisać, digest
// nie. Wersja wdrożenia to numer wydania albo wersja commita z gita.
func (d Deployer) registryImage(ctx context.Context, ref, release string) (image, error) {
	admitted, err := d.Admission.Admit(ctx, ref, admission.For(d.Env, d.Image, release))
	if err != nil {
		return image{}, err
	}
	a := admitted.Publication.Artifact
	version := a.Release
	if version == "" {
		if version, err = d.Versions.VersionOf(ctx, a.Commit); err != nil {
			return image{}, err
		}
	}
	return image{tag: a.Tag(), digest: a.Digest, want: artifact.Expected{Image: a.Pinned(), Version: version, Commit: a.Commit}}, nil
}
