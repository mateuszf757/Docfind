package chartpub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/core/platform"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Charts — przypięte charty platformy (adapters/helm): ścieżka archiwum
// sprawdzonego sumą z ci/pins.env.
type Charts interface {
	Fetch(ctx context.Context, chart pins.Chart) (string, error)
}

// Mirror — lustro przypiętych chartów platformy w rejestrze (decyzja 80):
// te same bajty, które sprawdza suma z ci/pins.env, w manifeście OCI, który
// wynika z nich deterministycznie. Argo CD pobiera je stąd po digeście, a nie
// z serwera charta po wersji.
type Mirror struct {
	Pins     pins.Pins
	Charts   Charts
	Registry Registry
	// Public — ten sam rejestr bez poświadczeń: tak pobiera Argo CD.
	Public Reader
	// Repository — repozytorium lustra charta o przypięciu pin.
	Repository func(pin string) string
	// Source — repozytorium kodu (adnotacja wiążąca pakiet z repozytorium).
	Source string
}

// Stored — chart w rejestrze: lustro albo chart platformy.
type Stored struct {
	Pin        string `json:"pin,omitempty"`
	Repository string `json:"repository"`
	Version    string `json:"version"`
	Digest     string `json:"digest"`
	// Upstream — skąd pochodzi archiwum (adres z ci/pins.env).
	Upstream string `json:"upstream,omitempty"`
	// Pushed — wysłany w tym biegu; fałsz — był już w rejestrze z tym digestem.
	Pushed bool `json:"pushed"`
	// Public — pobieralny bez poświadczeń.
	Public bool `json:"public"`
}

// build składa artefakt lustra przypiętego charta pc z archiwum sprawdzonego
// sumą z ci/pins.env.
func (m Mirror) build(ctx context.Context, pc platform.Component) (chart.OCI, pins.Chart, error) {
	pinned, err := m.Pins.Chart(pc.Pin)
	if err != nil {
		return chart.OCI{}, pins.Chart{}, err
	}
	path, err := m.Charts.Fetch(ctx, pinned)
	if err != nil {
		return chart.OCI{}, pins.Chart{}, err
	}
	archive, err := os.ReadFile(path)
	if err != nil {
		return chart.OCI{}, pins.Chart{}, err
	}
	oci, err := chart.Mirror(archive, pinned.Source, m.Source)
	if err != nil {
		return chart.OCI{}, pins.Chart{}, fmt.Errorf("%s: %w", pc.Label, err)
	}
	if oci.Version != pinned.Version {
		return chart.OCI{}, pins.Chart{}, cli.Unmet("%s: wersja w Chart.yaml %s, a w ci/pins.env %s", pc.Label, oci.Version, pinned.Version)
	}
	return oci, pinned, nil
}

// Digests zwraca digesty lustra po przypięciu, wyliczone z archiwów — bez
// pytania rejestru: manifest lustra wynika z archiwum deterministycznie.
func (m Mirror) Digests(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for _, pc := range platform.Mirrored() {
		oci, _, err := m.build(ctx, pc)
		if err != nil {
			return nil, err
		}
		out[pc.Pin] = oci.Digest()
	}
	return out, nil
}

// Run wysyła do lustra każdy chart, którym zarządza Argo CD, a którego tam
// nie ma. Tag lustra nie zmienia treści: ten sam tag z innym manifestem to
// odmowa — nowa treść to nowe przypięcie w ci/pins.env, z nową wersją.
func (m Mirror) Run(ctx context.Context) ([]Stored, error) {
	var out []Stored
	for _, pc := range platform.Mirrored() {
		oci, pinned, err := m.build(ctx, pc)
		if err != nil {
			return out, err
		}
		s, err := put(ctx, m.Registry, m.Repository(pc.Pin), oci, true)
		if err != nil {
			return out, err
		}
		s.Pin, s.Upstream = pc.Pin, pinned.Source
		if s.Public, err = public(ctx, m.Public, s); err != nil {
			return out, err
		}
		out = append(out, s)
	}
	return out, nil
}

// PlatformPublisher — chart platformy (deploy/charts/platform) z bieżącego
// commita (decyzja 80): wersja i appVersion jak w charcie aplikacji,
// wartości bez zmian — środowisko podaje je przy instalacji.
type PlatformPublisher struct {
	ChartDir   string
	Repo       Repo
	Registry   Registry
	Public     Reader
	Repository string
	Source     string
}

// PlatformChart — nazwa charta platformy w Chart.yaml i w archiwum.
const PlatformChart = "docfind-platform"

// Build składa chart platformy z bieżącego commita; brudne drzewo — odmowa.
func (p PlatformPublisher) Build(ctx context.Context) (chart.OCI, error) {
	if err := p.Repo.RequireClean(ctx); err != nil {
		return chart.OCI{}, err
	}
	files, err := chart.Files(p.ChartDir)
	if err != nil {
		return chart.OCI{}, err
	}
	version, err := p.Repo.Version(ctx)
	if err != nil {
		return chart.OCI{}, err
	}
	commit, err := p.Repo.Commit(ctx)
	if err != nil {
		return chart.OCI{}, err
	}
	epoch, err := p.Repo.SourceDateEpoch(ctx)
	if err != nil {
		return chart.OCI{}, err
	}
	m := chart.Meta{Version: chart.ChartVersion(version), AppVersion: artifact.CommitTag(commit)}
	return chart.Plain(PlatformChart, files, m, time.Unix(epoch, 0), p.Source)
}

// Publish publikuje chart platformy i sprawdza go w rejestrze. Ten sam
// commit to ten sam digest (pakowanie powtarzalne), więc ponowny bieg
// niczego nie zmienia.
func (p PlatformPublisher) Publish(ctx context.Context) (Stored, error) {
	oci, err := p.Build(ctx)
	if err != nil {
		return Stored{}, err
	}
	s, err := put(ctx, p.Registry, p.Repository, oci, false)
	if err != nil {
		return Stored{}, err
	}
	s.Public, err = public(ctx, p.Public, s)
	return s, err
}

// put wysyła artefakt pod repository:wersja, jeśli go tam nie ma, i sprawdza
// w rejestrze manifest i warstwę po digeście. immutable — istniejący tag
// z innym manifestem to odmowa (lustro); inaczej nadpisanie jest błędem
// powtarzalności, nie zmianą treści, i też jest odmową.
func put(ctx context.Context, reg Registry, repository string, oci chart.OCI, immutable bool) (Stored, error) {
	ref := repository + ":" + oci.Version
	s := Stored{Repository: repository, Version: oci.Version, Digest: oci.Digest()}
	raw, _, err := reg.Manifest(ctx, ref)
	switch {
	case err == nil && digestOf(raw) == oci.Digest():
		cli.Step("%s: już w rejestrze (%s)", ref, s.Digest)
	case err == nil && immutable:
		return s, cli.Unmet("%s: w lustrze jest inny manifest (%s) niż złożony z archiwum z ci/pins.env (%s) — tag lustra nie zmienia treści; nowa treść to nowe przypięcie", ref, digestOf(raw), oci.Digest())
	case err == nil:
		return s, cli.Unmet("%s: w rejestrze jest inny manifest (%s) niż złożony z tego commita (%s) — pakowanie miało być powtarzalne", ref, digestOf(raw), oci.Digest())
	case errors.Is(err, artifact.ErrNotFound):
		digest, err := reg.PushManifest(ctx, ref, oci.Manifest, chart.ManifestMediaType,
			artifact.Blob{Data: oci.Config, MediaType: chart.ConfigMediaType}, artifact.Blob{Data: oci.Archive, MediaType: chart.LayerMediaType})
		if err != nil {
			return s, err
		}
		if digest != oci.Digest() {
			return s, fmt.Errorf("rejestr przyjął manifest %s, złożono %s", digest, oci.Digest())
		}
		s.Pushed = true
		cli.Step("%s: wysłany (%s)", ref, digest)
	default:
		return s, err
	}
	return s, verifyStored(ctx, reg, repository, oci)
}

// verifyStored pobiera manifest i warstwę po digeście i porównuje bajty
// z tym, co miało trafić do rejestru.
func verifyStored(ctx context.Context, reg Reader, repository string, oci chart.OCI) error {
	raw, _, err := reg.Manifest(ctx, repository+"@"+oci.Digest())
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, oci.Manifest) {
		return cli.Unmet("%s@%s: manifest w rejestrze różni się od wysłanego", repository, oci.Digest())
	}
	layer, err := reg.Blob(ctx, repository, digestOf(oci.Archive))
	if err != nil {
		return err
	}
	if !bytes.Equal(layer, oci.Archive) {
		return cli.Unmet("%s@%s: archiwum w rejestrze różni się od wysłanego", repository, oci.Digest())
	}
	return nil
}

// public sprawdza bez poświadczeń, czy artefakt da się pobrać. Pakiet
// niepubliczny to ostrzeżenie, nie błąd: publikacja się udała, a widoczność
// pakietu w GHCR ustawia raz autor (Argo CD pobiera bez poświadczeń).
func public(ctx context.Context, anon Reader, s Stored) (bool, error) {
	_, _, err := anon.Manifest(ctx, s.Repository+"@"+s.Digest)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, artifact.ErrDenied), errors.Is(err, artifact.ErrNotFound):
		cli.Warn("%s: pakiet nie jest publiczny — Argo CD go nie pobierze; GitHub → Packages → %s → Package settings → Change visibility", s.Repository, s.Repository)
		return false, nil
	default:
		return false, err
	}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
