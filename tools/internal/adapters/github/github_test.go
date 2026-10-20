package github

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
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
		"gh api repos/{owner}/{repo}/actions/runs/7/jobs?per_page=100":                                       {Stdout: `{"jobs":[{"name":"cluster","conclusion":"success","started_at":"2026-10-20T10:00:00Z","steps":[{"name":"dft","conclusion":"success","started_at":"2026-10-20T10:00:05Z","completed_at":"2026-10-20T10:00:50Z"}]}]}`},
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
	steps, err := c.JobSteps(ctx, 7)
	if err != nil || len(steps) != 1 || steps[0].Name != "cluster" || len(steps[0].Steps) != 1 || steps[0].Steps[0].CompletedAt != "2026-10-20T10:00:50Z" {
		t.Errorf("zadania z krokami: %+v, %v", steps, err)
	}
	if n, err := c.FindIssue(ctx, title); err != nil || n != 7 {
		t.Errorf("issue o dokładnie tym tytule: %d, %v", n, err)
	}
}

func releaseCommand(version string) string {
	return "gh attestation verify oci://" + repo + "@" + digest + " --repo mateuszf757/Docfind --signer-workflow mateuszf757/Docfind/.github/workflows/ci.yml --predicate-type " +
		artifact.ReleasePredicate + " --deny-self-hosted-runners --format json --source-ref refs/tags/v" + version
}

// released — atestacja wydania w postaci, jaką zwraca gh: predykat z
// numerem wydania, certyfikat z tagu.
func released(version string) string {
	return `[{"verificationResult":{"statement":{"subject":[{"name":"` + repo + `","digest":{"sha256":"` + strings.TrimPrefix(digest, "sha256:") +
		`"}}],"predicateType":"` + artifact.ReleasePredicate + `","predicate":{"version":"` + version + `","image":"` + repo + "@" + digest +
		`","commit":"cafe"}},"signature":{"certificate":{"sourceRepositoryRef":"refs/tags/v` + version + `","sourceRepositoryDigest":"cafe","runnerEnvironment":"github-hosted"}}}}]`
}

func TestMergedPullRequestsAndRunsForCommit(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"gh api repos/{owner}/{repo}/pulls?state=closed&base=main&sort=updated&direction=desc&per_page=30": {Stdout: `[
			{"number":3,"merged_at":"2026-10-12T10:00:00Z","html_url":"u3","head":{"sha":"c3"}},
			{"number":2,"merged_at":null,"html_url":"u2","head":{"sha":"c2"}},
			{"number":4,"merged_at":"2026-10-14T10:00:00Z","html_url":"u4","head":{"sha":"c4"}}]`},
		"gh api repos/{owner}/{repo}/actions/workflows/ci.yml/runs?head_sha=c4&event=pull_request&status=completed&per_page=10": {Stdout: `{"workflow_runs":[{"id":11,"event":"pull_request","run_attempt":1,"head_sha":"c4"}]}`},
	}}
	c := CLI{Runner: f}
	ctx := context.Background()
	prs, err := c.MergedPullRequests(ctx, "main", 30)
	// Zamknięty bez scalenia pominięty; kolejność według scalenia.
	if err != nil || len(prs) != 2 || prs[0].Number != 4 || prs[1].Number != 3 || prs[0].HeadSHA != "c4" {
		t.Errorf("scalone PR-y: %+v, %v", prs, err)
	}
	runs, err := c.RunsForCommit(ctx, "ci.yml", "c4", "pull_request")
	if err != nil || len(runs) != 1 || runs[0].ID != 11 {
		t.Errorf("biegi commita: %+v, %v", runs, err)
	}
}

func TestVerifyRelease(t *testing.T) {
	a := Attestations{Repo: "mateuszf757/Docfind", Workflow: ".github/workflows/ci.yml"}
	ctx := context.Background()
	a.CLI = CLI{Runner: &proc.Fake{Responses: map[string]proc.FakeResponse{releaseCommand("0.4.0"): {Stdout: released("0.4.0")}}}}
	rel, prov, err := a.VerifyRelease(ctx, repo+"@"+digest, "0.4.0")
	if err != nil || rel.Version != "0.4.0" || rel.Commit != "cafe" || prov.SourceRef != "refs/tags/v0.4.0" || prov.SourceCommit != "cafe" {
		t.Fatalf("wydanie %+v, pochodzenie %+v, %v", rel, prov, err)
	}
	// Atestacja innego wydania tego samego digestu nie jest tym wydaniem.
	a.CLI = CLI{Runner: &proc.Fake{Responses: map[string]proc.FakeResponse{releaseCommand("0.4.1"): {Stdout: released("0.4.0")}}}}
	if _, _, err := a.VerifyRelease(ctx, repo+"@"+digest, "0.4.1"); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("cudzy numer wydania: %v", err)
	}
	a.CLI = CLI{Runner: &proc.Fake{Responses: map[string]proc.FakeResponse{releaseCommand("0.4.0"): {Code: 1, Stderr: "no attestations found"}}}}
	if _, _, err := a.VerifyRelease(ctx, repo+"@"+digest, "0.4.0"); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("brak atestacji wydania: %v", err)
	}
}

func TestVerifyEvidenceAndRunArtifacts(t *testing.T) {
	command := "gh attestation verify oci://" + repo + "@" + digest + " --repo mateuszf757/Docfind --signer-workflow mateuszf757/Docfind/.github/workflows/ci.yml --predicate-type " +
		artifact.EvidencePredicate + " --deny-self-hosted-runners --format json --source-ref refs/tags/v0.4.0"
	pack := func(version string) string {
		return `[{"verificationResult":{"statement":{"subject":[{"name":"` + repo + `","digest":{"sha256":"` + strings.TrimPrefix(digest, "sha256:") +
			`"}}],"predicateType":"` + artifact.EvidencePredicate + `","predicate":{"release":{"version":"` + version + `","image":"` + repo + "@" + digest +
			`","commit":"cafe"},"main_runs":[{"id":7,"html_url":"u7","jobs":[{"name":"cluster","conclusion":"success"}]}]}},` +
			`"signature":{"certificate":{"sourceRepositoryRef":"refs/tags/v0.4.0","sourceRepositoryDigest":"cafe","runnerEnvironment":"github-hosted"}}}}]`
	}
	a := Attestations{Repo: "mateuszf757/Docfind", Workflow: ".github/workflows/ci.yml"}
	ctx := context.Background()
	a.CLI = CLI{Runner: &proc.Fake{Responses: map[string]proc.FakeResponse{command: {Stdout: pack("0.4.0")}}}}
	e, prov, err := a.VerifyEvidence(ctx, repo+"@"+digest, "0.4.0")
	if err != nil || e.Release.Version != "0.4.0" || len(e.Main) != 1 || e.Main[0].Jobs[0].Name != "cluster" || prov.SourceRef != "refs/tags/v0.4.0" {
		t.Fatalf("pakiet %+v, pochodzenie %+v, %v", e, prov, err)
	}
	// Pakiet innego wydania tego samego digestu nie jest pakietem tego wydania.
	a.CLI = CLI{Runner: &proc.Fake{Responses: map[string]proc.FakeResponse{command: {Stdout: pack("0.3.1")}}}}
	if _, _, err := a.VerifyEvidence(ctx, repo+"@"+digest, "0.4.0"); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("pakiet innego wydania: %v", err)
	}
	// Digest z main bez wydania: gh nie znajduje atestacji z tym predykatem.
	a.CLI = CLI{Runner: &proc.Fake{Responses: map[string]proc.FakeResponse{command: {Code: 1, Stderr: "no attestations found"}}}}
	if _, _, err := a.VerifyEvidence(ctx, repo+"@"+digest, "0.4.0"); cli.ExitCode(err) != cli.ExitUnmet {
		t.Errorf("brak pakietu: %v", err)
	}

	c := CLI{Runner: &proc.Fake{Responses: map[string]proc.FakeResponse{
		"gh api repos/{owner}/{repo}/actions/runs/7/artifacts?per_page=100": {Stdout: `{"total_count":2,"artifacts":[` +
			`{"name":"dowody-cluster","size_in_bytes":2048,"digest":"sha256:dd","expired":false,"expires_at":"2026-10-22T10:00:00Z"},` +
			`{"name":"obraz-api","size_in_bytes":9,"digest":"sha256:ee","expired":true,"expires_at":"2026-10-16T10:00:00Z"}]}`},
	}}}
	artifacts, err := c.RunArtifacts(ctx, 7)
	if err != nil || len(artifacts) != 2 || artifacts[0].Digest != "sha256:dd" || artifacts[0].Size != 2048 || !artifacts[1].Expired {
		t.Errorf("artefakty: %+v, %v", artifacts, err)
	}
}

func TestBranchRules(t *testing.T) {
	c := CLI{Runner: &proc.Fake{Responses: map[string]proc.FakeResponse{
		"gh api repos/o/r/rules/branches/main?per_page=100": {Stdout: `[{"type":"deletion","ruleset_id":1},` +
			`{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false,"required_status_checks":[{"context":"test","integration_id":15368},{"context":"build"}]}}]`},
	}}}
	rules, err := c.BranchRules(context.Background(), "o/r", "main")
	if err != nil || len(rules) != 2 || rules[0].Type != "deletion" || strings.Join(rules[1].Checks, ",") != "test,build" {
		t.Errorf("reguły: %+v, %v", rules, err)
	}
}

func TestRunsByEvent(t *testing.T) {
	c := CLI{Runner: &proc.Fake{Responses: map[string]proc.FakeResponse{
		"gh api repos/{owner}/{repo}/actions/workflows/ci.yml/runs?event=schedule&status=completed&per_page=100": {Stdout: `{"workflow_runs":[{"id":3,"event":"schedule","created_at":"2026-10-15T03:17:00Z"}]}`},
	}}}
	runs, err := c.RunsByEvent(context.Background(), "ci.yml", "schedule", 500)
	if err != nil || len(runs) != 1 || runs[0].CreatedAt != "2026-10-15T03:17:00Z" {
		t.Errorf("biegi: %+v, %v", runs, err)
	}
}

func TestReleaseTag(t *testing.T) {
	f := &proc.Fake{Responses: map[string]proc.FakeResponse{
		"gh api repos/{owner}/{repo}/git/ref/tags/v0.4.0": {Stdout: `{"ref":"refs/tags/v0.4.0","object":{"sha":"t1","type":"tag"}}`},
		"gh api repos/{owner}/{repo}/git/tags/t1":         {Stdout: `{"tag":"v0.4.0","message":"Etap 4\n\nImage: x\n","object":{"sha":"c1","type":"commit"}}`},
		"gh api repos/{owner}/{repo}/git/ref/tags/v0.4.1": {Stdout: `{"ref":"refs/tags/v0.4.1","object":{"sha":"c2","type":"commit"}}`},
	}}
	c := CLI{Runner: f}
	ctx := context.Background()
	commit, message, err := c.ReleaseTag(ctx, "v0.4.0")
	if err != nil || commit != "c1" || message != "Etap 4\n\nImage: x\n" {
		t.Errorf("tag z adnotacją: %q, %q, %v", commit, message, err)
	}
	if _, _, err := c.ReleaseTag(ctx, "v0.4.1"); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "lekki") {
		t.Errorf("lekki tag: %v", err)
	}
}
