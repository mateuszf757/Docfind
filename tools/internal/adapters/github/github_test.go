package github

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/reposettings"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
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

func TestVerifyAttestation(t *testing.T) {
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
			a := Attestations{CLI: CLI{Runner: fake}, Repo: "mateuszf757/Docfind", Workflow: ".github/workflows/ci.yml"}
			got, err := a.Verify(context.Background(), tt.ref, tt.sourceRef)
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if tt.wantCode == cli.ExitOK && (got.SourceRef != "refs/tags/v0.4.0" || got.Runner != "github-hosted" || got.SourceCommit != "cafe") {
				t.Errorf("wynik %+v", got)
			}
		})
	}
}

// TestPutEnvironment: recenzent po identyfikatorze, ciało PUT z definicji,
// plik tymczasowy usunięty zaraz po wywołaniu.
func TestPutEnvironment(t *testing.T) {
	var body, input string
	runner := proc.RunnerFunc(func(_ context.Context, c proc.Cmd) (proc.Result, error) {
		if strings.HasPrefix(c.String(), "gh api --method PUT repos/o/r/environments/production --input ") {
			input = c.Args[5]
			raw, err := os.ReadFile(input)
			body = string(raw)
			return proc.Result{}, err
		}
		return proc.Result{}, &proc.ExitError{Cmd: c.String(), Code: 1}
	})
	e := reposettings.Environment{Name: "production", BranchPolicy: reposettings.BranchPolicy{CustomBranchPolicies: true}}
	if err := (CLI{Runner: runner}).PutEnvironment(context.Background(), "o/r", e, []int64{4242}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reviewers":[{"type":"User","id":4242}]`, `"prevent_self_review":false`, `"custom_branch_policies":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("ciało PUT bez %s: %s", want, body)
		}
	}
	if _, err := os.Stat(input); !os.IsNotExist(err) {
		t.Errorf("plik tymczasowy został: %v", err)
	}
}

func TestBranchPoliciesAndApproval(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"gh api repos/o/r/environments/production/deployment-branch-policies": {Stdout: `{"total_count":1,"branch_policies":[{"id":3,"name":"release/*","type":"branch"}]}`},
		"gh api repos/o/r/environments/production":                            {Stdout: `{"protection_rules":[{"type":"branch_policy"},{"type":"required_reviewers","reviewers":[{"type":"User"}]}]}`},
		"gh api repos/o/r/environments/staging":                               {Stdout: `{"protection_rules":[]}`},
		"gh api repos/o/r/environments/zepsute":                               {Stdout: `<html>`},
	}}
	c := CLI{Runner: f}
	ctx := context.Background()
	got, err := c.BranchPolicies(ctx, "o/r", "production")
	if err != nil || got[reposettings.Policy{Name: "release/*", Type: "branch"}] != 3 {
		t.Errorf("polityki: %v, %v", got, err)
	}
	if ok, err := c.RequiresApproval(ctx, "o/r", "production"); !ok || err != nil {
		t.Errorf("production: %v, %v", ok, err)
	}
	// Tak wygląda środowisko utworzone przez GitHuba przy pierwszym użyciu.
	if ok, err := c.RequiresApproval(ctx, "o/r", "staging"); ok || err != nil {
		t.Errorf("staging: %v, %v", ok, err)
	}
	if _, err := c.RequiresApproval(ctx, "o/r", "zepsute"); err == nil {
		t.Error("nieczytelna odpowiedź przyjęta")
	}
}

func TestRunsAndIssues(t *testing.T) {
	const title = "Podatności"
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"gh api repos/{owner}/{repo}/actions/workflows/ci.yml/runs?branch=main&status=completed&per_page=50": {Stdout: `{"workflow_runs":[{"id":7,"event":"schedule","run_attempt":1}]}`},
		"gh api repos/{owner}/{repo}/actions/runs/7/jobs":                                                    {Stdout: `{"jobs":[{"name":"cluster","conclusion":"success"}]}`},
		"gh issue list --state open --search " + title + " in:title --json number,title":                     {Stdout: `[{"number":9,"title":"` + title + ` — stare"},{"number":7,"title":"` + title + `"}]`},
	}}
	c := CLI{Runner: f}
	ctx := context.Background()
	runs, err := c.WorkflowRuns(ctx, "ci.yml", "main", 50)
	if err != nil || len(runs) != 1 || runs[0].ID != 7 || runs[0].Event != "schedule" {
		t.Errorf("biegi: %+v, %v", runs, err)
	}
	jobs, err := c.Jobs(ctx, 7)
	if err != nil || len(jobs) != 1 || jobs[0].Conclusion != "success" {
		t.Errorf("zadania: %+v, %v", jobs, err)
	}
	if n, err := c.FindIssue(ctx, title); err != nil || n != 7 {
		t.Errorf("issue o dokładnie tym tytule: %d, %v", n, err)
	}
}
