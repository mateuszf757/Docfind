package ociarchive

import (
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

func TestSingleBinaryIsReproducible(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(bin, []byte("binarka"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := BinaryConfig{Path: "/probe", User: 65532, Arch: "amd64"}
	first, err := SingleBinary(bin, cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SingleBinary(bin, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := first.Digest()
	b, _ := second.Digest()
	if a == "" || a != b {
		t.Errorf("ta sama binarka, różne obrazy: %s ≠ %s", a, b)
	}
	file, _ := first.img.ConfigFile()
	if file.Config.User != "65532:65532" || len(file.Config.Entrypoint) != 1 || file.Config.Entrypoint[0] != "/probe" || file.Architecture != "amd64" {
		t.Errorf("konfiguracja obrazu: %+v", file)
	}
}

// TestOpenWriteKeepsImage: archiwum z nowym tagiem niesie ten sam obraz
// (konfigurację i etykiety) — zmienia się tylko tag.
func TestOpenWriteKeepsImage(t *testing.T) {
	img, err := random.Image(128, 1)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Config(img, v1.Config{Labels: map[string]string{"org.opencontainers.image.version": "0.4.0"}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.tar")
	if err := (Image{img: img}).Write("example.invalid/app:stary", src); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	retagged := filepath.Join(dir, "retagged.tar")
	if err := opened.Write("example.invalid/app:local-abc", retagged); err != nil {
		t.Fatal(err)
	}
	again, err := Open(retagged)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := opened.Config()
	got, _ := again.Config()
	labels, _ := again.Labels()
	if got != want || labels["org.opencontainers.image.version"] != "0.4.0" {
		t.Errorf("po zmianie tagu: konfiguracja %s (było %s), etykiety %v", got, want, labels)
	}
	if _, err := Open(filepath.Join(dir, "brak.tar")); err == nil {
		t.Error("brak archiwum przyjęty")
	}
}
