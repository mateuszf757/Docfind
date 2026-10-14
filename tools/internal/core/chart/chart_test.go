package chart

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

const repoChart = "../../../../deploy/charts/docfind"

func chartFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files, err := Files(repoChart)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

var build = Build{
	Version: "0.4.1-dev.3", AppVersion: "fc8dfc11099d", Tag: "fc8dfc11099d",
	Digest: "sha256:" + strings.Repeat("a", 64), DeployVersion: "0.4.1-dev.3+fc8dfc1",
}

func unpack(t *testing.T, tgz []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Errorf("nagłówek gzip z nazwą albo czasem: %q %v", gz.Name, gz.ModTime)
	}
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Uid != 0 || h.Gid != 0 || h.Mode != 0o644 {
			t.Errorf("%s: właściciel %d:%d, tryb %o", h.Name, h.Uid, h.Gid, h.Mode)
		}
		out[h.Name] = string(data)
	}
}

// TestPackageReproducible: te same pliki i ten sam czas commita — te same
// bajty; inny czas commita — inne (czas jest w archiwum, nie pomijany).
func TestPackageReproducible(t *testing.T) {
	files := chartFiles(t)
	epoch := time.Unix(1791800000, 0)
	a, err := Package("docfind", files, build, epoch)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Package("docfind", files, build, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("ten sam wejściowy chart, różne archiwa")
	}
	c, err := Package("docfind", files, build, epoch.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c) {
		t.Error("czas commita nie trafia do archiwum")
	}
}

// TestPackageBakesImage: chart z rejestru wdraża obraz po digeście i wersję
// wdrożenia z tego commita; komentarze w values.yaml zostają.
func TestPackageBakesImage(t *testing.T) {
	files := chartFiles(t)
	tgz, err := Package("docfind", files, build, time.Unix(1791800000, 0))
	if err != nil {
		t.Fatal(err)
	}
	got := unpack(t, tgz)
	if len(got) != len(files) {
		t.Errorf("plików %d, w charcie %d", len(got), len(files))
	}
	chart, values := got["docfind/Chart.yaml"], got["docfind/values.yaml"]
	for _, want := range []string{`version: "0.4.1-dev.3"`, `appVersion: "fc8dfc11099d"`} {
		if !strings.Contains(chart, want) {
			t.Errorf("Chart.yaml bez %s:\n%s", want, chart)
		}
	}
	for _, want := range []string{`tag: "fc8dfc11099d"`, `digest: "` + build.Digest + `"`, `version: "0.4.1-dev.3+fc8dfc1"`, "# Sekrety aplikacji jako pliki"} {
		if !strings.Contains(values, want) {
			t.Errorf("values.yaml bez %s", want)
		}
	}
	if got["docfind/templates/api-deployment.yaml"] != string(files["templates/api-deployment.yaml"]) {
		t.Error("szablon zmieniony przy pakowaniu")
	}
}

// TestPackageRefuses: wersja z metadanymi budowania (Helm zamieni „+" na
// „_" i Argo nie odczyta semver), zły digest, brudne drzewo, brakujący klucz.
func TestPackageRefuses(t *testing.T) {
	files := chartFiles(t)
	for name, mutate := range map[string]func(*Build, map[string][]byte){
		"metadane budowania": func(b *Build, _ map[string][]byte) { b.Version = "0.4.1-dev.3+fc8dfc1" },
		"zły digest":         func(b *Build, _ map[string][]byte) { b.Digest = "latest" },
		"brudne drzewo":      func(b *Build, _ map[string][]byte) { b.DeployVersion = "0.4.1-dev.3+fc8dfc1-dirty" },
		"brak klucza":        func(_ *Build, f map[string][]byte) { f["values.yaml"] = []byte("api:\n  replicas: 2\n") },
	} {
		t.Run(name, func(t *testing.T) {
			b, f := build, map[string][]byte{}
			for k, v := range files {
				f[k] = v
			}
			mutate(&b, f)
			if _, err := Package("docfind", f, b, time.Unix(0, 0)); err == nil {
				t.Error("przyjęte")
			}
		})
	}
	if v := ChartVersion("0.4.1-dev.3+fc8dfc1"); v != "0.4.1-dev.3" {
		t.Errorf("wersja charta: %s", v)
	}
}

// TestArtifact: manifest OCI w formacie Helma z czasem commita — ten sam
// commit to ten sam digest; konfiguracja to metadane z Chart.yaml.
func TestArtifact(t *testing.T) {
	files := chartFiles(t)
	epoch := time.Unix(1791800000, 0)
	a, err := Artifact("docfind", files, build, epoch, "https://github.com/mateuszf757/Docfind")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Artifact("docfind", files, build, epoch, "https://github.com/mateuszf757/Docfind")
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() != b.Digest() || !strings.HasPrefix(a.Digest(), "sha256:") {
		t.Errorf("digest niepowtarzalny: %s %s", a.Digest(), b.Digest())
	}
	var m struct {
		Config struct {
			MediaType, Digest string
		}
		Layers []struct {
			MediaType, Digest string
		}
		Annotations map[string]string
	}
	if err := json.Unmarshal(a.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.Config.MediaType != ConfigMediaType || len(m.Layers) != 1 || m.Layers[0].MediaType != LayerMediaType || m.Layers[0].Digest != sha(a.Archive) {
		t.Errorf("manifest: %s", a.Manifest)
	}
	if m.Annotations["org.opencontainers.image.created"] != "2026-10-12T10:13:20Z" || m.Annotations["org.opencontainers.image.version"] != "0.4.1-dev.3" {
		t.Errorf("adnotacje: %v", m.Annotations)
	}
	var meta map[string]any
	if err := json.Unmarshal(a.Config, &meta); err != nil || meta["name"] != "docfind" || meta["version"] != "0.4.1-dev.3" || meta["appVersion"] != "fc8dfc11099d" {
		t.Errorf("konfiguracja: %s, %v", a.Config, err)
	}
}

// TestRead: z archiwum wraca dokładnie to, co Package zapisał.
func TestRead(t *testing.T) {
	tgz, err := Package("docfind", chartFiles(t), build, time.Unix(1791800000, 0))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(tgz)
	if err != nil || got != build {
		t.Errorf("odczyt %+v, %v; oczekiwano %+v", got, err, build)
	}
}
