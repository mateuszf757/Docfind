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

	sigsyaml "sigs.k8s.io/yaml"
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

// TestPlain: chart platformy dostaje wersję i appVersion w Chart.yaml, a jego
// values.yaml zostaje bajt w bajt; ten sam commit — ten sam digest.
func TestPlain(t *testing.T) {
	files, err := Files("../../../../deploy/charts/platform")
	if err != nil {
		t.Fatal(err)
	}
	m := Meta{Version: "0.4.1-dev.3", AppVersion: "fc8dfc11099d"}
	epoch := time.Unix(1791800000, 0)
	a, err := Plain("docfind-platform", files, m, epoch, "https://github.com/mateuszf757/Docfind")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Plain("docfind-platform", files, m, epoch, "https://github.com/mateuszf757/Docfind")
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() != b.Digest() {
		t.Errorf("dwa pakowania, dwa digesty: %s, %s", a.Digest(), b.Digest())
	}
	got := unpack(t, a.Archive)
	var meta struct{ Version, AppVersion string }
	if err := sigsyaml.Unmarshal([]byte(got["docfind-platform/Chart.yaml"]), &meta); err != nil || meta.Version != "0.4.1-dev.3" || meta.AppVersion != "fc8dfc11099d" {
		t.Errorf("Chart.yaml: %+v, %v\n%s", meta, err, got["docfind-platform/Chart.yaml"])
	}
	if got["docfind-platform/values.yaml"] != string(files["values.yaml"]) {
		t.Error("values.yaml zmieniony przy pakowaniu charta bez obrazu")
	}
	var manifest struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(a.Manifest, &manifest); err != nil || manifest.Annotations["org.opencontainers.image.revision"] != "fc8dfc11099d" {
		t.Errorf("adnotacje: %v, %v", manifest.Annotations, err)
	}
	if _, err := Plain("docfind-platform", files, Meta{Version: "0.4.1+abc", AppVersion: "x"}, epoch, ""); err == nil {
		t.Error("wersja z metadanymi budowania przyjęta")
	}
}

// upstream — archiwum jak z serwera charta: wpisy w katalogu name, opcjonalnie
// z nagłówkiem globalnym PAX (git archive, część narzędzi go dopisuje).
func upstream(t *testing.T, globalHeader bool, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if globalHeader {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "x"}, Format: tar.FormatPAX}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"cert-manager/Chart.yaml", "cert-manager/values.yaml", "cert-manager/templates/a.yaml", "inny/Chart.yaml"} {
		data, ok := entries[name]
		if !ok {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestMirror: lustro nie zmienia archiwum — warstwa to te same bajty, więc
// digest warstwy to suma z ci/pins.env — a manifest jest deterministyczny,
// bez czasu publikacji.
func TestMirror(t *testing.T) {
	archive := upstream(t, true, map[string]string{
		"cert-manager/Chart.yaml":       "apiVersion: v2\nname: cert-manager\nversion: v1.21.2\nappVersion: v1.21.2\n",
		"cert-manager/values.yaml":      "a: 1\n",
		"cert-manager/templates/a.yaml": "kind: ConfigMap\n",
	})
	const from = "https://charts.jetstack.io/charts/cert-manager-v1.21.2.tgz"
	a, err := Mirror(archive, from, "https://github.com/mateuszf757/Docfind")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Mirror(archive, from, "https://github.com/mateuszf757/Docfind")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Archive, archive) || a.Digest() != b.Digest() || a.Name != "cert-manager" || a.Version != "v1.21.2" {
		t.Fatalf("lustro: %s %s, digesty %s %s, archiwum zmienione: %v", a.Name, a.Version, a.Digest(), b.Digest(), !bytes.Equal(a.Archive, archive))
	}
	var manifest struct {
		Config struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
		Layers []struct {
			MediaType, Digest string
		} `json:"layers"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(a.Manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Config.MediaType != ConfigMediaType || len(manifest.Layers) != 1 || manifest.Layers[0].MediaType != LayerMediaType || manifest.Layers[0].Digest != sha(archive) {
		t.Errorf("manifest: %+v", manifest)
	}
	if manifest.Annotations[UpstreamAnnotation] != from || manifest.Annotations["org.opencontainers.image.created"] != "" {
		t.Errorf("adnotacje: %v", manifest.Annotations)
	}
	var config map[string]any
	if err := json.Unmarshal(a.Config, &config); err != nil || config["name"] != "cert-manager" {
		t.Errorf("konfiguracja: %s, %v", a.Config, err)
	}
}

func TestMirrorRefuses(t *testing.T) {
	for name, entries := range map[string]map[string]string{
		// Argo CD rozpakowuje chart z OCI tylko z jednym katalogiem.
		"dwa katalogi":   {"cert-manager/Chart.yaml": "name: cert-manager\nversion: v1\n", "inny/Chart.yaml": "name: inny\nversion: v1\n"},
		"bez Chart.yaml": {"cert-manager/values.yaml": "a: 1\n"},
		"bez wersji":     {"cert-manager/Chart.yaml": "name: cert-manager\n"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Mirror(upstream(t, false, entries), "https://x", ""); err == nil {
				t.Error("przyjęte")
			}
		})
	}
}
