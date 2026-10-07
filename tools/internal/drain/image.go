package drain

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// ProbeRepository — nazwa obrazu sondy. Obraz nie trafia do rejestru:
// w dev i ci budowany ze źródeł tego commita i importowany do węzłów.
const ProbeRepository = "docfind-drain-probe"

// ProbeUser — UID i GID sondy. Obraz nie ma /etc/passwd, a runAsNonRoot
// sprawdza tylko numeryczny UID, więc użytkownik jest liczbą (65532 —
// konwencja „nonroot" obrazów distroless).
const ProbeUser = 65532

// BuildProbeBinary kompiluje sondę dla architektury węzłów (tej samej co
// host, bo węzły k3d to kontenery na tym hoście): statycznie, bez cgo,
// z -trimpath, jak dft.
func BuildProbeBinary(ctx context.Context, r proc.Runner, toolsDir, outDir string) (string, error) {
	out := filepath.Join(outDir, "drain-probe")
	_, err := r.Run(ctx, proc.Cmd{
		Name: "go",
		Args: []string{"build", "-mod=readonly", "-trimpath", "-o", out, "./cmd/drain-probe"},
		Dir:  toolsDir,
		Env:  []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + runtime.GOARCH},
	})
	if err != nil {
		return "", fmt.Errorf("build sondy: %w", err)
	}
	return out, nil
}

// ProbeImage składa obraz sondy z jednej warstwy z binarką — bez Dockerfile,
// bez bazy (nic do przypinania i nic do łatania) i bez BuildKitu. Czasy
// w warstwie i konfiguracji są stałe, więc ta sama binarka daje ten sam
// digest: powtarzalność z konstrukcji, jak w decyzji 21.
func ProbeImage(binary string) (v1.Image, error) {
	data, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	epoch := time.Unix(0, 0)
	if err := tw.WriteHeader(&tar.Header{
		Name: "drain-probe", Mode: 0o555, Size: int64(len(data)), Typeflag: tar.TypeReg,
		ModTime: epoch, Uid: 0, Gid: 0, Format: tar.FormatPAX,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(data); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	layerBytes := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerBytes)), nil
	})
	if err != nil {
		return nil, err
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, err
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cfg = cfg.DeepCopy()
	cfg.OS = "linux"
	cfg.Architecture = runtime.GOARCH
	cfg.Created = v1.Time{Time: epoch}
	cfg.Config.Entrypoint = []string{"/drain-probe"}
	cfg.Config.User = fmt.Sprintf("%d:%d", ProbeUser, ProbeUser)
	return mutate.ConfigFile(img, cfg)
}

// ProbeTag zwraca tag obrazu z jego digestu: ta sama binarka — ten sam tag,
// więc ponowny import do węzłów nic nie zmienia.
func ProbeTag(img v1.Image) (string, error) {
	digest, err := img.Digest()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:local-%s", ProbeRepository, digest.Hex[:12]), nil
}

// ImportProbe zapisuje obraz do archiwum i importuje je do węzłów klastra
// k3d (`k3d image import --mode direct` — tryb domyślny przy Dockerze
// rootless zawodzi, decyzja 19).
func ImportProbe(ctx context.Context, r proc.Runner, img v1.Image, ref, cluster, tmp string) error {
	tag, err := name.NewTag(ref)
	if err != nil {
		return err
	}
	archive := filepath.Join(tmp, "drain-probe.tar")
	if err := tarball.WriteToFile(archive, tag, img); err != nil {
		return fmt.Errorf("archiwum sondy: %w", err)
	}
	if _, err := r.Run(ctx, proc.Cmd{Name: "k3d", Args: []string{"image", "import", archive, "--cluster", cluster, "--mode", "direct"}}); err != nil {
		return fmt.Errorf("import sondy do klastra %s: %w", cluster, err)
	}
	return nil
}
