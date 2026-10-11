package artifact

import (
	"slices"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// SBOMPredicate — typ predykatu in-toto SBOM (SPDX). Atestacja publikacji
// musi nieść SBOM i pochodzenie SLSA (decyzja 37).
const SBOMPredicate = "https://spdx.dev/Document"

// ProvenancePredicates — pochodzenie SLSA w wersjach, które wystawia BuildKit.
var ProvenancePredicates = []string{"https://slsa.dev/provenance/v0.2", "https://slsa.dev/provenance/v1"}

// Etykiety OCI, którymi build opisuje obraz (services/api/Dockerfile).
const (
	VersionLabel  = "org.opencontainers.image.version"
	RevisionLabel = "org.opencontainers.image.revision"
)

// Publication — artefakt w rejestrze razem z tym, co przy nim leży.
type Publication struct {
	Artifact
	// Image — digest manifestu obrazu platformy (pod indeksem).
	Image string `json:"image"`
	// Attestation — digest manifestu atestacji BuildKitu.
	Attestation string `json:"attestation"`
	// Predicates — typy predykatów w manifeście atestacji.
	Predicates []string `json:"predicates"`
}

// HasProvenance mówi, czy atestacja niesie pochodzenie SLSA.
func (p Publication) HasProvenance() bool {
	return slices.ContainsFunc(p.Predicates, func(t string) bool { return slices.Contains(ProvenancePredicates, t) })
}

// HasSBOM mówi, czy atestacja niesie SBOM (SPDX).
func (p Publication) HasSBOM() bool {
	return slices.Contains(p.Predicates, SBOMPredicate)
}

// RequireAttestations odmawia publikacji bez pochodzenia SLSA albo bez SBOM.
func (p Publication) RequireAttestations() error {
	switch {
	case !p.HasProvenance():
		return cli.Unmet("atestacja %s bez pochodzenia SLSA (predykaty: %v)", p.Attestation, p.Predicates)
	case !p.HasSBOM():
		return cli.Unmet("atestacja %s bez SBOM (predykaty: %v)", p.Attestation, p.Predicates)
	}
	return nil
}

// ProvenanceV1 — predykat atestacji GitHuba (actions/attest).
const ProvenanceV1 = "https://slsa.dev/provenance/v1"

// Provenance — co zweryfikowana atestacja GitHuba (Sigstore) mówi
// o pochodzeniu digestu.
type Provenance struct {
	Subject       string `json:"subject"`
	PredicateType string `json:"predicate_type"`
	// SourceRef i SourceCommit — gałąź albo tag i commit, z których
	// zbudowano obraz.
	SourceRef    string `json:"source_ref"`
	SourceCommit string `json:"source_commit"`
	// Run — bieg workflowu, który obraz zbudował i podpisał.
	Run    string `json:"run"`
	Runner string `json:"runner_environment"`
}
