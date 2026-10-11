package release

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
	"github.com/mateuszf757/Docfind/tools/internal/stages/drain"
	"github.com/mateuszf757/Docfind/tools/internal/stages/identity"
	"github.com/mateuszf757/Docfind/tools/internal/stages/ingress"
)

const apiImage = "ghcr.io/mateuszf757/docfind-api"

var (
	digest = "sha256:" + strings.Repeat("d", 64)
	commit = strings.Repeat("c", 40)
)

func quiet(t *testing.T) {
	t.Helper()
	oldOut, oldErr := cli.Out, cli.Err
	cli.Out, cli.Err = io.Discard, io.Discard
	t.Cleanup(func() { cli.Out, cli.Err = oldOut, oldErr })
}

func staging(t *testing.T) environment.Definition {
	t.Helper()
	for _, key := range []string{"DOCFIND_HOSTNAME", "DOCFIND_TLS_ISSUER", "DOCFIND_ACME_EMAIL", "DOCFIND_ACME_ZONE"} {
		t.Setenv(key, "")
	}
	e, err := environment.Load("../../../..", "staging")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// gate — wpuszczanie: obraz z main z commitem commit, zapamiętuje politykę.
type gate struct {
	policy *admission.Policy
	commit string
	err    error
}

func (g gate) Admit(_ context.Context, ref string, p admission.Policy) (admission.Admitted, error) {
	*g.policy = p
	if g.err != nil {
		return admission.Admitted{}, g.err
	}
	repository, d, _ := artifact.ParseRef(ref)
	return admission.Admitted{Publication: artifact.Publication{Artifact: artifact.Artifact{Repository: repository, Digest: d, Commit: g.commit}}}, nil
}

type git struct {
	exists  bool
	latest  string
	created map[string]string
}

func (g *git) TagExists(context.Context, string) (bool, error)       { return g.exists, nil }
func (g *git) LatestRelease(context.Context, string) (string, error) { return g.latest, nil }
func (g *git) CreateTag(_ context.Context, name, c, message string) error {
	g.created[name] = c + "\n" + message
	return nil
}

type cluster struct{ image string }

func (c cluster) DeploymentImage(_ context.Context, ns, name, container string) (string, error) {
	if ns != "docfind" || name != "docfind-api" || container != "api" {
		return "", errors.New("zły Deployment")
	}
	return c.image, nil
}

// gates — bramki akceptacji z wynikami; calls — kolejność wywołań.
type gates struct {
	calls     []string
	drainErr  error
	renewSkip string
}

func (g *gates) Identity(context.Context) ([]identity.Pod, error) {
	g.calls = append(g.calls, "tożsamość")
	return []identity.Pod{{Pod: "a"}, {Pod: "b"}}, nil
}

func (g *gates) Drain(context.Context) (drain.Report, error) {
	g.calls = append(g.calls, "drain")
	if g.drainErr != nil {
		return drain.Report{}, g.drainErr
	}
	return drain.Report{Drains: []drain.DrainResult{{Node: "a"}, {Node: "b"}, {Node: "c"}}}, nil
}

func (g *gates) Ingress(context.Context) (ingress.Report, error) {
	g.calls = append(g.calls, "tls")
	return ingress.Report{Hostname: "staging.docfind.lol", RevisionBefore: 3, RevisionAfter: 4, Requests: 120, RenewalSkipped: g.renewSkip}, nil
}

func acceptor(t *testing.T, g *git, gt *gates, image string, policy *admission.Policy) Acceptor {
	return Acceptor{
		Staging: staging(t), Repository: apiImage, Cluster: cluster{image: image},
		Admission: gate{policy: policy, commit: commit}, Gates: gt, Git: g,
		Now: func() time.Time { return time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC) },
	}
}

// TestAccept: digest ze stagingu, pochodzenie z main, bramki w kolejności
// (tożsamość, drain, TLS, tożsamość po drainach) i tag z adnotacją na
// commicie obrazu, z zapisem, który CI odczyta.
func TestAccept(t *testing.T) {
	quiet(t)
	g, gt := &git{latest: "0.3.0", created: map[string]string{}}, &gates{}
	var policy admission.Policy
	r, err := acceptor(t, g, gt, apiImage+":cccccccccccc@"+digest, &policy).Accept(context.Background(), "v0.4.0", "Etap 4: indeksowanie")
	if err != nil {
		t.Fatal(err)
	}
	if !policy.Attested || policy.Repository != apiImage || policy.ReleasesOnly {
		t.Errorf("polityka %+v — wydaniem tylko obraz aplikacji z main", policy)
	}
	if strings.Join(gt.calls, ",") != "tożsamość,drain,tls,tożsamość" {
		t.Errorf("kolejność bramek: %v", gt.calls)
	}
	created, ok := g.created["v0.4.0"]
	if !ok || !strings.HasPrefix(created, commit+"\n") {
		t.Fatalf("tag: %q", created)
	}
	back, err := artifact.ParseReleaseTag("v0.4.0", strings.TrimPrefix(created, commit+"\n"))
	if err != nil || back != r || back.Image != apiImage+"@"+digest || back.Title != "Etap 4: indeksowanie" {
		t.Errorf("zapis w tagu: %+v, %v", back, err)
	}
	if !strings.Contains(r.Staging, "drain 3 węzłów") || !strings.Contains(r.Staging, "rewizja 3 → 4") {
		t.Errorf("podsumowanie akceptacji: %q", r.Staging)
	}
}

func TestAcceptRejects(t *testing.T) {
	quiet(t)
	tests := []struct {
		name    string
		tag     string
		git     *git
		gates   *gates
		image   string
		admit   error
		code    int
		wantMsg string
	}{
		{"tag spoza vX.Y.Z", "v0.4", &git{}, &gates{}, apiImage + "@" + digest, nil, cli.ExitFailure, "vX.Y.Z"},
		{"tag już jest", "v0.4.0", &git{exists: true}, &gates{}, apiImage + "@" + digest, nil, cli.ExitUnmet, "już istnieje"},
		{"na stagingu obraz bez digestu", "v0.4.0", &git{}, &gates{}, apiImage + ":local-abc", nil, cli.ExitUnmet, "tylko obraz z rejestru"},
		{"obraz spoza main", "v0.4.0", &git{}, &gates{}, apiImage + "@" + digest, cli.Unmet("atestacja z refs/tags/v0.3.0"), cli.ExitUnmet, "refs/tags"},
		{"numer nie rośnie", "v0.3.0", &git{latest: "0.3.0"}, &gates{}, apiImage + "@" + digest, nil, cli.ExitUnmet, "nie jest późniejsze"},
		{"drain nie przeszedł", "v0.4.0", &git{}, &gates{drainErr: cli.Unmet("2 z 900 żądań nie powiodło się")}, apiImage + "@" + digest, nil, cli.ExitUnmet, "nie powiodło się"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.git.created = map[string]string{}
			var policy admission.Policy
			a := acceptor(t, tt.git, tt.gates, tt.image, &policy)
			a.Admission = gate{policy: &policy, commit: commit, err: tt.admit}
			_, err := a.Accept(context.Background(), tt.tag, "")
			if cli.ExitCode(err) != tt.code || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("oczekiwano kodu %d z %q, jest %v", tt.code, tt.wantMsg, err)
			}
			if len(tt.git.created) > 0 {
				t.Errorf("tag mimo odmowy: %v", tt.git.created)
			}
		})
	}
}

type tags struct {
	commit, message string
}

func (t tags) ReleaseTag(context.Context, string) (string, string, error) {
	return t.commit, t.message, nil
}

type registry struct{ tagged []string }

func (r *registry) Tag(_ context.Context, ref, tag string) error {
	r.tagged = append(r.tagged, ref+" "+tag)
	return nil
}

func record() artifact.Release {
	return artifact.Release{Version: "0.4.0", Image: apiImage + "@" + digest, Commit: commit, AcceptedAt: "2026-10-12T10:00:00Z", Staging: "ok"}
}

// TestPublish: tag wydania z zapisem z akceptacji — ten sam digest dostaje
// w rejestrze tag z numerem, bez builda.
func TestPublish(t *testing.T) {
	quiet(t)
	reg := &registry{}
	var policy admission.Policy
	p := Publisher{Repository: apiImage, Tags: tags{commit, record().TagMessage()}, Admission: gate{policy: &policy, commit: commit}, Registry: reg}
	r, err := p.Publish(context.Background(), "v0.4.0", commit)
	if err != nil {
		t.Fatal(err)
	}
	if r.Version != "0.4.0" || strings.Join(reg.tagged, ",") != apiImage+"@"+digest+" 0.4.0" || !policy.Attested {
		t.Errorf("wydanie %+v, tagi %v, polityka %+v", r, reg.tagged, policy)
	}

	other := strings.Repeat("e", 40)
	for name, tt := range map[string]struct {
		p    Publisher
		head string
		want string
	}{
		"tag ręczny bez zapisu":           {Publisher{Repository: apiImage, Tags: tags{commit, "Etap 4\n"}, Admission: gate{policy: &policy, commit: commit}, Registry: &registry{}}, commit, "bez digestu"},
		"tag na innym commicie niż zapis": {Publisher{Repository: apiImage, Tags: tags{other, record().TagMessage()}, Admission: gate{policy: &policy, commit: commit}, Registry: &registry{}}, other, "a zapisane wydanie"},
		"workflow na innym commicie":      {Publisher{Repository: apiImage, Tags: tags{commit, record().TagMessage()}, Admission: gate{policy: &policy, commit: commit}, Registry: &registry{}}, other, "workflow biegnie"},
		"obraz z innego commita":          {Publisher{Repository: apiImage, Tags: tags{commit, record().TagMessage()}, Admission: gate{policy: &policy, commit: other}, Registry: &registry{}}, commit, "zbudowano z"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := tt.p.Publish(context.Background(), "v0.4.0", tt.head); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("oczekiwano odmowy z %q, jest %v", tt.want, err)
			}
			if reg := tt.p.Registry.(*registry); len(reg.tagged) > 0 {
				t.Errorf("tag w rejestrze mimo odmowy: %v", reg.tagged)
			}
		})
	}
}
