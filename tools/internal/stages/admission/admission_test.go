package admission

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

const (
	apiImage = "ghcr.io/mateuszf757/docfind-api"
	repoRoot = "../../../.."
)

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

func definition(t *testing.T, name string) environment.Definition {
	t.Helper()
	for _, key := range []string{"DOCFIND_HOSTNAME", "DOCFIND_TLS_ISSUER", "DOCFIND_ACME_EMAIL", "DOCFIND_ACME_ZONE"} {
		t.Setenv(key, "")
	}
	e, err := environment.Load(repoRoot, name)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// registry — publikacja z podaną wersją i predykatami pod jednym digestem.
type registry struct {
	version    string
	predicates []string
}

func (r registry) Inspect(_ context.Context, ref string) (artifact.Publication, error) {
	if ref != apiImage+"@"+digest {
		return artifact.Publication{}, fmt.Errorf("nieoczekiwany ref %s", ref)
	}
	return artifact.Publication{
		Artifact:   artifact.Artifact{Repository: apiImage, Digest: digest, Version: r.version, Commit: commit},
		Predicates: r.predicates,
	}, nil
}

// attestations — atestacja GitHuba z podanego commita; zapamiętuje wymagany
// tag źródła.
type attestations struct {
	sourceCommit string
	refuse       bool
	asked        *string
}

func (a attestations) Verify(_ context.Context, ref, sourceRef string) (artifact.Provenance, error) {
	*a.asked = sourceRef
	if a.refuse {
		return artifact.Provenance{}, cli.Unmet("atestacja GitHuba dla %s nie przeszła weryfikacji", ref)
	}
	return artifact.Provenance{PredicateType: artifact.ProvenanceV1, SourceRef: sourceRef, SourceCommit: a.sourceCommit}, nil
}

func TestAdmit(t *testing.T) {
	quiet(t)
	attested := []string{artifact.ProvenancePredicates[0], artifact.SBOMPredicate}
	tests := []struct {
		name, env, ref, version string
		predicates              []string
		sourceCommit            string
		refuse, noVerifier      bool
		wantCode                int
		wantSourceRef           string
	}{
		{"obraz z main na staging", "staging", apiImage + "@" + digest, "0.3.1-dev.4+abc1234", attested, commit, false, false, cli.ExitOK, ""},
		{"tag w referencji niczego nie wybiera", "staging", apiImage + ":latest@" + digest, "0.3.1-dev.4+abc1234", attested, commit, false, false, cli.ExitOK, ""},
		{"wydanie na produkcję z tagu swojej wersji", "prod", apiImage + "@" + digest, "0.3.1", attested, commit, false, false, cli.ExitOK, "refs/tags/v0.3.1"},
		{"wydanie na staging też z tagu swojej wersji", "staging", apiImage + "@" + digest, "0.3.1", attested, commit, false, false, cli.ExitOK, "refs/tags/v0.3.1"},
		{"obraz z main na produkcję", "prod", apiImage + "@" + digest, "0.3.1-dev.4+abc1234", attested, commit, false, false, cli.ExitUnmet, ""},
		{"obcy obraz", "staging", "docker.io/library/nginx@" + digest, "0.3.1", attested, commit, false, false, cli.ExitUnmet, ""},
		{"tag zamiast digestu", "staging", apiImage + ":0.3.1", "0.3.1", attested, commit, false, false, cli.ExitFailure, ""},
		// Publikacje sprzed decyzji 37: pochodzenie bez SBOM.
		{"atestacja bez SBOM na staging", "staging", apiImage + "@" + digest, "0.3.0", attested[:1], commit, false, false, cli.ExitUnmet, ""},
		{"bez atestacji GitHuba na staging", "staging", apiImage + "@" + digest, "0.3.1-dev.4+abc1234", attested, commit, true, false, cli.ExitUnmet, ""},
		{"podpis z innego commita niż etykieta", "staging", apiImage + "@" + digest, "0.3.1-dev.4+abc1234", attested, strings.Repeat("e", 40), false, false, cli.ExitUnmet, ""},
		{"atestacja wymagana, brak weryfikatora", "staging", apiImage + "@" + digest, "0.3.1-dev.4+abc1234", attested, commit, false, true, cli.ExitFailure, ""},
		// dev nie wymaga atestacji — autor może postawić wydanie sprzed niej.
		{"wydanie sprzed SBOM w dev", "dev", apiImage + "@" + digest, "0.3.0", attested[:1], commit, false, true, cli.ExitOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asked := ""
			g := Gate{Registry: registry{version: tt.version, predicates: tt.predicates}}
			if !tt.noVerifier {
				g.Attestations = attestations{sourceCommit: tt.sourceCommit, refuse: tt.refuse, asked: &asked}
			}
			got, err := g.Admit(context.Background(), tt.ref, For(definition(t, tt.env), apiImage))
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if err == nil && (got.Publication.Digest != digest || got.Publication.Pinned() != apiImage+":"+artifact.DockerTag(tt.version)+"@"+digest) {
				t.Errorf("przyjęty %+v", got)
			}
			if asked != tt.wantSourceRef {
				t.Errorf("wymagany tag źródła %q, oczekiwano %q", asked, tt.wantSourceRef)
			}
		})
	}
}
