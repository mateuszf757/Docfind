package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/ociarchive"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Build buduje obraz przez buildx według żądania etapu. Przy publikacji
// zwraca digest indeksu z metadanych BuildKitu.
//
// Wyjście kopii lokalnej zależy od sterownika BuildKitu. Sterownik docker
// (lokalny demon z magazynem containerd) eksportuje prosto do demona
// i odrzuca rewrite-timestamp razem z domyślnym rozpakowaniem warstw —
// rozpakowanie nastąpi przy pierwszym uruchomieniu. Sterownik
// docker-container (CI) nie ma dostępu do magazynu demona, więc obraz wraca
// jako archiwum i jest ładowany — dopiero wtedy da się go uruchomić.
func (c Client) Build(ctx context.Context, req artifact.BuildRequest) (string, error) {
	args := []string{"buildx", "build"}
	if req.Target != "" {
		args = append(args, "--target", req.Target)
	}
	for _, bc := range req.Contexts {
		args = append(args, "--build-context", bc.Name+"="+bc.Dir)
	}
	for _, a := range req.Args {
		args = append(args, "--build-arg", a.Name+"="+a.Value)
	}
	var metadata string
	switch {
	case req.CacheOnly:
		// Postęp w postaci tekstowej niezależnie od terminala: etap testów
		// czyta z logu podsumowanie pytesta.
		args = append(args, "--output", "type=cacheonly", "--progress", "plain")
	case req.Publish == nil:
		builder, err := c.CurrentBuilder(ctx)
		if err != nil {
			return "", err
		}
		output := "type=docker,rewrite-timestamp=true"
		if builder.Driver == "docker" {
			output = "type=image,rewrite-timestamp=true,unpack=false"
		}
		args = append(args, "--provenance=false", "--output", output)
	default:
		if req.Publish.SBOMGenerator == "" {
			return "", errors.New("publikacja bez generatora SBOM — brak DF_SBOM_GENERATOR_IMAGE")
		}
		tmp, err := os.MkdirTemp("", "dft-push-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmp)
		metadata = filepath.Join(tmp, "push.json")
		// Prosto z BuildKitu (eksporter image z push=true): przez `docker
		// push` z magazynu demona atestacja ginęła po drodze (decyzja 21).
		// mode=max zapisuje pełną definicję builda — build argi nie są
		// sekretami. SBOM z generatora przypiętego digestem (decyzja 37).
		args = append(args, "--provenance=mode=max", "--attest", "type=sbom,generator="+req.Publish.SBOMGenerator,
			"--output", "type=image,push=true,rewrite-timestamp=true,unpack=false", "--metadata-file", metadata)
	}
	for _, t := range req.Tags {
		args = append(args, "--tag", t)
	}
	if req.NoCache {
		args = append(args, "--no-cache")
	}
	args = append(args, req.ContextDir)
	if _, err := c.Runner.Run(ctx, proc.Cmd{Name: "docker", Args: args, Stdout: req.Out, Stderr: req.Err}); err != nil {
		return "", err
	}
	if metadata == "" {
		return "", nil
	}
	return pushedDigest(metadata)
}

// pushedDigest czyta digest publikacji z --metadata-file BuildKitu.
func pushedDigest(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("metadane publikacji: %w", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("metadane publikacji: %w", err)
	}
	digest, ok := meta["containerimage.digest"].(string)
	if !ok || digest == "" {
		return "", errors.New("metadane publikacji: brak containerimage.digest")
	}
	return digest, nil
}

// SaveArchive zapisuje obraz do archiwum (`docker save`) i zwraca digest
// jego konfiguracji — tożsamość zawartości. Z archiwum, bo `docker image
// inspect .Id` znaczy co innego w magazynie klasycznym (konfiguracja)
// i w magazynie containerd (manifest).
func (c Client) SaveArchive(ctx context.Context, ref, path string) (string, error) {
	if err := c.Save(ctx, ref, path); err != nil {
		return "", err
	}
	return ociarchive.ConfigDigestFromSave(path)
}
