package reproducible

import (
	"archive/tar"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/random"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/build"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

type versions struct{ dirty bool }

func (versions) Version(context.Context) (string, error)        { return "0.4.1-dev.1+0123456", nil }
func (versions) Commit(context.Context) (string, error)         { return commit, nil }
func (versions) SourceDateEpoch(context.Context) (int64, error) { return 1790856000, nil }
func (v versions) RequireClean(context.Context) error {
	if v.dirty {
		return cli.Unmet("drzewo robocze jest brudne")
	}
	return nil
}

// images — build zapisuje kolejny obraz z listy; SaveArchive zapisuje go
// w układzie OCI, jak `docker save` z Dockera 25+.
type images struct {
	t       *testing.T
	built   []v1.Image
	next    int
	noCache []bool
}

func (i *images) Build(_ context.Context, req artifact.BuildRequest) (string, error) {
	i.noCache = append(i.noCache, req.NoCache)
	return "", nil
}

func (i *images) ReadFiles(context.Context, string, ...string) ([]byte, error) {
	return []byte(`{"version":"0.4.1-dev.1+0123456","commit":"` + commit + `"}`), nil
}

func (i *images) SaveArchive(_ context.Context, _, path string) (string, error) {
	img := i.built[i.next]
	i.next++
	dir := i.t.TempDir()
	p, err := layout.Write(dir, empty.Index)
	if err != nil {
		return "", err
	}
	if err := p.AppendImage(img); err != nil {
		return "", err
	}
	config, err := img.ConfigName()
	if err != nil {
		return "", err
	}
	return config.String(), tarDir(dir, path)
}

// tarDir pakuje katalog do archiwum tar ze ścieżkami względnymi.
func tarDir(dir, archive string) error {
	f, err := os.Create(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

type buildKit struct{}

func (buildKit) BuildKitVersion(context.Context) (string, error) { return "v0.33.0", nil }

func checker(t *testing.T, v versions, built ...v1.Image) (Checker, *images) {
	t.Helper()
	imgs := &images{t: t, built: built}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "services", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Checker{
		Builder:        build.Builder{Versions: v, Images: imgs},
		BuildKit:       buildKit{},
		Options:        build.Options{Root: root, Service: "api", Image: "ghcr.io/x/docfind-api"},
		PinnedBuildKit: "moby/buildkit:v0.33.0@sha256:" + strings.Repeat("a", 64),
		Out:            io.Discard,
	}, imgs
}

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

func image(t *testing.T) v1.Image {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// TestRun: build z pamięcią podręczną, potem od zera; ten sam obraz —
// powtarzalny, inny — wynik negatywny, brudne drzewo — odmowa przed
// buildem.
func TestRun(t *testing.T) {
	quiet(t)
	ctx := context.Background()
	img := image(t)
	c, imgs := checker(t, versions{}, img, img)
	if err := c.Run(ctx, t.TempDir()); err != nil {
		t.Fatalf("ten sam obraz: %v", err)
	}
	if len(imgs.noCache) != 2 || imgs.noCache[0] || !imgs.noCache[1] {
		t.Errorf("buildy (no-cache): %v — najpierw z pamięcią podręczną, potem od zera", imgs.noCache)
	}

	c, _ = checker(t, versions{}, image(t), image(t))
	if err := c.Run(ctx, t.TempDir()); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("różne obrazy: %v", err)
	}

	c, imgs = checker(t, versions{dirty: true}, img, img)
	if err := c.Run(ctx, t.TempDir()); cli.ExitCode(err) != cli.ExitUnmet || len(imgs.noCache) > 0 {
		t.Errorf("brudne drzewo: %v, buildy %v", err, imgs.noCache)
	}
}
