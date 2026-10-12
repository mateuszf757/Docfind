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
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

const chartRepo = "ghcr.io/mateuszf757/charts/docfind"

// fakeChartRegistry — repozytorium charta z prawdziwymi artefaktami
// (core/chart), żeby odczyt szedł tą samą ścieżką co z rejestru.
type fakeChartRegistry struct {
	*recorder
	charts map[string]chart.OCI
}

func (f fakeChartRegistry) Tags(_ context.Context, repo string) ([]string, error) {
	// Atestacja obok charta — tag spoza SemVer, który rozwiązanie pomija.
	tags := []string{"sha256-" + strings.Repeat("e", 64)}
	for v := range f.charts {
		tags = append(tags, v)
	}
	slices.Sort(tags)
	return tags, f.call("tags %s", repo)
}

func (f fakeChartRegistry) Manifest(_ context.Context, ref string) ([]byte, string, error) {
	_, version, _ := strings.Cut(strings.TrimPrefix(ref, chartRepo), ":")
	oci, ok := f.charts[version]
	if !ok {
		return nil, "", fmt.Errorf("brak %s", ref)
	}
	return oci.Manifest, chart.ManifestMediaType, f.call("manifest %s", ref)
}

func (f fakeChartRegistry) Blob(_ context.Context, _, digest string) ([]byte, error) {
	for _, oci := range f.charts {
		if sum := sha256.Sum256(oci.Archive); "sha256:"+hex.EncodeToString(sum[:]) == digest {
			return oci.Archive, nil
		}
	}
	return nil, fmt.Errorf("brak bloba %s", digest)
}

// published — chart w wersji version z obrazem commita (tag = 12 znaków
// commita, jak publikuje CI) po digeście.
func published(t *testing.T, version, digest string) chart.OCI {
	t.Helper()
	files, err := chart.Files(filepath.Join(repoRoot, "deploy", "charts", "docfind"))
	if err != nil {
		t.Fatal(err)
	}
	tag := commit[:12]
	oci, err := chart.Artifact("docfind", files, chart.Build{
		Version: version, AppVersion: tag, Tag: tag, Digest: digest, DeployVersion: version + "+" + commit[:7],
	}, time.Unix(0, 0), "https://github.com/mateuszf757/Docfind")
	if err != nil {
		t.Fatal(err)
	}
	return oci
}

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// gitopsDeployer — staging z Argo CD: chart(y) w rejestrze i kolejne stany
// Application.
func gitopsDeployer(t *testing.T, rec *recorder, charts map[string]chart.OCI, apps []cluster.Application, podImage string, helmRecords ...string) Deployer {
	t.Helper()
	d := deployer(t, "staging", rec, true, podImage)
	d.Env.Gateway.ACMEEmail = "ops@example.com"
	d.ChartRegistry = fakeChartRegistry{rec, charts}
	d.Poll, d.DeliveryTimeout = time.Millisecond, 50*time.Millisecond
	d.Connect = func(cluster.Target) (Cluster, error) {
		return fakeCluster{recorder: rec, image: podImage, apps: &apps, helmRecords: helmRecords}, rec.call("connect")
	}
	return d
}

func deployed(version string) cluster.Application {
	return cluster.Application{Sync: "Synced", Revision: version, Health: "Healthy",
		Operation: cluster.AppOperation{Phase: "Succeeded", Revision: version}}
}

// TestUpGitOps: chart, który Argo wybierze z zakresu, i jego obraz
// rozstrzygnięte przed zmianą na klastrze; Argo CD na końcu platformy,
// w przestrzeni z PSA restricted; Application z wartościami z definicji;
// czekanie, aż Argo wdroży tę wersję; tożsamość na podach; zapis wydania
// Helma sprzed Argo usunięty.
func TestUpGitOps(t *testing.T) {
	quiet(t)
	rec := &recorder{version: "0.4.1-dev.3+" + commit[:7]}
	charts := map[string]chart.OCI{"0.4.1-dev.2": published(t, "0.4.1-dev.2", digestOf('b')), "0.4.1-dev.3": published(t, "0.4.1-dev.3", digestOf('d'))}
	// Argo najpierw na wersji sprzed odświeżenia, potem rollout, potem
	// wdrożona wersja z rejestru.
	apps := []cluster.Application{
		deployed("0.4.1-dev.2"),
		{Sync: "Synced", Revision: "0.4.1-dev.3", Health: "Progressing", Operation: cluster.AppOperation{Phase: "Running", Revision: "0.4.1-dev.3"}},
		deployed("0.4.1-dev.3"),
	}
	podImage := apiImage + ":" + commit[:12] + "@" + digestOf('d')
	d := gitopsDeployer(t, rec, charts, apps, podImage, "sh.helm.release.v1.docfind.v7")
	if err := d.Up(context.Background(), Source{}); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rec.calls, "\n"))
	}
	joined := strings.Join(rec.calls, "\n")
	// Najnowsza wersja z zakresu i jej obraz wpuszczony polityką stagingu —
	// zanim cokolwiek dotknie klastra.
	want := []string{"tags " + chartRepo, "manifest " + chartRepo + ":0.4.1-dev.3", "admit staging attested=true", "k3d exists"}
	if !slices.Equal(rec.calls[:len(want)], want) {
		t.Errorf("początek:\n%s\noczekiwano:\n%s", joined, strings.Join(want, "\n"))
	}
	order := []string{
		"dns-token ensure", "namespace argocd v1.36", "fetch argocd", "install argocd ",
		"namespace docfind v1.36", "openbao app secrets", "install delivery ", "refresh argocd/docfind",
		"application argocd/docfind 0.4.1-dev.3", "identity", "delete secret docfind/sh.helm.release.v1.docfind.v7",
	}
	last := -1
	for _, o := range order {
		i := slices.IndexFunc(rec.calls, func(c string) bool { return strings.HasPrefix(c, o) })
		if i <= last {
			t.Errorf("%q nie po poprzednim kroku:\n%s", o, joined)
		}
		last = i
	}
	install := rec.calls[slices.IndexFunc(rec.calls, func(c string) bool { return strings.HasPrefix(c, "install delivery ") })]
	for _, v := range []string{"application.repoURL=ghcr.io/mateuszf757/charts", "application.versions=>=0.0.0-0",
		"application.values.route.hostname=staging.docfind.lol", "application.values.api.secrets.externalSecret.caBundle=Q0E=", "hooksOnly"} {
		if !strings.Contains(install, v) {
			t.Errorf("Application bez %s: %s", v, install)
		}
	}
	// Aplikacji nie instaluje Helm — tylko Argo.
	if strings.Contains(joined, "install docfind") {
		t.Errorf("chart aplikacji zainstalowany Helmem obok Argo CD:\n%s", joined)
	}
}

// TestUpGitOpsNewerDuringWait: CI opublikował nowszy chart, gdy dft czekał
// — Argo go wdraża, a dft rozstrzyga zakres jeszcze raz (z polityką), zamiast
// czekać na wersję, której Argo już nie wybierze.
func TestUpGitOpsNewerDuringWait(t *testing.T) {
	quiet(t)
	rec := &recorder{version: "0.4.1-dev.4+" + commit[:7]}
	charts := map[string]chart.OCI{"0.4.1-dev.3": published(t, "0.4.1-dev.3", digestOf('d'))}
	d := gitopsDeployer(t, rec, charts, []cluster.Application{deployed("0.4.1-dev.4")}, apiImage+":"+commit[:12]+"@"+digestOf('f'))
	// Nowa wersja pojawia się w rejestrze po rozstrzygnięciu przed klastrem.
	reg := d.ChartRegistry.(fakeChartRegistry)
	d.Connect = func(cluster.Target) (Cluster, error) {
		reg.charts["0.4.1-dev.4"] = published(t, "0.4.1-dev.4", digestOf('f'))
		apps := []cluster.Application{deployed("0.4.1-dev.4")}
		return fakeCluster{recorder: rec, image: apiImage + ":" + commit[:12] + "@" + digestOf('f'), apps: &apps}, rec.call("connect")
	}
	if err := d.Up(context.Background(), Source{}); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(rec.calls, "\n"))
	}
	if n := strings.Count(strings.Join(rec.calls, "\n"), "admit staging"); n != 2 {
		t.Errorf("obraz nowej wersji wpuszczony %d razy, oczekiwano ponownego rozstrzygnięcia:\n%s", n, strings.Join(rec.calls, "\n"))
	}
}

// TestUpGitOpsRefuses: zakres bez wersji, obraz, który nie przechodzi
// polityki, i chart opisujący obraz z innego commita zatrzymują wdrożenie
// przed klastrem; Argo, który nie wdraża wersji z rejestru, to warunek
// niespełniony z tym, na co czekał.
func TestUpGitOpsRefuses(t *testing.T) {
	quiet(t)
	t.Run("brak wersji w zakresie", func(t *testing.T) {
		rec := &recorder{}
		d := gitopsDeployer(t, rec, map[string]chart.OCI{}, nil, "")
		err := d.Up(context.Background(), Source{})
		if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "nie spełnia zakresu") || strings.Contains(strings.Join(rec.calls, "\n"), "k3d") {
			t.Errorf("%v\n%s", err, strings.Join(rec.calls, "\n"))
		}
	})
	t.Run("obraz odrzucony przez politykę", func(t *testing.T) {
		rec := &recorder{fail: "admit"}
		d := gitopsDeployer(t, rec, map[string]chart.OCI{"0.4.1-dev.3": published(t, "0.4.1-dev.3", digestOf('d'))}, nil, "")
		if err := d.Up(context.Background(), Source{}); err == nil || strings.Contains(strings.Join(rec.calls, "\n"), "k3d") {
			t.Errorf("%v\n%s", err, strings.Join(rec.calls, "\n"))
		}
	})
	t.Run("chart z obrazem innego commita", func(t *testing.T) {
		rec := &recorder{}
		files, err := chart.Files(filepath.Join(repoRoot, "deploy", "charts", "docfind"))
		if err != nil {
			t.Fatal(err)
		}
		other, err := chart.Artifact("docfind", files, chart.Build{Version: "0.4.1-dev.3", AppVersion: "abcdefabcdef", Tag: "abcdefabcdef", Digest: digestOf('d'), DeployVersion: "0.4.1-dev.3+abcdefa"}, time.Unix(0, 0), "")
		if err != nil {
			t.Fatal(err)
		}
		d := gitopsDeployer(t, rec, map[string]chart.OCI{"0.4.1-dev.3": other}, nil, "")
		err = d.Up(context.Background(), Source{})
		if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "pochodzi z commita") || strings.Contains(strings.Join(rec.calls, "\n"), "k3d") {
			t.Errorf("%v\n%s", err, strings.Join(rec.calls, "\n"))
		}
	})
	t.Run("Argo nie wdraża wersji z rejestru", func(t *testing.T) {
		rec := &recorder{}
		stuck := deployed("0.4.1-dev.2")
		stuck.Conditions = []cluster.AppCondition{{Type: "ComparisonError", Message: "failed to get tags"}}
		d := gitopsDeployer(t, rec, map[string]chart.OCI{"0.4.1-dev.3": published(t, "0.4.1-dev.3", digestOf('d'))}, []cluster.Application{stuck}, "")
		err := d.Up(context.Background(), Source{})
		if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "ComparisonError: failed to get tags") || strings.Contains(strings.Join(rec.calls, "\n"), "identity") {
			t.Errorf("%v\n%s", err, strings.Join(rec.calls, "\n"))
		}
	})
}
