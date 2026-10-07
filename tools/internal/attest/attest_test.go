package attest

import (
	"context"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

const (
	repo   = "ghcr.io/mateuszf757/docfind-api"
	digest = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
)

func command(sourceRef string) string {
	c := "gh attestation verify oci://" + repo + "@" + digest + " --repo mateuszf757/Docfind --signer-workflow mateuszf757/Docfind/.github/workflows/ci.yml --predicate-type https://slsa.dev/provenance/v1 --deny-self-hosted-runners --format json"
	if sourceRef != "" {
		c += " --source-ref " + sourceRef
	}
	return c
}

// verified — wynik w postaci, jaką zwraca gh 2.101.0 (skrócony).
func verified(subjectDigest string) string {
	return `[{"verificationResult":{"statement":{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"` + repo + `","digest":{"sha256":"` +
		subjectDigest + `"}}],"predicateType":"https://slsa.dev/provenance/v1"},"signature":{"certificate":{"sourceRepositoryRef":"refs/tags/v0.4.0","sourceRepositoryDigest":"cafe","runInvocationURI":"https://github.com/mateuszf757/Docfind/actions/runs/1/attempts/1","runnerEnvironment":"github-hosted"}}}}]`
}

func TestVerify(t *testing.T) {
	want := strings.TrimPrefix(digest, "sha256:")
	tests := []struct {
		name, ref, sourceRef string
		response             proc.FakeResponse
		wantCode             int
	}{
		{"zweryfikowana", repo + "@" + digest, "", proc.FakeResponse{Stdout: verified(want)}, cli.ExitOK},
		{"tag w referencji niczego nie zmienia", repo + ":0.4.0@" + digest, "refs/tags/v0.4.0", proc.FakeResponse{Stdout: verified(want)}, cli.ExitOK},
		{"atestacja innego obrazu", repo + "@" + digest, "", proc.FakeResponse{Stdout: verified(strings.Repeat("0", 64))}, cli.ExitUnmet},
		{"gh odmawia", repo + "@" + digest, "", proc.FakeResponse{Code: 1, Stderr: "✗ Verification failed\nno attestations found"}, cli.ExitUnmet},
		{"nieczytelny wynik", repo + "@" + digest, "", proc.FakeResponse{Stdout: "nie JSON"}, cli.ExitFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &proc.Fake{Responses: map[string]proc.FakeResponse{command(tt.sourceRef): tt.response}}
			v := Verifier{Runner: fake, Repo: "mateuszf757/Docfind", Workflow: ".github/workflows/ci.yml"}
			got, err := v.Verify(context.Background(), tt.ref, tt.sourceRef)
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if tt.wantCode == cli.ExitOK && (got.SourceRef != "refs/tags/v0.4.0" || got.Runner != "github-hosted") {
				t.Errorf("wynik %+v", got)
			}
		})
	}
}
