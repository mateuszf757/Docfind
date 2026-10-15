package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repoRoot = "../../../.."

// promoteBeforeFix — układ zadań z ci.yml sprzed decyzji 47, na którym
// promote nie wystartował na tagu v0.3.1 (bieg 38164714816): build
// pominięty łańcuchem od test, vulns rusza dzięki !cancelled(), a promote
// z warunkiem bez funkcji statusu — nie.
const promoteBeforeFix = `
name: ci
on: {push: {tags: ["v*"]}}
jobs:
  test:
    if: github.ref_type != 'tag'
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
  build:
    needs: test
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
  release:
    if: github.ref_type == 'tag'
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
  attest-release:
    needs: release
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
  vulns:
    needs: [build, release]
    if: ${{ !cancelled() && (needs.build.result == 'success' || needs.release.result == 'success') }}
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
  promote:
    needs: [release, attest-release, vulns]
    if: github.ref_type == 'tag'
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
`

func violationsOf(t *testing.T, content string) []string {
	t.Helper()
	w, err := Parse("ci.yml", []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return SkipChain(w)
}

func jobsIn(violations []string) []string {
	var jobs []string
	for _, v := range violations {
		_, rest, _ := strings.Cut(v, "zadanie ")
		name, _, _ := strings.Cut(rest, " ")
		jobs = append(jobs, name)
	}
	return jobs
}

// TestSkipChainRejectsPromoteBeforeFix — wariant, który reguła musi
// odrzucić: dokładnie promote z v0.3.1, a do tego build i attest-release,
// które dziedziczą pominięcie tak samo.
func TestSkipChainRejectsPromoteBeforeFix(t *testing.T) {
	got := jobsIn(violationsOf(t, promoteBeforeFix))
	want := []string{"attest-release", "build", "promote"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("naruszenia w %v, oczekiwano %v", got, want)
	}
}

// promoteAfterFix — ten sam graf z !cancelled() i jawnymi wynikami
// bezpośrednich needs.
const promoteAfterFix = `
jobs:
  test:
    if: github.ref_type != 'tag'
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
  build:
    needs: test
    if: ${{ !cancelled() && needs.test.result == 'success' }}
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
  release:
    if: github.ref_type == 'tag'
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
  attest-release:
    needs: release
    if: ${{ !cancelled() && needs.release.result == 'success' }}
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
  vulns:
    needs: [build, release]
    if: ${{ !cancelled() && (needs.build.result == 'success' || needs.release.result == 'success') }}
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
  promote:
    needs: [release, attest-release, vulns]
    if: ${{ !cancelled() && github.ref_type == 'tag' && needs.release.result == 'success' && needs.attest-release.result == 'success' && needs.vulns.result == 'success' }}
    runs-on: ubuntu-24.04
    steps: [{run: "true"}]
`

// TestSkipChainAcceptsExplicitResults — ten sam graf po poprawce przechodzi.
func TestSkipChainAcceptsExplicitResults(t *testing.T) {
	if got := violationsOf(t, promoteAfterFix); len(got) != 0 {
		t.Fatalf("oczekiwano zero naruszeń, są: %q", got)
	}
}

func TestSkipChainCases(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name: "łańcuch bez warunków",
			content: `
jobs:
  a: {runs-on: x, steps: [{run: "true"}]}
  b: {needs: a, runs-on: x, steps: [{run: "true"}]}
  c: {needs: [b], runs-on: x, steps: [{run: "true"}]}
`,
		},
		{
			name: "warunek tylko przy korzeniu, bez needs",
			content: `
jobs:
  a: {if: "github.event_name == 'push'", runs-on: x, steps: [{run: "true"}]}
`,
		},
		{
			name: "warunkowy przodek pośredni, zadanie bez if",
			content: `
jobs:
  a: {if: "false", runs-on: x, steps: [{run: "true"}]}
  b: {needs: a, if: "${{ always() }}", runs-on: x, steps: [{run: "true"}]}
  c: {needs: b, runs-on: x, steps: [{run: "true"}]}
`,
			want: []string{"c"},
		},
		{
			name: "if jako wartość logiczna",
			content: `
jobs:
  a: {if: false, runs-on: x, steps: [{run: "true"}]}
  b: {needs: a, runs-on: x, steps: [{run: "true"}]}
`,
			want: []string{"b"},
		},
		{
			name: "każda funkcja statusu wyłącza domyślne success()",
			content: `
jobs:
  a: {if: "false", runs-on: x, steps: [{run: "true"}]}
  b: {needs: a, if: "success() && true", runs-on: x, steps: [{run: "true"}]}
  c: {needs: a, if: "${{ failure() }}", runs-on: x, steps: [{run: "true"}]}
  d: {needs: a, if: "${{ !cancelled() }}", runs-on: x, steps: [{run: "true"}]}
`,
		},
		{
			name: "nieznana nazwa w needs nie wysadza reguły",
			content: `
jobs:
  b: {needs: [brak], runs-on: x, steps: [{run: "true"}]}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jobsIn(violationsOf(t, tt.content))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("naruszenia w %v, oczekiwano %v", got, tt.want)
			}
		})
	}
}

func TestParseRejectsBadNeeds(t *testing.T) {
	_, err := Parse("ci.yml", []byte("jobs:\n  a: {needs: {x: 1}}\n"))
	if err == nil || !strings.Contains(err.Error(), "needs") {
		t.Fatalf("oczekiwano błędu needs, jest %v", err)
	}
}

func TestLoadNeedsWorkflows(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("katalog bez workflowów przeszedł")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, Dir, "x.yaml"), []byte("jobs: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Load(dir)
	if err != nil || len(w) != 1 || w[0].File != filepath.Join(Dir, "x.yaml") {
		t.Fatalf("Load: %+v, %v", w, err)
	}
}

// TestRepositoryWorkflows — workflowy repozytorium spełniają regułę. Ten test
// jest czerwony na ci.yml sprzed decyzji 47.
func TestRepositoryWorkflows(t *testing.T) {
	workflows, err := Load(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range workflows {
		for _, v := range SkipChain(w) {
			t.Error(v)
		}
	}
}
