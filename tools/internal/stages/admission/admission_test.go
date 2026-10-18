package admission

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
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

// registry — publikacja z commitem i predykatami pod jednym digestem.
type registry struct {
	commit     string
	predicates []string
}

func (r registry) Inspect(_ context.Context, ref string) (artifact.Publication, error) {
	if ref != apiImage+"@"+digest {
		return artifact.Publication{}, fmt.Errorf("nieoczekiwany ref %s", ref)
	}
	return artifact.Publication{
		Artifact:   artifact.Artifact{Repository: apiImage, Digest: digest, Commit: r.commit},
		Predicates: r.predicates,
	}, nil
}

// attestations — pochodzenie z podanej gałęzi i commita oraz atestacja
// wydania; zapamiętuje, o co pytano.
type attestations struct {
	sourceRef, sourceCommit string
	refuse                  bool
	release                 *artifact.Release
	releaseCommit           string
	evidence                *artifact.Evidence
	askedRef                *string
}

func (a attestations) Verify(_ context.Context, ref, sourceRef string) (artifact.Provenance, error) {
	*a.askedRef = sourceRef
	if a.refuse || (sourceRef != "" && sourceRef != a.sourceRef) {
		return artifact.Provenance{}, cli.Unmet("atestacja GitHuba dla %s nie przeszła weryfikacji (źródło %s)", ref, sourceRef)
	}
	return artifact.Provenance{PredicateType: artifact.ProvenanceV1, SourceRef: a.sourceRef, SourceCommit: a.sourceCommit}, nil
}

func (a attestations) VerifyRelease(_ context.Context, ref, version string) (artifact.Release, artifact.Provenance, error) {
	if a.release == nil || a.release.Version != version {
		return artifact.Release{}, artifact.Provenance{}, cli.Unmet("brak atestacji wydania %s dla %s", version, ref)
	}
	return *a.release, artifact.Provenance{PredicateType: artifact.ReleasePredicate, SourceRef: "refs/tags/v" + version, SourceCommit: a.releaseCommit}, nil
}

func (a attestations) VerifyEvidence(_ context.Context, ref, version string) (artifact.Evidence, artifact.Provenance, error) {
	if a.evidence == nil || a.evidence.Release.Version != version {
		return artifact.Evidence{}, artifact.Provenance{}, cli.Unmet("brak atestacji pakietu dowodów wydania %s dla %s", version, ref)
	}
	return *a.evidence, artifact.Provenance{PredicateType: artifact.EvidencePredicate, SourceRef: "refs/tags/v" + version}, nil
}

// evidenceOf — pakiet dowodów wydania r z zielonym biegiem main.
func evidenceOf(r *artifact.Release) *artifact.Evidence {
	var jobs []ci.Job
	for _, name := range artifact.MainJobs {
		jobs = append(jobs, ci.Job{Name: name, Conclusion: "success"})
	}
	return &artifact.Evidence{Release: *r, Main: []artifact.EvidenceRun{{
		Run:  ci.Run{ID: 7, Conclusion: "success", HeadSHA: r.Commit, URL: "https://github.com/o/r/actions/runs/7"},
		Jobs: jobs,
	}}}
}

func releaseOf(image, version, from string) *artifact.Release {
	return &artifact.Release{Version: version, Image: image, Commit: from, AcceptedAt: "2026-10-12T10:00:00Z", Staging: "ok", GatesCommit: strings.Repeat("a", 40)}
}

func TestAdmit(t *testing.T) {
	quiet(t)
	attested := []string{artifact.ProvenancePredicates[0], artifact.SBOMPredicate}
	main := attestations{sourceRef: MainRef, sourceCommit: commit}
	withRelease := func(a attestations, r *artifact.Release, signedFrom string) attestations {
		a.release, a.releaseCommit, a.evidence = r, signedFrom, evidenceOf(r)
		return a
	}
	withoutEvidence := func(a attestations) attestations {
		a.evidence = nil
		return a
	}
	redMain := func(a attestations) attestations {
		e := *a.evidence
		e.Main = []artifact.EvidenceRun{{Run: ci.Run{ID: 8, Conclusion: "failure", HeadSHA: commit, URL: "u8"}}}
		a.evidence = &e
		return a
	}
	released := withRelease(main, releaseOf(apiImage+"@"+digest, "0.4.0", commit), commit)
	tests := []struct {
		name, env, ref, release string
		predicates              []string
		att                     *attestations
		wantCode                int
		wantPinned              string
	}{
		{"obraz z main na staging", "staging", apiImage + "@" + digest, "", attested, &main, cli.ExitOK, apiImage + ":cccccccccccc@" + digest},
		{"tag w referencji niczego nie wybiera", "staging", apiImage + ":latest@" + digest, "", attested, &main, cli.ExitOK, apiImage + ":cccccccccccc@" + digest},
		{"obraz zbudowany na tagu (sprzed decyzji 40)", "staging", apiImage + "@" + digest, "", attested, &attestations{sourceRef: "refs/tags/v0.3.0", sourceCommit: commit}, cli.ExitUnmet, ""},
		{"podpis z innego commita niż etykieta", "staging", apiImage + "@" + digest, "", attested, &attestations{sourceRef: MainRef, sourceCommit: strings.Repeat("e", 40)}, cli.ExitUnmet, ""},
		{"bez atestacji GitHuba", "staging", apiImage + "@" + digest, "", attested, &attestations{refuse: true}, cli.ExitUnmet, ""},
		// Publikacje sprzed decyzji 37: pochodzenie bez SBOM.
		{"atestacja bez SBOM", "staging", apiImage + "@" + digest, "", attested[:1], &main, cli.ExitUnmet, ""},
		{"obcy obraz", "staging", "docker.io/library/nginx@" + digest, "", attested, &main, cli.ExitUnmet, ""},
		{"tag zamiast digestu", "staging", apiImage + ":0.4.0", "", attested, &main, cli.ExitFailure, ""},
		{"wydanie na produkcję", "prod", apiImage + "@" + digest, "0.4.0", attested, ptr(withRelease(main, releaseOf(apiImage+"@"+digest, "0.4.0", commit), commit)), cli.ExitOK, apiImage + ":0.4.0@" + digest},
		{"produkcja bez numeru wydania", "prod", apiImage + "@" + digest, "", attested, &main, cli.ExitFailure, ""},
		{"obraz z main bez atestacji wydania na produkcję", "prod", apiImage + "@" + digest, "0.4.0", attested, &main, cli.ExitUnmet, ""},
		{"atestacja wydania innego digestu", "prod", apiImage + "@" + digest, "0.4.0", attested, ptr(withRelease(main, releaseOf(apiImage+"@sha256:"+strings.Repeat("0", 64), "0.4.0", commit), commit)), cli.ExitUnmet, ""},
		{"wydanie podpisane z innego commita", "prod", apiImage + "@" + digest, "0.4.0", attested, ptr(withRelease(main, releaseOf(apiImage+"@"+digest, "0.4.0", commit), strings.Repeat("e", 40))), cli.ExitUnmet, ""},
		{"wydanie bez pakietu dowodów", "prod", apiImage + "@" + digest, "0.4.0", attested, ptr(withoutEvidence(released)), cli.ExitUnmet, ""},
		{"pakiet dowodów z czerwonym biegiem main", "prod", apiImage + "@" + digest, "0.4.0", attested, ptr(redMain(released)), cli.ExitUnmet, ""},
		// dev nie wymaga atestacji.
		{"bez atestacji w dev", "dev", apiImage + "@" + digest, "", attested[:1], nil, cli.ExitOK, apiImage + ":cccccccccccc@" + digest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asked := ""
			g := Gate{Registry: registry{commit: commit, predicates: tt.predicates}}
			if tt.att != nil {
				a := *tt.att
				a.askedRef = &asked
				g.Attestations = a
			}
			got, err := g.Admit(context.Background(), tt.ref, For(definition(t, tt.env), apiImage, tt.release))
			if code := cli.ExitCode(err); code != tt.wantCode {
				t.Fatalf("kod %d, oczekiwano %d (%v)", code, tt.wantCode, err)
			}
			if err == nil && got.Publication.Pinned() != tt.wantPinned {
				t.Errorf("przyjęty %s, oczekiwano %s", got.Publication.Pinned(), tt.wantPinned)
			}
			if tt.att != nil && tt.wantCode == cli.ExitOK && asked != MainRef {
				t.Errorf("pochodzenie sprawdzane z %q, oczekiwano %s", asked, MainRef)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
