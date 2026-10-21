package chartpub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/registry"
	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/core/platform"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// fetched — archiwa chartów jak z serwera charta: jeden katalog z Chart.yaml
// w wersji z ci/pins.env (albo wrongVersion).
type fetched struct {
	dir          string
	wrongVersion string
}

func (f fetched) Fetch(_ context.Context, c pins.Chart) (string, error) {
	version := c.Version
	if f.wrongVersion != "" {
		version = f.wrongVersion
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Kolejność wpisów stała: to samo przypięcie to te same bajty, jak plik
	// z pamięci podręcznej.
	for _, e := range []struct{ name, data string }{
		{c.Name + "/Chart.yaml", "apiVersion: v2\nname: " + c.Name + "\nversion: " + version + "\n"},
		{c.Name + "/values.yaml", "a: 1\n"},
	} {
		name, data := e.name, e.data
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return "", err
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(f.dir, c.Name+".tgz")
	return path, os.WriteFile(path, buf.Bytes(), 0o644)
}

// denied — rejestr bez poświadczeń dla pakietu prywatnego.
type denied struct{}

func (denied) Manifest(context.Context, string) ([]byte, string, error) {
	return nil, "", artifact.ErrDenied
}
func (denied) Blob(context.Context, string, string) ([]byte, error) { return nil, artifact.ErrDenied }

func testRegistry(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://") + "/mateuszf757"
}

func mirror(t *testing.T, f fetched, prefix string) Mirror {
	t.Helper()
	p, err := pins.Load("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	return Mirror{
		Pins: p, Charts: f, Registry: registry.Client{}, Public: registry.Client{Anonymous: true},
		Repository: func(pin string) string { return prefix + "/charts/platform/" + pin },
		Source:     "https://github.com/mateuszf757/Docfind",
	}
}

// TestMirror: każdy chart, którym zarządza Argo CD, trafia do lustra
// z digestem wyliczonym z archiwum; drugi bieg niczego nie wysyła.
func TestMirror(t *testing.T) {
	quiet(t)
	m := mirror(t, fetched{dir: t.TempDir()}, testRegistry(t))
	first, err := m.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != len(platform.Mirrored()) {
		t.Fatalf("w lustrze %d chartów, oczekiwano %d", len(first), len(platform.Mirrored()))
	}
	for _, s := range first {
		if !s.Pushed || !s.Public || s.Upstream == "" || !strings.HasSuffix(s.Repository, "/charts/platform/"+s.Pin) {
			t.Errorf("pierwszy bieg: %+v", s)
		}
		if s.Pin == pins.CoreDNS || s.Pin == pins.ArgoCD {
			t.Errorf("%s w lustrze — instaluje go dft przed Argo CD", s.Pin)
		}
	}
	second, err := m.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range second {
		if s.Pushed || s.Digest != first[i].Digest {
			t.Errorf("drugi bieg: %+v, pierwszy %+v", s, first[i])
		}
	}
}

func TestMirrorRefuses(t *testing.T) {
	quiet(t)
	t.Run("tag lustra z inną treścią", func(t *testing.T) {
		prefix := testRegistry(t)
		dir := t.TempDir()
		m := mirror(t, fetched{dir: dir}, prefix)
		// Pod tagiem z ci/pins.env stoi już inny manifest: to samo archiwum
		// z innym źródłem w adnotacji.
		pc := platform.Mirrored()[0]
		pinned, _ := m.Pins.Chart(pc.Pin)
		path, err := m.Charts.Fetch(context.Background(), pinned)
		if err != nil {
			t.Fatal(err)
		}
		archive, _ := os.ReadFile(path)
		other, err := chart.Mirror(archive, "https://inne.example.com/chart.tgz", m.Source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (registry.Client{}).PushManifest(context.Background(), m.Repository(pc.Pin)+":"+other.Version, other.Manifest, chart.ManifestMediaType,
			artifact.Blob{Data: other.Config, MediaType: chart.ConfigMediaType}, artifact.Blob{Data: other.Archive, MediaType: chart.LayerMediaType}); err != nil {
			t.Fatal(err)
		}
		_, err = m.Run(context.Background())
		if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "tag lustra nie zmienia treści") {
			t.Errorf("nadpisanie tagu lustra: %v", err)
		}
	})
	t.Run("wersja w Chart.yaml inna niż w ci/pins.env", func(t *testing.T) {
		_, err := mirror(t, fetched{dir: t.TempDir(), wrongVersion: "9.9.9"}, testRegistry(t)).Run(context.Background())
		if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "w ci/pins.env") {
			t.Errorf("rozjazd wersji: %v", err)
		}
	})
}

// TestMirrorNotPublic: pakiet prywatny to ostrzeżenie i Public=false, nie
// błąd — widoczność ustawia raz autor.
func TestMirrorNotPublic(t *testing.T) {
	quiet(t)
	m := mirror(t, fetched{dir: t.TempDir()}, testRegistry(t))
	m.Public = denied{}
	got, err := m.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got {
		if s.Public {
			t.Errorf("%s publiczny mimo odmowy", s.Repository)
		}
	}
}

// TestPlatformPublish: chart platformy pod wersją z commita, ten sam
// commit — ten sam digest i nic do wysłania; brudne drzewo — odmowa.
func TestPlatformPublish(t *testing.T) {
	quiet(t)
	p := PlatformPublisher{
		ChartDir: "../../../../deploy/charts/platform", Repo: repo{}, Registry: registry.Client{}, Public: registry.Client{Anonymous: true},
		Repository: testRegistry(t) + "/charts/docfind-platform", Source: "https://github.com/mateuszf757/Docfind",
	}
	first, err := p.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Publish(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != "0.4.1-dev.3" || !first.Pushed || second.Pushed || second.Digest != first.Digest || !first.Public {
		t.Errorf("publikacje: %+v, %+v", first, second)
	}
	raw, _, err := (registry.Client{}).Manifest(context.Background(), p.Repository+":0.4.1-dev.3")
	if err != nil || digestOf(raw) != first.Digest {
		t.Errorf("manifest pod tagiem: %v", err)
	}
	p.Repo = repo{dirty: true}
	if _, err := p.Publish(context.Background()); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("brudne drzewo: %v", err)
	}
}
