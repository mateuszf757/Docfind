package artifact

import (
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

const repo = "ghcr.io/mateuszf757/docfind-api"

var digest = "sha256:" + strings.Repeat("d", 64)

func TestRefs(t *testing.T) {
	a := Artifact{Repository: repo, Digest: digest, Version: "0.3.1-dev.4+abc1234"}
	if a.Ref() != repo+"@"+digest || a.Pinned() != repo+":0.3.1-dev.4-abc1234@"+digest {
		t.Errorf("Ref %q, Pinned %q", a.Ref(), a.Pinned())
	}
	for _, ref := range []string{repo + "@" + digest, repo + ":latest@" + digest} {
		r, d, err := ParseRef(ref)
		if err != nil || r != repo || d != digest {
			t.Errorf("ParseRef(%q) = %q, %q, %v", ref, r, d, err)
		}
	}
	if _, _, err := ParseRef(repo + ":0.3.0"); err == nil {
		t.Error("tag bez digestu przyjęty")
	}
}

func TestRequireAttestations(t *testing.T) {
	tests := []struct {
		name       string
		predicates []string
		want       int
	}{
		{"pochodzenie i SBOM", []string{ProvenancePredicates[1], SBOMPredicate}, cli.ExitOK},
		{"bez SBOM (publikacje sprzed decyzji 37)", []string{ProvenancePredicates[1]}, cli.ExitUnmet},
		{"bez pochodzenia", []string{SBOMPredicate}, cli.ExitUnmet},
	}
	for _, tt := range tests {
		p := Publication{Predicates: tt.predicates}
		if code := cli.ExitCode(p.RequireAttestations()); code != tt.want {
			t.Errorf("%s: kod %d, oczekiwano %d", tt.name, code, tt.want)
		}
	}
}

func TestShortCommit(t *testing.T) {
	if ShortCommit(strings.Repeat("a", 40)) != strings.Repeat("a", 12) || ShortCommit("abc") != "abc" {
		t.Error("ShortCommit")
	}
}
