package deploy

import (
	"context"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/gitops"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
	"github.com/mateuszf757/Docfind/tools/internal/stages/chartpub"
)

// previousScan — ile najnowszych wersji charta przejrzeć w poszukiwaniu
// poprzedniej: każda to odczyt charta z rejestru.
const previousScan = 10

// Previous — N-1 dla upgrade'u pod ruchem (decyzja 79): wersja charta
// aplikacji w rejestrze i tożsamość, którą po instalacji mają podać pody.
type Previous struct {
	Chart string            `json:"chart"`
	Want  artifact.Expected `json:"want"`
}

// RequireChartInstall odmawia instalacji charta z rejestru obok wdrożenia
// tam, gdzie nie wolno: bez operacji deploy, bez obrazów z rejestru
// i z Argo CD — samonaprawa cofnęłaby każdą zmianę obok niego.
func (d Deployer) RequireChartInstall() error {
	if err := d.Env.Require(environment.OpDeploy); err != nil {
		return err
	}
	if d.Env.GitOps() {
		return cli.Unmet("środowisko %s wdraża Argo CD z samonaprawą — upgrade obok niego zostałby cofnięty, a cofnięcie to tam przypięcie wersji w definicji; upgrade i rollback pod ruchem sprawdza się na ci i dev", d.Env.Name)
	}
	if !d.Env.AllowsSource(environment.SourceRegistry) {
		return cli.Unmet("środowisko %s nie przyjmuje obrazów z rejestru (artifact.sources: %v)", d.Env.Name, d.Env.Artifact.Sources)
	}
	return nil
}

// AdmitPrevious wybiera N-1 — najwyższą wersję charta w rejestrze AppChart,
// której obraz pochodzi z innego commita niż commit (chart z tego samego
// commita to ta sama wersja w innym opakowaniu) — i przepuszcza jego obraz
// przez politykę środowiska jak obraz z --image (decyzja 35), zanim
// cokolwiek zmieni się na klastrze. Upgrade nie wpuści niczego, czego nie
// wpuściłby `cluster up`.
func (d Deployer) AdmitPrevious(ctx context.Context, commit string) (Previous, error) {
	if err := d.RequireChartInstall(); err != nil {
		return Previous{}, err
	}
	version, b, err := d.previousChart(ctx, commit)
	if err != nil {
		return Previous{}, err
	}
	admitted, err := d.Admission.Admit(ctx, d.Image+"@"+b.Digest, admission.For(d.Env, d.Image, ""))
	if err != nil {
		return Previous{}, err
	}
	imageCommit := admitted.Publication.Artifact.Commit
	if !strings.HasPrefix(imageCommit, b.Tag) {
		return Previous{}, cli.Unmet("chart %s:%s opisuje obraz tagiem %s, a obraz %s pochodzi z commita %s", d.AppChart, version, b.Tag, b.Digest, artifact.ShortCommit(imageCommit))
	}
	cli.Step("N-1: chart %s:%s, obraz %s@%s z commita %s", d.AppChart, version, b.Tag, b.Digest, artifact.ShortCommit(imageCommit))
	return Previous{Chart: version, Want: artifact.Expected{Image: d.Image + ":" + b.Tag + "@" + b.Digest, Version: b.DeployVersion, Commit: imageCommit}}, nil
}

func (d Deployer) previousChart(ctx context.Context, commit string) (string, chart.Build, error) {
	tags, err := d.ChartRegistry.Tags(ctx, d.AppChart)
	if err != nil {
		return "", chart.Build{}, err
	}
	versions := gitops.Descending(tags)
	for _, v := range versions[:min(len(versions), previousScan)] {
		b, err := chartpub.Read(ctx, d.ChartRegistry, d.AppChart, d.AppChart+":"+v)
		if err != nil {
			return "", chart.Build{}, err
		}
		if b.Tag != "" && !strings.HasPrefix(commit, b.Tag) {
			return v, b, nil
		}
	}
	return "", chart.Build{}, cli.Unmet("%s: wśród %d najnowszych wersji charta nie ma obrazu z commita innego niż %s", d.AppChart, min(len(versions), previousScan), artifact.ShortCommit(commit))
}

// InstallPrevious aktualizuje aplikację do charta p z rejestru, z wartościami
// środowiska jak każde wdrożenie; obraz i wersja wdrożenia są w wartościach
// charta (decyzja 61). Tożsamość na podach sprawdza wołający — tę z p.
func (d Deployer) InstallPrevious(ctx context.Context, p Previous) error {
	if err := d.RequireChartInstall(); err != nil {
		return err
	}
	c, err := d.Connect(d.target())
	if err != nil {
		return err
	}
	caBundle, err := d.appSecrets(ctx, c)
	if err != nil {
		return err
	}
	cli.Step("helm upgrade %s do %s:%s", d.Env.App.Release, d.AppChart, p.Chart)
	return d.Releases.Install(ctx, d.target(), cluster.Release{
		Name: d.Env.App.Release, Chart: "oci://" + d.AppChart, Version: p.Chart, Namespace: d.Env.App.Namespace,
		Set: d.Env.AppValues(caBundle), Timeout: 3 * time.Minute,
	})
}
