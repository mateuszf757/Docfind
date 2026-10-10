package ociarchive

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Image — obraz z archiwum albo złożony w pamięci. Typy go-containerregistry
// zostają w tym pakiecie: etapy widzą digesty, etykiety i archiwa.
type Image struct {
	img v1.Image
}

// Open czyta obraz z archiwum `docker save` (jeden obraz).
func Open(archive string) (Image, error) {
	img, err := tarball.ImageFromPath(archive, nil)
	if err != nil {
		return Image{}, fmt.Errorf("archiwum %s: %w", archive, err)
	}
	return Image{img: img}, nil
}

// Config zwraca digest konfiguracji obrazu — tożsamość zawartości
// (diff_ids warstw), niezależną od kompresji i typu manifestu.
func (i Image) Config() (string, error) {
	h, err := i.img.ConfigName()
	if err != nil {
		return "", err
	}
	return h.String(), nil
}

// Digest zwraca digest manifestu obrazu.
func (i Image) Digest() (string, error) {
	h, err := i.img.Digest()
	if err != nil {
		return "", err
	}
	return h.String(), nil
}

// Labels zwraca etykiety z konfiguracji obrazu.
func (i Image) Labels() (map[string]string, error) {
	f, err := i.img.ConfigFile()
	if err != nil {
		return nil, err
	}
	return f.Config.Labels, nil
}

// Write zapisuje obraz jako archiwum `docker save` z tagiem ref — do
// importu do węzłów klastra. Obraz zostaje ten sam, zmienia się tylko tag.
func (i Image) Write(ref, archive string) error {
	tag, err := name.NewTag(ref)
	if err != nil {
		return err
	}
	if err := tarball.WriteToFile(archive, tag, i.img); err != nil {
		return fmt.Errorf("archiwum %s: %w", archive, err)
	}
	return nil
}

// BinaryConfig — konfiguracja obrazu z jedną binarką.
type BinaryConfig struct {
	// Path — ścieżka binarki w obrazie, zarazem ENTRYPOINT.
	Path string
	// User — numeryczny UID i GID: obraz nie ma /etc/passwd, a runAsNonRoot
	// sprawdza tylko numer.
	User int
	// Arch — architektura węzłów (GOARCH).
	Arch string
}

// SingleBinary składa obraz z jednej warstwy z binarką — bez Dockerfile,
// bez bazy (nic do przypinania i nic do łatania) i bez BuildKitu. Czasy
// w warstwie i konfiguracji są stałe, więc ta sama binarka daje ten sam
// digest: powtarzalność z konstrukcji, jak w decyzji 21.
func SingleBinary(binary string, cfg BinaryConfig) (Image, error) {
	data, err := os.ReadFile(binary)
	if err != nil {
		return Image{}, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	epoch := time.Unix(0, 0)
	if err := tw.WriteHeader(&tar.Header{
		Name: path.Base(cfg.Path), Mode: 0o555, Size: int64(len(data)), Typeflag: tar.TypeReg,
		ModTime: epoch, Uid: 0, Gid: 0, Format: tar.FormatPAX,
	}); err != nil {
		return Image{}, err
	}
	if _, err := tw.Write(data); err != nil {
		return Image{}, err
	}
	if err := tw.Close(); err != nil {
		return Image{}, err
	}
	layerBytes := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerBytes)), nil
	})
	if err != nil {
		return Image{}, err
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return Image{}, err
	}
	file, err := img.ConfigFile()
	if err != nil {
		return Image{}, err
	}
	file = file.DeepCopy()
	file.OS = "linux"
	file.Architecture = cfg.Arch
	file.Created = v1.Time{Time: epoch}
	file.Config.Entrypoint = []string{cfg.Path}
	file.Config.User = fmt.Sprintf("%d:%d", cfg.User, cfg.User)
	img, err = mutate.ConfigFile(img, file)
	if err != nil {
		return Image{}, err
	}
	return Image{img: img}, nil
}
