package chartpub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/registry"
	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/core/delivery"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// published — chart aplikacji i chart platformy z bieżącego commita w rejestrze.
func publishedCharts(t *testing.T, prefix string) (app, plat string) {
	t.Helper()
	ctx := context.Background()
	pub := publisher(t, repo{})
	pub.Repository = prefix + "/charts/docfind"
	a, err := pub.Publish(ctx, image, "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := PlatformPublisher{
		ChartDir: "../../../../deploy/charts/platform", Repo: repo{}, Registry: registry.Client{}, Public: registry.Client{Anonymous: true},
		Repository: prefix + "/charts/docfind-platform",
	}.Publish(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return a.Repository + "@" + a.Digest, p.Repository + "@" + p.Digest
}

func deliveryPublisher(t *testing.T, prefix string) DeliveryPublisher {
	t.Helper()
	return DeliveryPublisher{
		Root: "../../../..", Repo: repo{}, Registry: registry.Client{}, Public: registry.Client{Anonymous: true},
		Mirror: mirror(t, fetched{dir: t.TempDir()}, prefix), Repository: prefix + "/charts/docfind-delivery",
		Source: "https://github.com/mateuszf757/Docfind",
	}
}

// TestDeliveryPublish: pakiet z digestami charta aplikacji, charta platformy
// i lustra z tego commita, w rejestrze pod wersją z commita; ponowny bieg
// to ten sam digest.
func TestDeliveryPublish(t *testing.T) {
	quiet(t)
	prefix := testRegistry(t)
	app, plat := publishedCharts(t, prefix)
	p := deliveryPublisher(t, prefix)
	first, b, err := p.Publish(context.Background(), app, plat)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := p.Publish(context.Background(), app, plat)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != "0.4.1-dev.3" || !first.Pushed || second.Pushed || second.Digest != first.Digest {
		t.Errorf("publikacje: %+v, %+v", first, second)
	}
	// Pakiet z rejestru jest artefaktem tego commita.
	got, err := Read(context.Background(), registry.Client{}, first.Repository, first.Repository+"@"+first.Digest)
	if err != nil || got.AppVersion != artifact.CommitTag(commit) {
		t.Fatalf("pakiet w rejestrze: %+v, %v", got, err)
	}
	appComp, ok := b.App()
	_, appDigest, _ := artifact.ParseRef(app)
	if !ok || appComp.Digest != appDigest || appComp.RepoURL != "oci://"+prefix+"/charts/docfind" {
		t.Errorf("aplikacja w pakiecie: %+v", appComp)
	}
	digests, err := p.Mirror.Digests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range b.Components {
		if c.Project == delivery.PlatformProject && c.ReleaseName != "platform" && c.Digest != digests[c.Name] {
			t.Errorf("%s: digest %s, lustro %s", c.Name, c.Digest, digests[c.Name])
		}
	}
	if _, ok := b.Environments["staging"]; !ok {
		t.Errorf("pakiet bez wartości stagingu: %v", b.Environments)
	}
}

// TestDeliveryRefuses: chart aplikacji z innego commita w pakiecie commita
// bieżącego — odmowa; brudne drzewo — odmowa.
func TestDeliveryRefuses(t *testing.T) {
	quiet(t)
	prefix := testRegistry(t)
	_, plat := publishedCharts(t, prefix)
	files, err := chart.Files("../../../../deploy/charts/docfind")
	if err != nil {
		t.Fatal(err)
	}
	other, err := chart.Artifact("docfind", files, chart.Build{Version: "0.4.1-dev.2", AppVersion: "abcdef123456", Tag: "abcdef123456",
		Digest: "sha256:" + strings.Repeat("b", 64), DeployVersion: "0.4.1-dev.2+abcdef1"}, time.Unix(0, 0), "")
	if err != nil {
		t.Fatal(err)
	}
	repoName := prefix + "/charts/docfind"
	if _, err := (registry.Client{}).PushManifest(context.Background(), repoName+":0.4.1-dev.2", other.Manifest, chart.ManifestMediaType,
		artifact.Blob{Data: other.Config, MediaType: chart.ConfigMediaType}, artifact.Blob{Data: other.Archive, MediaType: chart.LayerMediaType}); err != nil {
		t.Fatal(err)
	}
	p := deliveryPublisher(t, prefix)
	_, _, err = p.Publish(context.Background(), repoName+"@"+other.Digest(), plat)
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "jest z commita abcdef123456") {
		t.Errorf("chart aplikacji z innego commita: %v", err)
	}
	p.Repo = repo{dirty: true}
	if _, _, err := p.Publish(context.Background(), repoName+"@"+other.Digest(), plat); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("brudne drzewo: %v", err)
	}
}
