package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Attestations weryfikuje atestacje GitHuba (Sigstore) obrazów budowanych
// przez workflow Workflow repozytorium Repo: podpis z krótkotrwałym
// certyfikatem wystawionym workflowowi GitHub Actions, wpis w publicznym
// dzienniku przejrzystości (Rekor) i predykat pochodzenia SLSA v1.
//
// Atestacja BuildKitu leży w rejestrze obok obrazu i mówi, jak obraz
// zbudowano — ale podpisuje ją nikt; ten, kto może pisać do rejestru, może ją
// podmienić. Atestacja GitHuba wiąże digest z workflowem i commitem tego
// repozytorium podpisem, którego klucza nie ma nikt. Weryfikuje `gh
// attestation verify` z mise — ten sam program, którym sprawdziłby ją klient.
type Attestations struct {
	CLI CLI
	// Repo — właściciel/repozytorium, np. mateuszf757/Docfind.
	Repo string
	// Workflow — ścieżka workflowu, który podpisuje, np. .github/workflows/ci.yml.
	Workflow string
}

type verification struct {
	VerificationResult struct {
		Statement struct {
			Subject []struct {
				Name   string            `json:"name"`
				Digest map[string]string `json:"digest"`
			} `json:"subject"`
			PredicateType string `json:"predicateType"`
		} `json:"statement"`
		Signature struct {
			Certificate struct {
				SourceRepositoryRef    string `json:"sourceRepositoryRef"`
				SourceRepositoryDigest string `json:"sourceRepositoryDigest"`
				RunInvocationURI       string `json:"runInvocationURI"`
				RunnerEnvironment      string `json:"runnerEnvironment"`
			} `json:"certificate"`
		} `json:"signature"`
	} `json:"verificationResult"`
}

// Verify sprawdza atestację obrazu ref (repozytorium@sha256:…). sourceRef,
// jeśli niepusty, to wymagana gałąź albo tag źródła (np. refs/tags/v0.4.0
// dla wydania). Odmowa — także gdy gh nie dosięgnie API — to warunek
// niespełniony: obrazu bez sprawdzonej atestacji się nie wdraża.
func (a Attestations) Verify(ctx context.Context, ref, sourceRef string) (artifact.Provenance, error) {
	repository, digest, err := artifact.ParseRef(ref)
	if err != nil {
		return artifact.Provenance{}, err
	}
	want := strings.TrimPrefix(digest, "sha256:")
	args := []string{"attestation", "verify", "oci://" + repository + "@" + digest,
		"--repo", a.Repo,
		"--signer-workflow", a.Repo + "/" + a.Workflow,
		"--predicate-type", artifact.ProvenanceV1,
		// Runner self-hosted mógłby zbudować cokolwiek pod podpisem tego
		// workflowu (decyzja 20: tylko runnery GitHuba).
		"--deny-self-hosted-runners",
		"--format", "json"}
	if sourceRef != "" {
		args = append(args, "--source-ref", sourceRef)
	}
	res, err := a.CLI.Runner.Run(ctx, proc.Cmd{Name: "gh", Args: args})
	if err != nil {
		stderr := ""
		var exit *proc.ExitError
		if errors.As(err, &exit) {
			stderr = strings.TrimSpace(string(exit.Stderr))
		}
		if proc.ExitCode(err) > 0 {
			return artifact.Provenance{}, cli.Unmet("atestacja GitHuba dla %s nie przeszła weryfikacji: %s", ref, firstLines(stderr, 3))
		}
		return artifact.Provenance{}, err
	}
	var results []verification
	if err := json.Unmarshal(res.Stdout, &results); err != nil {
		return artifact.Provenance{}, fmt.Errorf("gh attestation verify: nieczytelny wynik: %w", err)
	}
	for _, r := range results {
		s := r.VerificationResult.Statement
		for _, subject := range s.Subject {
			if subject.Digest["sha256"] != want || s.PredicateType != artifact.ProvenanceV1 {
				continue
			}
			c := r.VerificationResult.Signature.Certificate
			return artifact.Provenance{
				Subject: subject.Name + "@sha256:" + want, PredicateType: s.PredicateType,
				SourceRef: c.SourceRepositoryRef, SourceCommit: c.SourceRepositoryDigest,
				Run: c.RunInvocationURI, Runner: c.RunnerEnvironment,
			}, nil
		}
	}
	return artifact.Provenance{}, cli.Unmet("gh zweryfikował %d atestacji, ale żadna nie dotyczy %s z predykatem %s", len(results), ref, artifact.ProvenanceV1)
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}
