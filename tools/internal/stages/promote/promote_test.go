package promote

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
)

const apiImage = "ghcr.io/mateuszf757/docfind-api"

var (
	digest = "sha256:" + strings.Repeat("d", 64)
	commit = strings.Repeat("c", 40)
)

func definition(t *testing.T, name string) environment.Definition {
	t.Helper()
	for _, key := range []string{"DOCFIND_HOSTNAME", "DOCFIND_TLS_ISSUER", "DOCFIND_ACME_EMAIL", "DOCFIND_ACME_ZONE"} {
		t.Setenv(key, "")
	}
	e, err := environment.Load("../../../..", name)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type gate struct {
	policy *admission.Policy
	err    error
}

func (g gate) Admit(_ context.Context, ref string, p admission.Policy) (admission.Admitted, error) {
	*g.policy = p
	repository, d, _ := artifact.ParseRef(ref)
	return admission.Admitted{Publication: artifact.Publication{Artifact: artifact.Artifact{Repository: repository, Digest: d, Version: "0.4.0", Commit: commit}}}, g.err
}

type approvals struct {
	required bool
	err      error
}

func (a approvals) RequiresApproval(context.Context, string, string) (bool, error) {
	return a.required, a.err
}

func TestPromote(t *testing.T) {
	oldOut := cli.Out
	cli.Out = io.Discard
	defer func() { cli.Out = oldOut }()
	ctx := context.Background()
	var policy admission.Policy
	p := Promoter{Env: definition(t, "prod"), Repository: apiImage, Admission: gate{policy: &policy}, Run: "https://github.com/o/r/actions/runs/1"}
	got, err := p.Promote(ctx, apiImage+"@"+digest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Release.Image != apiImage+":0.4.0@"+digest || got.Release.Tag != "0.4.0" || got.Run == "" {
		t.Errorf("promocja %+v", got)
	}
	// Polityka produkcji: tylko wydania z atestacją, tylko obraz aplikacji.
	if !policy.ReleasesOnly || !policy.Attested || policy.Repository != apiImage || policy.Environment != "prod" {
		t.Errorf("polityka %+v", policy)
	}
	for _, want := range []string{"git checkout v0.4.0", "--set api.image.digest=" + digest, "--set route.hostname="} {
		if !strings.Contains(got.Install, want) {
			t.Errorf("instalacja bez %q:\n%s", want, got.Install)
		}
	}
	if !strings.Contains(got.Summary(), "| 0.4.0 | `"+apiImage+":0.4.0@"+digest+"` |") {
		t.Errorf("podsumowanie:\n%s", got.Summary())
	}

	t.Run("środowisko GitHuba bez zgody", func(t *testing.T) {
		p := p
		p.Approvals, p.GitHubRepo, p.GitHubEnvironment = approvals{required: false}, "o/r", "production"
		if _, err := p.Promote(ctx, apiImage+"@"+digest); cli.ExitCode(err) != cli.ExitUnmet {
			t.Errorf("promocja bez zgody: %v", err)
		}
		p.Approvals = approvals{err: errors.New("API")}
		if _, err := p.Promote(ctx, apiImage+"@"+digest); cli.ExitCode(err) != cli.ExitFailure {
			t.Errorf("awaria API: %v", err)
		}
	})
	t.Run("środowisko bez źródła rejestru", func(t *testing.T) {
		p := Promoter{Env: definition(t, "ci"), Repository: apiImage, Admission: gate{policy: &policy}}
		p.Env.Artifact.Sources = []string{environment.SourceArchive}
		if _, err := p.Promote(ctx, apiImage+"@"+digest); cli.ExitCode(err) != cli.ExitUnmet {
			t.Errorf("środowisko bez rejestru: %v", err)
		}
	})
}
