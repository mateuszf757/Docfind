// Package chartpub publikuje chart aplikacji w rejestrze OCI (decyzja 45):
// wersja z tożsamości commita, w wartościach obraz z tego samego commita
// po digeście. Argo CD śledzi zakres wersji tego repozytorium, więc chart to
// jedyne, co CI musi opublikować, żeby środowisko dostało nowy build — bez
// zapisu do gita.
//
// Po wysłaniu chart jest pobierany z powrotem po digeście i sprawdzany: to,
// co jest w rejestrze, a nie to, co miało tam trafić.
package chartpub

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Repo — tożsamość commita (adapters/git).
type Repo interface {
	RequireClean(ctx context.Context) error
	Version(ctx context.Context) (string, error)
	Commit(ctx context.Context) (string, error)
	SourceDateEpoch(ctx context.Context) (int64, error)
}

// Registry — rejestr OCI (adapters/registry).
type Registry interface {
	PushManifest(ctx context.Context, ref string, manifest []byte, mediaType string, blobs ...artifact.Blob) (string, error)
	Manifest(ctx context.Context, ref string) ([]byte, string, error)
	Blob(ctx context.Context, repository, digest string) ([]byte, error)
}

// Images — opublikowane obrazy (adapters/registry).
type Images interface {
	Inspect(ctx context.Context, ref string) (artifact.Publication, error)
}

// Publisher — chart z katalogu ChartDir do repozytorium Repository.
type Publisher struct {
	ChartDir   string
	Repo       Repo
	Registry   Registry
	Images     Images
	Repository string
	// Source — repozytorium kodu (adnotacja wiążąca pakiet z repozytorium).
	Source string
}

// Published — opublikowany chart.
type Published struct {
	Repository string `json:"repository"`
	Version    string `json:"version"`
	Digest     string `json:"digest"`
	Image      string `json:"image"`
}

// Build składa artefakt dla obrazu image (repozytorium@sha256:…) zbudowanego
// z bieżącego commita. Brudne drzewo — odmowa: opublikowany chart ma
// odpowiadać commitowi.
func (p Publisher) Build(ctx context.Context, image string) (chart.OCI, chart.Build, error) {
	if err := p.Repo.RequireClean(ctx); err != nil {
		return chart.OCI{}, chart.Build{}, err
	}
	repository, digest, err := artifact.ParseRef(image)
	if err != nil {
		return chart.OCI{}, chart.Build{}, err
	}
	files, err := chart.Files(p.ChartDir)
	if err != nil {
		return chart.OCI{}, chart.Build{}, err
	}
	// Chart ma ciągnąć obraz z tego repozytorium, które podaje wywołujący —
	// digest z innego repozytorium w tym charcie nie istnieje.
	var values struct {
		API struct {
			Image struct {
				Repository string `json:"repository"`
			} `json:"image"`
		} `json:"api"`
	}
	if err := yaml.Unmarshal(files["values.yaml"], &values); err != nil {
		return chart.OCI{}, chart.Build{}, fmt.Errorf("values.yaml: %w", err)
	}
	if values.API.Image.Repository != repository {
		return chart.OCI{}, chart.Build{}, cli.Unmet("obraz %s spoza repozytorium charta (%s)", image, values.API.Image.Repository)
	}
	version, err := p.Repo.Version(ctx)
	if err != nil {
		return chart.OCI{}, chart.Build{}, err
	}
	commit, err := p.Repo.Commit(ctx)
	if err != nil {
		return chart.OCI{}, chart.Build{}, err
	}
	epoch, err := p.Repo.SourceDateEpoch(ctx)
	if err != nil {
		return chart.OCI{}, chart.Build{}, err
	}
	// Chart opisuje obraz tagiem i wersją swojego commita, więc obraz musi
	// z niego pochodzić — etykieta revision obrazu w rejestrze, nie słowo
	// wywołującego.
	pub, err := p.Images.Inspect(ctx, image)
	if err != nil {
		return chart.OCI{}, chart.Build{}, err
	}
	if pub.Commit != commit {
		return chart.OCI{}, chart.Build{}, cli.Unmet("obraz %s zbudowano z commita %.12s, a chart powstaje z %.12s", image, pub.Commit, commit)
	}
	tag := artifact.CommitTag(commit)
	b := chart.Build{Version: chart.ChartVersion(version), AppVersion: tag, Tag: tag, Digest: digest, DeployVersion: version}
	oci, err := chart.Artifact(filepath.Base(p.ChartDir), files, b, time.Unix(epoch, 0), p.Source)
	return oci, b, err
}

// Publish publikuje chart dla obrazu image i sprawdza go w rejestrze.
func (p Publisher) Publish(ctx context.Context, image string) (Published, error) {
	oci, b, err := p.Build(ctx, image)
	if err != nil {
		return Published{}, err
	}
	digest, err := p.Registry.PushManifest(ctx, p.Repository+":"+oci.Version, oci.Manifest, chart.ManifestMediaType,
		artifact.Blob{Data: oci.Config, MediaType: chart.ConfigMediaType}, artifact.Blob{Data: oci.Archive, MediaType: chart.LayerMediaType})
	if err != nil {
		return Published{}, err
	}
	if digest != oci.Digest() {
		return Published{}, fmt.Errorf("rejestr przyjął manifest %s, złożono %s", digest, oci.Digest())
	}
	cli.Step("chart %s:%s opublikowany: %s", p.Repository, oci.Version, digest)
	if err := p.Verify(ctx, p.Repository+"@"+digest, b); err != nil {
		return Published{}, err
	}
	return Published{Repository: p.Repository, Version: oci.Version, Digest: digest, Image: image}, nil
}

// Verify pobiera chart ref (repozytorium@digest) z rejestru i sprawdza, że
// to chart Helma z wersją i obrazem z want.
func (p Publisher) Verify(ctx context.Context, ref string, want chart.Build) error {
	raw, mediaType, err := p.Registry.Manifest(ctx, ref)
	if err != nil {
		return err
	}
	var m struct {
		Config struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("manifest %s: %w", ref, err)
	}
	if mediaType != chart.ManifestMediaType || m.Config.MediaType != chart.ConfigMediaType || len(m.Layers) != 1 || m.Layers[0].MediaType != chart.LayerMediaType {
		return cli.Unmet("%s nie jest chartem Helma (manifest %s, konfiguracja %s, warstw %d)", ref, mediaType, m.Config.MediaType, len(m.Layers))
	}
	archive, err := p.Registry.Blob(ctx, p.Repository, m.Layers[0].Digest)
	if err != nil {
		return err
	}
	got, err := chart.Read(archive)
	if err != nil {
		return err
	}
	if got != want {
		return cli.Unmet("chart %s w rejestrze: %+v, oczekiwano %+v", ref, got, want)
	}
	cli.Step("chart w rejestrze: wersja %s, obraz %s@%s, wersja wdrożenia %s", got.Version, got.Tag, got.Digest, got.DeployVersion)
	return nil
}
