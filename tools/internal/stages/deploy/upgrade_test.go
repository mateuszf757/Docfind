package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
)

// other — commit N-1: obrazy z rejestru w testach upgrade'u.
var other = strings.Repeat("a", 40)

// publishedFrom — chart w wersji version z obrazem commita c.
func publishedFrom(t *testing.T, version, digest, c string) chart.OCI {
	t.Helper()
	files, err := chart.Files(filepath.Join(repoRoot, "deploy", "charts", "docfind"))
	if err != nil {
		t.Fatal(err)
	}
	tag := artifact.CommitTag(c)
	oci, err := chart.Artifact("docfind", files, chart.Build{
		Version: version, AppVersion: tag, Tag: tag, Digest: digest, DeployVersion: version + "+" + c[:7],
	}, time.Unix(0, 0), "https://github.com/mateuszf757/Docfind")
	if err != nil {
		t.Fatal(err)
	}
	return oci
}

// chartRepo — repozytorium charta aplikacji w testach upgrade'u.
const chartRepo = appRepo

// published — chart w wersji version z obrazem commita N.
func published(t *testing.T, version, digest string) chart.OCI {
	t.Helper()
	return publishedFrom(t, version, digest, commit)
}

// appCharts — repozytorium charta aplikacji z chartami po wersji.
type appCharts struct {
	*recorder
	charts map[string]chart.OCI
}

func (f appCharts) Tags(_ context.Context, repo string) ([]string, error) {
	var tags []string
	for v := range f.charts {
		tags = append(tags, v)
	}
	return tags, f.call("tags %s", repo)
}

func (f appCharts) Manifest(_ context.Context, ref string) ([]byte, string, error) {
	oci, ok := f.charts[ref[strings.LastIndex(ref, ":")+1:]]
	if !ok {
		return nil, "", fmt.Errorf("brak %s", ref)
	}
	return oci.Manifest, chart.ManifestMediaType, f.call("manifest %s", ref)
}

func (f appCharts) Blob(_ context.Context, _, digest string) ([]byte, error) {
	for _, oci := range f.charts {
		if sum := sha256.Sum256(oci.Archive); "sha256:"+hex.EncodeToString(sum[:]) == digest {
			return oci.Archive, nil
		}
	}
	return nil, fmt.Errorf("brak bloba %s", digest)
}

// commitAdmission — polityka przyjmuje obraz z commita c.
type commitAdmission struct {
	*recorder
	commit string
}

func (f commitAdmission) Admit(_ context.Context, ref string, p admission.Policy) (admission.Admitted, error) {
	repository, digest, _ := artifact.ParseRef(ref)
	return admission.Admitted{Publication: artifact.Publication{Artifact: artifact.Artifact{
		Repository: repository, Digest: digest, Commit: f.commit,
	}}}, f.call("admit %s %s attested=%v", p.Environment, ref, p.Attested)
}

func upgradeDeployer(t *testing.T, env string, rec *recorder, charts map[string]chart.OCI, imageCommit string) Deployer {
	t.Helper()
	d := deployer(t, env, rec, true, "")
	d.AppChart = chartRepo
	d.ChartRegistry = appCharts{rec, charts}
	d.Admission = commitAdmission{rec, imageCommit}
	return d
}

// TestAdmitPrevious: N-1 to najnowszy chart z obrazem z innego commita niż
// N (chart z tego samego commita jest pomijany), a jego obraz przechodzi
// politykę środowiska po digeście, zanim cokolwiek zmieni się na klastrze.
func TestAdmitPrevious(t *testing.T) {
	quiet(t)
	rec := &recorder{}
	d := upgradeDeployer(t, "ci", rec, map[string]chart.OCI{
		"0.4.1-dev.10": published(t, "0.4.1-dev.10", digestOf('e')),
		"0.4.1-dev.9":  publishedFrom(t, "0.4.1-dev.9", digestOf('b'), other),
		"0.4.1-dev.3":  publishedFrom(t, "0.4.1-dev.3", digestOf('d'), other),
	}, other)
	p, err := d.AdmitPrevious(context.Background(), commit)
	if err != nil {
		t.Fatal(err)
	}
	want := Previous{Chart: "0.4.1-dev.9", Want: artifact.Expected{
		Image: apiImage + ":" + other[:12] + "@" + digestOf('b'), Version: "0.4.1-dev.9+" + other[:7], Commit: other,
	}}
	if p != want {
		t.Errorf("AdmitPrevious = %+v\n   oczekiwano %+v", p, want)
	}
	if !slices.Contains(rec.calls, "admit ci "+apiImage+"@"+digestOf('b')+" attested=true") {
		t.Errorf("wywołania: %q", rec.calls)
	}
	if slices.ContainsFunc(rec.calls, func(c string) bool { return strings.HasPrefix(c, "connect") || strings.HasPrefix(c, "install") }) {
		t.Errorf("wybór N-1 zmienia klaster: %q", rec.calls)
	}
}

func TestAdmitPreviousRejected(t *testing.T) {
	quiet(t)
	for name, tc := range map[string]struct {
		charts      map[string]chart.OCI
		imageCommit string
		want        string
	}{
		"tylko chart z commita N": {
			charts:      map[string]chart.OCI{"0.4.1-dev.3": published(t, "0.4.1-dev.3", digestOf('d'))},
			imageCommit: other,
			want:        "nie ma obrazu z commita innego niż",
		},
		// Tag w wartościach charta nie odpowiada etykiecie obrazu po digeście.
		"obraz z innego commita niż tag": {
			charts:      map[string]chart.OCI{"0.4.1-dev.3": publishedFrom(t, "0.4.1-dev.3", digestOf('d'), other)},
			imageCommit: strings.Repeat("f", 40),
			want:        "opisuje obraz tagiem " + other[:12],
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := upgradeDeployer(t, "ci", &recorder{}, tc.charts, tc.imageCommit)
			_, err := d.AdmitPrevious(context.Background(), commit)
			if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("AdmitPrevious: %v, oczekiwano fragmentu %q", err, tc.want)
			}
		})
	}
}

// TestRequireChartInstall: upgrade obok wdrożenia tylko tam, gdzie wolno
// wdrażać obrazy z rejestru i nie ma Argo CD z samonaprawą.
func TestRequireChartInstall(t *testing.T) {
	quiet(t)
	for env, want := range map[string]string{
		"ci":      "",
		"dev":     "",
		"staging": "Argo CD",
		"prod":    "nie dopuszcza operacji deploy",
	} {
		rec := &recorder{}
		d := upgradeDeployer(t, env, rec, nil, other)
		err := d.RequireChartInstall()
		if want == "" && err != nil || want != "" && (cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v, oczekiwano %q", env, err, want)
		}
		if want == "" {
			continue
		}
		if _, err := d.AdmitPrevious(context.Background(), commit); cli.ExitCode(err) != cli.ExitUnmet {
			t.Errorf("%s: AdmitPrevious: %v", env, err)
		}
		if err := d.InstallPrevious(context.Background(), Previous{Chart: "0.4.1-dev.3"}); cli.ExitCode(err) != cli.ExitUnmet {
			t.Errorf("%s: InstallPrevious: %v", env, err)
		}
		if len(rec.calls) != 0 {
			t.Errorf("%s: odmowa po wywołaniach %q", env, rec.calls)
		}
	}
}

// TestInstallPrevious: wydanie aplikacji dostaje chart z rejestru w wersji
// N-1 z wartościami środowiska — obraz i wersja wdrożenia z charta, nie
// z --set — po sekretach aplikacji.
func TestInstallPrevious(t *testing.T) {
	quiet(t)
	rec := &recorder{}
	d := upgradeDeployer(t, "ci", rec, nil, other)
	if err := d.InstallPrevious(context.Background(), Previous{Chart: "0.4.1-dev.9"}); err != nil {
		t.Fatal(err)
	}
	install := slices.IndexFunc(rec.calls, func(c string) bool { return strings.HasPrefix(c, "install docfind ") })
	secrets := slices.Index(rec.calls, "openbao app secrets")
	if install < 0 || secrets < 0 || secrets > install {
		t.Fatalf("wywołania: %q", rec.calls)
	}
	call := rec.calls[install]
	if !strings.Contains(call, "chart=oci://"+chartRepo+":0.4.1-dev.9") || strings.Contains(call, "api.image") || strings.Contains(call, "api.version") {
		t.Errorf("instalacja: %s", call)
	}
}
