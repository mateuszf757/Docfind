package chartpub

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/core/delivery"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// DeliveryChart — nazwa pakietu dostawy w Chart.yaml i w archiwum.
const DeliveryChart = "docfind-delivery"

// DeliveryPublisher — pakiet dostawy (deploy/charts/delivery, decyzja 81):
// Applications platformy i aplikacji po digestach artefaktów z bieżącego
// commita, z wartościami środowisk z GitOps z definicji.
type DeliveryPublisher struct {
	// Root — korzeń repozytorium: chart pakietu, wartości komponentów
	// (deploy/platform) i definicje środowisk.
	Root     string
	Repo     Repo
	Registry Registry
	Public   Reader
	// Mirror — lustro, z którego pakiet bierze digesty przypiętych chartów.
	Mirror     Mirror
	Repository string
	Source     string
}

// Build składa pakiet dla charta aplikacji app i charta platformy plat
// (repozytorium@digest w rejestrze). Oba muszą być artefaktami bieżącego
// commita — pakiet dostawy commita X wdraża tylko to, co z X zbudowano
// i przetestowano. Brudne drzewo — odmowa.
func (p DeliveryPublisher) Build(ctx context.Context, app, plat string) (chart.OCI, delivery.Bundle, error) {
	if err := p.Repo.RequireClean(ctx); err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	commit, err := p.Repo.Commit(ctx)
	if err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	tag := artifact.CommitTag(commit)
	appRepo, appDigest, err := p.ofCommit(ctx, "chart aplikacji", app, tag)
	if err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	platRepo, platDigest, err := p.ofCommit(ctx, "chart platformy", plat, tag)
	if err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	mirror, err := p.Mirror.Digests(ctx)
	if err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	values, err := delivery.MirrorValues(p.Root)
	if err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	all, err := environment.All(p.Root)
	if err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	var envs []environment.Definition
	for _, e := range all {
		if e.GitOps() {
			envs = append(envs, e)
		}
	}
	b, err := delivery.Compose(delivery.Artifacts{
		Mirror: mirror, MirrorRepo: p.Mirror.Repository, MirrorValues: values,
		PlatformRepo: platRepo, PlatformDigest: platDigest, AppRepo: appRepo, AppDigest: appDigest,
	}, envs)
	if err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	files, err := chart.Files(filepath.Join(p.Root, "deploy", "charts", "delivery"))
	if err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	version, err := p.Repo.Version(ctx)
	if err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	epoch, err := p.Repo.SourceDateEpoch(ctx)
	if err != nil {
		return chart.OCI{}, delivery.Bundle{}, err
	}
	oci, err := chart.Values(DeliveryChart, files, chart.Meta{Version: chart.ChartVersion(version), AppVersion: tag}, b.Values(), time.Unix(epoch, 0), p.Source)
	return oci, b, err
}

// ofCommit czyta chart ref (repozytorium@digest) z rejestru i sprawdza, że
// to artefakt commita o tagu tag (appVersion w Chart.yaml).
func (p DeliveryPublisher) ofCommit(ctx context.Context, what, ref, tag string) (string, string, error) {
	repository, digest, err := artifact.ParseRef(ref)
	if err != nil {
		return "", "", err
	}
	b, err := Read(ctx, p.Registry, repository, ref)
	if err != nil {
		return "", "", fmt.Errorf("%s %s: %w", what, ref, err)
	}
	if b.AppVersion != tag {
		return "", "", cli.Unmet("%s %s jest z commita %s, a pakiet dostawy powstaje z %s", what, ref, b.AppVersion, tag)
	}
	return repository, digest, nil
}

// Publish publikuje pakiet dostawy i sprawdza go w rejestrze.
func (p DeliveryPublisher) Publish(ctx context.Context, app, plat string) (Stored, delivery.Bundle, error) {
	oci, b, err := p.Build(ctx, app, plat)
	if err != nil {
		return Stored{}, b, err
	}
	s, err := put(ctx, p.Registry, p.Repository, oci, false)
	if err != nil {
		return Stored{}, b, err
	}
	s.Public, err = public(ctx, p.Public, s)
	return s, b, err
}
