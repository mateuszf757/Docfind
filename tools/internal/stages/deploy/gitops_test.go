package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/delivery"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/platform"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

const (
	bundleRepo = "ghcr.io/mateuszf757/charts/docfind-delivery"
	appRepo    = "ghcr.io/mateuszf757/charts/docfind"
)

// fakeChartRegistry — repozytoria pakietu dostawy i charta aplikacji
// z prawdziwymi artefaktami (core/chart), żeby odczyt szedł tą samą ścieżką
// co z rejestru: manifest, warstwa, wartości.
type fakeChartRegistry struct {
	*recorder
	// bundles — pakiety po wersji; byDigest — każdy artefakt po digeście.
	bundles  map[string]chart.OCI
	byDigest map[string]chart.OCI
}

func newRegistry(rec *recorder) *fakeChartRegistry {
	return &fakeChartRegistry{recorder: rec, bundles: map[string]chart.OCI{}, byDigest: map[string]chart.OCI{}}
}

func (f *fakeChartRegistry) Tags(_ context.Context, repo string) ([]string, error) {
	// Atestacja obok pakietu — tag spoza SemVer, który rozwiązanie pomija.
	tags := []string{"sha256-" + strings.Repeat("e", 64)}
	for v := range f.bundles {
		tags = append(tags, v)
	}
	slices.Sort(tags)
	return tags, f.call("tags %s", repo)
}

func (f *fakeChartRegistry) Manifest(_ context.Context, ref string) ([]byte, string, error) {
	if repo, digest, ok := strings.Cut(ref, "@"); ok {
		oci, found := f.byDigest[digest]
		if !found {
			return nil, "", fmt.Errorf("brak %s", ref)
		}
		return oci.Manifest, chart.ManifestMediaType, f.call("manifest %s@", repo)
	}
	version := ref[strings.LastIndex(ref, ":")+1:]
	oci, found := f.bundles[version]
	if !found {
		return nil, "", fmt.Errorf("brak %s", ref)
	}
	return oci.Manifest, chart.ManifestMediaType, f.call("manifest %s", ref)
}

func (f *fakeChartRegistry) Blob(_ context.Context, _, digest string) ([]byte, error) {
	for _, oci := range f.byDigest {
		if sum := sha256.Sum256(oci.Archive); "sha256:"+hex.EncodeToString(sum[:]) == digest {
			return oci.Archive, nil
		}
	}
	return nil, fmt.Errorf("brak bloba %s", digest)
}

// publish dodaje do rejestru chart aplikacji z obrazem digest (tag = 12
// znaków commita, jak publikuje CI) i pakiet dostawy w wersji version, który
// go wdraża. Zwraca pakiet i digest manifestu pakietu.
func (f *fakeChartRegistry) publish(t *testing.T, version, digest string) (delivery.Bundle, string) {
	t.Helper()
	files, err := chart.Files(filepath.Join(repoRoot, "deploy", "charts", "docfind"))
	if err != nil {
		t.Fatal(err)
	}
	tag := commit[:12]
	app, err := chart.Artifact("docfind", files, chart.Build{
		Version: version, AppVersion: tag, Tag: tag, Digest: digest, DeployVersion: version + "+" + commit[:7],
	}, time.Unix(0, 0), "https://github.com/mateuszf757/Docfind")
	if err != nil {
		t.Fatal(err)
	}
	f.byDigest[app.Digest()] = app
	d := "sha256:" + strings.Repeat("a", 64)
	mirror := map[string]string{}
	for _, pc := range platform.Mirrored() {
		mirror[pc.Pin] = d
	}
	b, err := delivery.Compose(delivery.Artifacts{
		Mirror: mirror, MirrorRepo: func(pin string) string { return "ghcr.io/mateuszf757/charts/platform/" + pin },
		PlatformRepo: "ghcr.io/mateuszf757/charts/docfind-platform", PlatformDigest: d,
		AppRepo: appRepo, AppDigest: app.Digest(),
	}, []environment.Definition{definition(t, "staging")})
	if err != nil {
		t.Fatal(err)
	}
	bundleFiles, err := chart.Files(filepath.Join(repoRoot, "deploy", "charts", "delivery"))
	if err != nil {
		t.Fatal(err)
	}
	oci, err := chart.Values("docfind-delivery", bundleFiles, chart.Meta{Version: version, AppVersion: tag}, b.Values(), time.Unix(0, 0), "")
	if err != nil {
		t.Fatal(err)
	}
	f.bundles[version] = oci
	f.byDigest[oci.Digest()] = oci
	return b, oci.Digest()
}

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func deployed(digest string) cluster.Application {
	return cluster.Application{Sync: "Synced", Revision: digest, Health: "Healthy",
		Operation: cluster.AppOperation{Phase: "Succeeded", Revision: digest}}
}

// wanted — stany Application: korzeń na digeście pakietu, każdy komponent na
// swoim digeście, OpenBao zsynchronizowany od razu.
func wanted(b delivery.Bundle, bundleDigest string) map[string][]cluster.Application {
	apps := map[string][]cluster.Application{environment.RootApplication: {deployed(bundleDigest)}}
	for _, c := range b.Components {
		apps[c.Name] = []cluster.Application{deployed(c.Digest)}
	}
	return apps
}

// gitopsDeployer — staging z Argo CD: rejestr z pakietami i stany Application.
func gitopsDeployer(t *testing.T, rec *recorder, reg *fakeChartRegistry, apps map[string][]cluster.Application, podImage string, helmRecords map[string][]string) Deployer {
	t.Helper()
	d := deployer(t, "staging", rec, true, podImage)
	d.Env.Gateway.ACMEEmail = "ops@example.com"
	d.ChartRegistry = reg
	d.Poll, d.DeliveryTimeout = time.Millisecond, 50*time.Millisecond
	d.Connect = func(cluster.Target) (Cluster, error) {
		return fakeCluster{recorder: rec, image: podImage, apps: &apps, helmRecords: helmRecords}, rec.call("connect")
	}
	return d
}

// TestUpGitOps: pakiet, który Argo wybierze z zakresu, i obraz aplikacji
// z niego rozstrzygnięte przed zmianą na klastrze; dft instaluje tylko
// CoreDNS i Argo CD, przygotowuje OpenBao i przestrzeń aplikacji, zakłada
// korzeń i prowadzi kroki startowe w miejscu fal; na końcu każdy komponent
// na swoim digeście, tożsamość na podach i przejęcie wydań Helma.
func TestUpGitOps(t *testing.T) {
	quiet(t)
	rec := &recorder{version: "0.4.1-dev.3+" + commit[:7]}
	reg := newRegistry(rec)
	reg.publish(t, "0.4.1-dev.2", digestOf('b'))
	b, bundleDigest := reg.publish(t, "0.4.1-dev.3", digestOf('d'))
	apps := wanted(b, bundleDigest)
	// OpenBao: zasoby założone, pody czekają na inicjalizację.
	apps["openbao"] = []cluster.Application{{Sync: "OutOfSync"}, {Sync: "Synced", Health: "Progressing"}, deployed(apps["openbao"][0].Revision)}
	podImage := apiImage + ":" + commit[:12] + "@" + digestOf('d')
	d := gitopsDeployer(t, rec, reg, apps, podImage, map[string][]string{
		"cert-manager/cert-manager": {"sh.helm.release.v1.cert-manager.v3"},
		"argocd/delivery":           {"sh.helm.release.v1.delivery.v1"},
	})
	if err := d.Up(context.Background(), Source{}); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rec.calls, "\n"))
	}
	joined := strings.Join(rec.calls, "\n")
	// Najnowszy pakiet z zakresu i obraz z jego charta aplikacji wpuszczony
	// polityką stagingu — zanim cokolwiek dotknie klastra.
	want := []string{"tags " + bundleRepo, "manifest " + bundleRepo + ":0.4.1-dev.3", "manifest " + appRepo + "@", "admit staging attested=true", "k3d exists"}
	if !slices.Equal(rec.calls[:len(want)], want) {
		t.Errorf("początek:\n%s\noczekiwano:\n%s", joined, strings.Join(want, "\n"))
	}
	order := []string{
		"install coredns ", "namespace argocd v1.36", "install argocd ", "openbao prepare", "namespace docfind v1.36",
		"install delivery-root ", "refresh argocd/docfind-delivery", "application argocd/openbao", "openbao bootstrap",
		"dns-token ensure", "openbao app secrets", "secret cert-manager/docfind-root-ca", "configmap docfind/" + environment.PlatformCAConfigMap + " CA",
		"application argocd/docfind-delivery " + bundleDigest, "identity",
		"manifest cert-manager/cert-manager", "field manager apps/v1/Deployment cert-manager/cert-manager -helm +argocd-controller",
		"delete secret cert-manager/sh.helm.release.v1.cert-manager.v3", "delete secret argocd/sh.helm.release.v1.delivery.v1",
	}
	last := -1
	for _, o := range order {
		i := slices.IndexFunc(rec.calls, func(c string) bool { return strings.HasPrefix(c, o) })
		if i <= last {
			t.Errorf("%q nie po poprzednim kroku:\n%s", o, joined)
		}
		last = i
	}
	install := rec.calls[slices.IndexFunc(rec.calls, func(c string) bool { return strings.HasPrefix(c, "install delivery-root ") })]
	for _, v := range []string{"environment=staging", "bundle.repoURL=oci://" + bundleRepo, "bundle.versions=>=0.0.0-0", "runtime.acmeEmail=ops@example.com", "hooksOnly"} {
		if !strings.Contains(install, v) {
			t.Errorf("korzeń bez %s: %s", v, install)
		}
	}
	// Platformę i aplikację instaluje Argo, nie Helm.
	for _, helm := range []string{"install envoy-gateway", "install cert-manager", "install platform", "install openbao", "install docfind "} {
		if strings.Contains(joined, helm) {
			t.Errorf("%s Helmem obok Argo CD:\n%s", helm, joined)
		}
	}
	for _, c := range b.Components {
		if !strings.Contains(joined, "application argocd/"+c.Name+" "+c.Digest) {
			t.Errorf("komponent %s nie sprawdzony na swoim digeście", c.Name)
		}
	}
}

// TestUpGitOpsNewerDuringWait: CI opublikował nowszy pakiet, gdy dft czekał
// — Argo go wdraża, a dft rozstrzyga zakres jeszcze raz (z polityką), zamiast
// czekać na pakiet, którego Argo już nie wybierze.
func TestUpGitOpsNewerDuringWait(t *testing.T) {
	quiet(t)
	rec := &recorder{version: "0.4.1-dev.4+" + commit[:7]}
	reg := newRegistry(rec)
	reg.publish(t, "0.4.1-dev.3", digestOf('d'))
	apps := map[string][]cluster.Application{}
	d := gitopsDeployer(t, rec, reg, apps, apiImage+":"+commit[:12]+"@"+digestOf('f'), nil)
	// Nowy pakiet pojawia się po rozstrzygnięciu przed klastrem.
	connect := d.Connect
	d.Connect = func(t2 cluster.Target) (Cluster, error) {
		b, digest := reg.publish(t, "0.4.1-dev.4", digestOf('f'))
		for name, seq := range wanted(b, digest) {
			apps[name] = seq
		}
		return connect(t2)
	}
	if err := d.Up(context.Background(), Source{}); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rec.calls, "\n"))
	}
	if n := strings.Count(strings.Join(rec.calls, "\n"), "admit staging"); n != 2 {
		t.Errorf("obraz nowego pakietu wpuszczony %d razy, oczekiwano ponownego rozstrzygnięcia:\n%s", n, strings.Join(rec.calls, "\n"))
	}
}

// TestUpGitOpsRefuses: zakres bez wersji i obraz, który nie przechodzi
// polityki, zatrzymują wdrożenie przed klastrem; komponent, który nie dochodzi
// do swojego digestu, to warunek niespełniony z tym, na co czekał.
func TestUpGitOpsRefuses(t *testing.T) {
	quiet(t)
	t.Run("brak wersji w zakresie", func(t *testing.T) {
		rec := &recorder{}
		d := gitopsDeployer(t, rec, newRegistry(rec), nil, "", nil)
		err := d.Up(context.Background(), Source{})
		if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "nie spełnia zakresu") || strings.Contains(strings.Join(rec.calls, "\n"), "k3d") {
			t.Errorf("%v\n%s", err, strings.Join(rec.calls, "\n"))
		}
	})
	t.Run("obraz odrzucony przez politykę", func(t *testing.T) {
		rec := &recorder{fail: "admit"}
		reg := newRegistry(rec)
		reg.publish(t, "0.4.1-dev.3", digestOf('d'))
		d := gitopsDeployer(t, rec, reg, nil, "", nil)
		if err := d.Up(context.Background(), Source{}); err == nil || strings.Contains(strings.Join(rec.calls, "\n"), "k3d") {
			t.Errorf("%v\n%s", err, strings.Join(rec.calls, "\n"))
		}
	})
	t.Run("komponent na innym digeście", func(t *testing.T) {
		rec := &recorder{version: "0.4.1-dev.3+" + commit[:7]}
		reg := newRegistry(rec)
		b, digest := reg.publish(t, "0.4.1-dev.3", digestOf('d'))
		apps := wanted(b, digest)
		stuck := deployed(digestOf('9'))
		stuck.Conditions = []cluster.AppCondition{{Type: "ComparisonError", Message: "manifest unknown"}}
		apps["cert-manager"] = []cluster.Application{stuck}
		d := gitopsDeployer(t, rec, reg, apps, apiImage+":"+commit[:12]+"@"+digestOf('d'), nil)
		err := d.Up(context.Background(), Source{})
		if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "cert-manager:") || !strings.Contains(err.Error(), "ComparisonError: manifest unknown") || strings.Contains(strings.Join(rec.calls, "\n"), "identity") {
			t.Errorf("%v\n%s", err, strings.Join(rec.calls, "\n"))
		}
	})
}
