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
			PredicateType string          `json:"predicateType"`
			Predicate     json.RawMessage `json:"predicate"`
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

// Verify sprawdza atestację pochodzenia (SLSA v1) obrazu ref
// (repozytorium@sha256:…). sourceRef, jeśli niepusty, to wymagana gałąź
// albo tag źródła (np. refs/heads/main). Odmowa — także gdy gh nie dosięgnie
// API — to warunek niespełniony: obrazu bez sprawdzonej atestacji się nie
// wdraża.
func (a Attestations) Verify(ctx context.Context, ref, sourceRef string) (artifact.Provenance, error) {
	prov, _, err := a.verify(ctx, ref, artifact.ProvenanceV1, sourceRef, func(json.RawMessage) bool { return true })
	return prov, err
}

// VerifyRelease sprawdza atestację wydania version obrazu ref: predykat
// wydania (decyzja 40) z workflowu tego repozytorium, wystawiony na tagu
// vX.Y.Z. Zwraca wydanie z predykatu i pochodzenie podpisu (commit tagu).
func (a Attestations) VerifyRelease(ctx context.Context, ref, version string) (artifact.Release, artifact.Provenance, error) {
	var release artifact.Release
	prov, raw, err := a.verify(ctx, ref, artifact.ReleasePredicate, "refs/tags/v"+version, func(p json.RawMessage) bool {
		var r artifact.Release
		return json.Unmarshal(p, &r) == nil && r.Version == version
	})
	if err != nil {
		return artifact.Release{}, artifact.Provenance{}, err
	}
	if err := json.Unmarshal(raw, &release); err != nil {
		return artifact.Release{}, artifact.Provenance{}, fmt.Errorf("predykat wydania: %w", err)
	}
	return release, prov, nil
}

// verify uruchamia `gh attestation verify` dla typu predykatu i zwraca
// pierwszą atestację tego digestu, której predykat przyjmuje accept.
func (a Attestations) verify(ctx context.Context, ref, predicateType, sourceRef string, accept func(json.RawMessage) bool) (artifact.Provenance, json.RawMessage, error) {
	repository, digest, err := artifact.ParseRef(ref)
	if err != nil {
		return artifact.Provenance{}, nil, err
	}
	want := strings.TrimPrefix(digest, "sha256:")
	args := []string{"attestation", "verify", "oci://" + repository + "@" + digest,
		"--repo", a.Repo,
		"--signer-workflow", a.Repo + "/" + a.Workflow,
		"--predicate-type", predicateType,
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
			return artifact.Provenance{}, nil, cli.Unmet("atestacja GitHuba (%s) dla %s nie przeszła weryfikacji: %s", predicateType, ref, firstLines(stderr, 3))
		}
		return artifact.Provenance{}, nil, err
	}
	var results []verification
	if err := json.Unmarshal(res.Stdout, &results); err != nil {
		return artifact.Provenance{}, nil, fmt.Errorf("gh attestation verify: nieczytelny wynik: %w", err)
	}
	for _, r := range results {
		s := r.VerificationResult.Statement
		for _, subject := range s.Subject {
			if subject.Digest["sha256"] != want || s.PredicateType != predicateType || !accept(s.Predicate) {
				continue
			}
			c := r.VerificationResult.Signature.Certificate
			return artifact.Provenance{
				Subject: subject.Name + "@sha256:" + want, PredicateType: s.PredicateType,
				SourceRef: c.SourceRepositoryRef, SourceCommit: c.SourceRepositoryDigest,
				Run: c.RunInvocationURI, Runner: c.RunnerEnvironment,
			}, s.Predicate, nil
		}
	}
	return artifact.Provenance{}, nil, cli.Unmet("gh zweryfikował %d atestacji, ale żadna nie dotyczy %s z predykatem %s", len(results), ref, predicateType)
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, " | ")
}
