package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// layout buduje syntetyczny układ OCI, taki jak z `docker save`.
type layout struct {
	t   *testing.T
	dir string
}

func newLayout(t *testing.T) *layout {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &layout{t: t, dir: dir}
}

func (l *layout) blob(content []byte) string {
	l.t.Helper()
	sum := sha256.Sum256(content)
	hexSum := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(l.dir, "blobs", "sha256", hexSum), content, 0o644); err != nil {
		l.t.Fatal(err)
	}
	return "sha256:" + hexSum
}

func (l *layout) jsonBlob(v any) string {
	l.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		l.t.Fatal(err)
	}
	return l.blob(data)
}

// layer zwraca digest warstwy (tar skompresowany gzipem) z podanymi plikami.
func (l *layout) layer(files map[string]string) string {
	l.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Kolejność wpisów musi być stała — iteracja po mapie w Go jest losowa,
	// a ta sama zawartość w innej kolejności to inna warstwa.
	for _, name := range slices.Sorted(maps.Keys(files)) {
		content := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			l.t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			l.t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		l.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		l.t.Fatal(err)
	}
	return l.blob(buf.Bytes())
}

// image zapisuje obraz z jedną warstwą i opcjonalną atestacją; zwraca digest
// manifestu obrazu.
func (l *layout) image(files map[string]string, attestation bool) string {
	l.t.Helper()
	layer := l.layer(files)
	config := l.jsonBlob(map[string]any{"os": "linux", "architecture": "amd64", "rootfs": map[string]any{"diff_ids": []string{layer}}})
	manifest := l.jsonBlob(map[string]any{
		"schemaVersion": 2,
		"config":        map[string]string{"digest": config},
		"layers":        []map[string]string{{"digest": layer}},
	})
	top := map[string]any{"digest": manifest}
	if attestation {
		att := l.jsonBlob(map[string]any{"schemaVersion": 2, "config": map[string]string{"digest": config}, "layers": []any{}})
		index := l.jsonBlob(map[string]any{"manifests": []map[string]any{
			{"digest": manifest, "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
			{"digest": att, "annotations": map[string]string{
				"vnd.docker.reference.type":   AttestationKey,
				"vnd.docker.reference.digest": manifest,
			}},
		}})
		top = map[string]any{"digest": index}
	}
	data, _ := json.Marshal(map[string]any{"manifests": []any{top}})
	if err := os.WriteFile(filepath.Join(l.dir, "index.json"), data, 0o644); err != nil {
		l.t.Fatal(err)
	}
	return manifest
}

func TestCompare(t *testing.T) {
	files := map[string]string{"app/version.json": `{"version":"1.0.0"}`, "app/main.py": "print()"}
	changed := map[string]string{"app/version.json": `{"version":"1.0.1"}`, "app/main.py": "print()"}
	tests := []struct {
		name             string
		a, b             map[string]string
		attA, attB       bool
		want             bool
		wantOut, notWant string
	}{
		{name: "identyczne", a: files, b: files, want: true, wantOut: "OK       linux/amd64"},
		{name: "różny plik w warstwie", a: files, b: changed, want: false, wantOut: "      app/version.json", notWant: "app/main.py"},
		{name: "atestacja tylko po jednej stronie", a: files, b: files, attB: true, want: true, wantOut: "obecna tylko w jednym obrazie — nie wpływa na werdykt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := newLayout(t), newLayout(t)
			a.image(tt.a, tt.attA)
			b.image(tt.b, tt.attB)
			var out bytes.Buffer
			got, err := Compare(Layout(a.dir), Layout(b.dir), &out)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("Compare = %v, oczekiwano %v\n%s", got, tt.want, out.String())
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("wyjście nie zawiera %q:\n%s", tt.wantOut, out.String())
			}
			if tt.notWant != "" && strings.Contains(out.String(), tt.notWant) {
				t.Errorf("wyjście zawiera %q, a ten plik się nie zmienił:\n%s", tt.notWant, out.String())
			}
		})
	}
}

func TestCompareBrokenLayout(t *testing.T) {
	a := newLayout(t)
	a.image(map[string]string{"x": "y"}, false)
	_, err := Compare(Layout(a.dir), Layout(t.TempDir()), &bytes.Buffer{})
	if err == nil {
		t.Fatal("brak index.json nie dał błędu — awaria porównania wyglądałaby jak wynik")
	}
}

func saveArchive(t *testing.T, entries map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "image.tar")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, content := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigDigestFromSave(t *testing.T) {
	hexSum := strings.Repeat("ab", 32)
	for _, config := range []string{"blobs/sha256/" + hexSum, hexSum + ".json"} {
		archive := saveArchive(t, map[string]string{"manifest.json": `[{"Config":"` + config + `"}]`})
		got, err := ConfigDigestFromSave(archive)
		if err != nil {
			t.Fatal(err)
		}
		if got != "sha256:"+hexSum {
			t.Errorf("Config %q: %q", config, got)
		}
	}
	if _, err := ConfigDigestFromSave(saveArchive(t, map[string]string{"index.json": "{}"})); err == nil {
		t.Error("archiwum bez manifest.json przyjęte")
	}
}

func TestExtractRejectsEscapingPaths(t *testing.T) {
	archive := saveArchive(t, map[string]string{"../poza.txt": "x"})
	if err := Extract(archive, t.TempDir()); err == nil {
		t.Fatal("ścieżka wychodząca poza katalog przyjęta")
	}
	dir := t.TempDir()
	archive = saveArchive(t, map[string]string{"blobs/sha256/abc": "zawartość"})
	if err := Extract(archive, dir); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "blobs", "sha256", "abc")); err != nil || string(data) != "zawartość" {
		t.Errorf("rozpakowany plik: %q, %v", data, err)
	}
}
