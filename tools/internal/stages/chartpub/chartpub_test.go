package chartpub

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/registry"
	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

const commit = "fc8dfc11099d0123456789abcdef0123456789ab"

type repo struct{ dirty bool }

func (r repo) RequireClean(context.Context) error {
	if r.dirty {
		return cli.Unmet("drzewo brudne")
	}
	return nil
}
func (repo) Version(context.Context) (string, error)        { return "0.4.1-dev.3+fc8dfc1", nil }
func (repo) Commit(context.Context) (string, error)         { return commit, nil }
func (repo) SourceDateEpoch(context.Context) (int64, error) { return 1791800000, nil }

// images — obraz w rejestrze z etykietą revision.
type images struct{ commit string }

func (i images) Inspect(_ context.Context, ref string) (artifact.Publication, error) {
	a := artifact.Artifact{Commit: i.commit}
	return artifact.Publication{Artifact: a}, nil
}

func publisher(t *testing.T, r repo) Publisher {
	t.Helper()
	server := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	return Publisher{
		ChartDir: "../../../../deploy/charts/docfind", Repo: r, Registry: registry.Client{}, Images: images{commit},
		Repository: strings.TrimPrefix(server.URL, "http://") + "/mateuszf757/charts/docfind",
		Source:     "https://github.com/mateuszf757/Docfind",
	}
}

const image = "ghcr.io/mateuszf757/docfind-api@sha256:733c718c4c1ed725f2b5f247245d8da841b5e0d810b5600c646c53ef39d0c9fb"

// TestPublish: chart w rejestrze pod wersją z commita, z obrazem po
// digeście i wersją wdrożenia; drugi bieg tego samego commita daje ten sam
// digest (powtarzalny artefakt).
func TestPublish(t *testing.T) {
	quiet(t)
	p := publisher(t, repo{})
	got, err := p.Publish(context.Background(), image, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.4.1-dev.3" || !strings.HasPrefix(got.Digest, "sha256:") {
		t.Errorf("publikacja: %+v", got)
	}
	again, err := p.Publish(context.Background(), image, "")
	if err != nil || again.Digest != got.Digest {
		t.Errorf("ponowna publikacja: %s, %v; pierwsza %s", again.Digest, err, got.Digest)
	}
	err = p.Verify(context.Background(), p.Repository+"@"+got.Digest, chart.Build{Version: "0.4.1-dev.3"})
	if cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("chart niezgodny z oczekiwaniem przyjęty: %v", err)
	}
}

// TestPublishRefuses: brudne drzewo i obraz spoza repozytorium charta.
func TestPublishRefuses(t *testing.T) {
	quiet(t)
	if _, err := publisher(t, repo{dirty: true}).Publish(context.Background(), image, ""); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("brudne drzewo: %v", err)
	}
	// Obraz z innego commita — chart opisałby go tagiem swojego.
	p := publisher(t, repo{})
	p.Images = images{"0123456789ab" + commit[12:]}
	if _, err := p.Publish(context.Background(), image, ""); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "zbudowano z commita") {
		t.Errorf("obraz z innego commita: %v", err)
	}
	other := "ghcr.io/ktos/inny@sha256:" + strings.Repeat("b", 64)
	_, err := publisher(t, repo{}).Publish(context.Background(), other, "")
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "spoza repozytorium") {
		t.Errorf("obraz z innego repozytorium: %v", err)
	}
}

// TestPackageIsPublished: archiwum zainstalowane w zadaniu cluster to ten sam
// artefakt, który zadanie chart publikuje; inny digest przetestowany —
// odmowa publikacji (decyzja 61).
func TestPackageIsPublished(t *testing.T) {
	quiet(t)
	p := publisher(t, repo{})
	archive, tested, err := p.Package(context.Background(), image, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(archive, "docfind-0.4.1-dev.3.tgz") {
		t.Errorf("archiwum: %s", archive)
	}
	got, err := p.Publish(context.Background(), image, tested)
	if err != nil || got.Digest != tested {
		t.Fatalf("publikacja przetestowanego: %+v, %v (przetestowany %s)", got, err, tested)
	}
	if _, err := p.Publish(context.Background(), image, "sha256:"+strings.Repeat("0", 64)); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "to nie chart przetestowany") {
		t.Errorf("inny chart przetestowany: %v", err)
	}
}
