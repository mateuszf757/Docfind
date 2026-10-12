package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
)

// rawManifest — manifest wysyłany bajt w bajt, jak został złożony: digest
// w rejestrze to digest tych bajtów, bez ponownej serializacji.
type rawManifest struct {
	raw []byte
	mt  types.MediaType
}

func (m rawManifest) RawManifest() ([]byte, error)        { return m.raw, nil }
func (m rawManifest) MediaType() (types.MediaType, error) { return m.mt, nil }

// PushManifest wysyła bloby i manifest pod ref (repozytorium:tag) i zwraca
// digest manifestu. Artefakt inny niż obraz (chart Helma) — stąd bloby
// i manifest wprost, a nie v1.Image.
func (c Client) PushManifest(ctx context.Context, ref string, manifest []byte, mediaType string, blobs ...artifact.Blob) (string, error) {
	tag, err := name.NewTag(ref)
	if err != nil {
		return "", fmt.Errorf("referencja %q: %w", ref, err)
	}
	for _, b := range blobs {
		if err := remote.WriteLayer(tag.Context(), static.NewLayer(b.Data, types.MediaType(b.MediaType)), c.opts(ctx)...); err != nil {
			return "", fmt.Errorf("wysyłka %s do %s: %w", b.MediaType, tag.Context(), err)
		}
	}
	if err := remote.Put(tag, rawManifest{manifest, types.MediaType(mediaType)}, c.opts(ctx)...); err != nil {
		return "", fmt.Errorf("manifest %s: %w", ref, err)
	}
	sum := sha256.Sum256(manifest)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Manifest zwraca surowy manifest pod ref i jego typ mediów.
func (c Client) Manifest(ctx context.Context, ref string) ([]byte, string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, "", fmt.Errorf("referencja %q: %w", ref, err)
	}
	desc, err := remote.Get(r, c.opts(ctx)...)
	if err != nil {
		return nil, "", fmt.Errorf("manifest %s: %w", ref, err)
	}
	return desc.Manifest, string(desc.MediaType), nil
}

// Blob zwraca zawartość bloba digest z repozytorium repository.
func (c Client) Blob(ctx context.Context, repository, digest string) ([]byte, error) {
	d, err := name.NewDigest(repository + "@" + digest)
	if err != nil {
		return nil, err
	}
	layer, err := remote.Layer(d, c.opts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", d, err)
	}
	rc, err := layer.Compressed()
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", d, err)
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Tags zwraca tagi repozytorium (repozytorium bez tagu i digestu) — do
// rozwiązania zakresu wersji charta tak, jak robi to Argo CD.
func (c Client) Tags(ctx context.Context, repository string) ([]string, error) {
	repo, err := name.NewRepository(repository)
	if err != nil {
		return nil, fmt.Errorf("repozytorium %q: %w", repository, err)
	}
	tags, err := remote.List(repo, c.opts(ctx)...)
	if err != nil {
		return nil, fmt.Errorf("tagi %s: %w", repository, err)
	}
	return tags, nil
}
