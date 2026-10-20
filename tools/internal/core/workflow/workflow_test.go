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

// TestRepositoryWorkflows — workflowy repozytorium spełniają obie reguły.
// Ten test jest czerwony na ci.yml sprzed decyzji 47 (łańcuch pominięć)
// i sprzed decyzji 49 (build z packages: write na PR-ach).
func TestRepositoryWorkflows(t *testing.T) {
	workflows, err := Load(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) < 2 {
		t.Fatalf("workflowy: %d", len(workflows))
	}
	for _, w := range workflows {
		if len(w.Events) == 0 {
			t.Errorf("%s: brak zdarzeń z on", w.File)
		}
		for _, v := range SkipChain(w) {
			t.Error(v)
		}
		for _, v := range WriteOnPullRequest(w) {
			t.Error(v)
		}
	}
}

// buildBeforeFix — układ ci.yml sprzed decyzji 49: jedno zadanie build na PR
// i na push, z packages: write, bo publikuje przy push.
const buildBeforeFix = `
on:
  pull_request:
  push: {branches: [main], tags: ["v*"]}
  schedule: [{cron: "17 3 * * *"}]
permissions:
  contents: read
jobs:
  test:
    if: github.ref_type != 'tag'
    runs-on: x
    steps: [{run: "true"}]
  build:
    needs: test
    if: ${{ !cancelled() && needs.test.result == 'success' }}
    permissions: {contents: read, packages: write}
    runs-on: x
    steps: [{run: "true"}]
  attest:
    needs: build
    if: ${{ !cancelled() && github.event_name == 'push' && needs.build.result == 'success' }}
    permissions: {id-token: write, attestations: write, packages: write}
    runs-on: x
    steps: [{run: "true"}]
  release:
    if: github.ref_type == 'tag'
    permissions: {contents: read, packages: write}
    runs-on: x
    steps: [{run: "true"}]
  attest-release:
    needs: release
    if: ${{ !cancelled() && needs.release.result == 'success' }}
    permissions: {id-token: write, packages: write}
    runs-on: x
    steps: [{run: "true"}]
  vulns-issue:
    needs: [test]
    if: ${{ !cancelled() && github.event_name == 'schedule' }}
    permissions: {contents: read, issues: write}
    runs-on: x
    steps: [{run: "true"}]
`

// TestWriteOnPullRequestRejectsBuildBeforeFix — wariant, który reguła musi
// odrzucić: build z packages: write biegnący na PR-ach (logi zadań
// 115819177283 i 113359704777 — „Packages: write”). Zadania z warunkiem
// wykluczającym PR (push, tag, schedule — także przez jawny wynik
// zadania z takim warunkiem) przechodzą.
func TestWriteOnPullRequestRejectsBuildBeforeFix(t *testing.T) {
	w, err := Parse("ci.yml", []byte(buildBeforeFix))
	if err != nil {
		t.Fatal(err)
	}
	got := jobsIn(WriteOnPullRequest(w))
	if strings.Join(got, ",") != "build" {
		t.Fatalf("naruszenia w %v, oczekiwano [build]", got)
	}
	fixed := strings.Replace(buildBeforeFix,
		"    if: ${{ !cancelled() && needs.test.result == 'success' }}\n    permissions: {contents: read, packages: write}",
		"    if: ${{ !cancelled() && github.event_name == 'push' && github.ref_type == 'branch' && needs.test.result == 'success' }}\n    permissions: {contents: read, packages: write}", 1)
	w, err = Parse("ci.yml", []byte(fixed))
	if err != nil {
		t.Fatal(err)
	}
	if got := WriteOnPullRequest(w); len(got) != 0 {
		t.Fatalf("po poprawce oczekiwano zero naruszeń, są: %q", got)
	}
}

func TestWriteOnPullRequestCases(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{
			name: "workflow bez pull_request nie dotyczy",
			content: `
on: {push: {branches: [main]}}
jobs:
  a: {permissions: {packages: write}, runs-on: x, steps: [{run: "true"}]}
`,
		},
		{
			name: "uprawnienia z workflowu, write-all",
			content: `
on: pull_request
permissions: write-all
jobs:
  a: {runs-on: x, steps: [{run: "true"}]}
`,
			want: []string{"a"},
		},
		{
			name: "pull_request_target też jest PR-em",
			content: `
on: [pull_request_target]
jobs:
  a: {permissions: {contents: write}, runs-on: x, steps: [{run: "true"}]}
`,
			want: []string{"a"},
		},
		{
			name: "warunek z alternatywą traktowany jako spełniony",
			content: `
on: pull_request
jobs:
  a: {if: "github.event_name == 'push' || github.event_name == 'pull_request'", permissions: {packages: write}, runs-on: x, steps: [{run: "true"}]}
`,
			want: []string{"a"},
		},
		{
			name: "zadanie bez funkcji statusu za zadaniem tylko na push",
			content: `
on: [pull_request, push]
jobs:
  p: {if: "github.event_name == 'push'", runs-on: x, steps: [{run: "true"}]}
  q: {needs: p, permissions: {packages: write}, runs-on: x, steps: [{run: "true"}]}
`,
		},
		{
			name: "always() za zadaniem tylko na push — nadal osiągalne",
			content: `
on: [pull_request, push]
jobs:
  p: {if: "github.event_name == 'push'", runs-on: x, steps: [{run: "true"}]}
  q: {needs: p, if: "${{ always() }}", permissions: {packages: write}, runs-on: x, steps: [{run: "true"}]}
`,
			want: []string{"q"},
		},
		{
			name: "odczyt i none nie są zapisem",
			content: `
on: pull_request
permissions: {}
jobs:
  a: {permissions: {contents: read, pull-requests: read, issues: none}, runs-on: x, steps: [{run: "true"}]}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, err := Parse("ci.yml", []byte(tt.content))
			if err != nil {
				t.Fatal(err)
			}
			got := jobsIn(WriteOnPullRequest(w))
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("naruszenia w %v, oczekiwano %v", got, tt.want)
			}
		})
	}
}

func TestConjuncts(t *testing.T) {
	got := conjuncts("${{ !cancelled() && (a == 'x && y' || b) && needs.c.result == 'success' }}")
	want := []string{"!cancelled()", "(a == 'x && y' || b)", "needs.c.result == 'success'"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("człony %q, oczekiwano %q", got, want)
	}
}

const requiredWorkflow = `on: [pull_request, push]
jobs:
  test:
    if: github.ref_type != 'tag'
    runs-on: ubuntu-24.04
  build:
    needs: test
    runs-on: ubuntu-24.04
  fast-feedback:
    needs: [test, build]
    if: ${{ always() && github.event_name == 'pull_request' }}
    runs-on: ubuntu-24.04
  integration:
    name: integracja
    needs: build
    if: always()
    runs-on: ubuntu-24.04
  half:
    needs: build
    if: ${{ always() && needs.build.result == 'success' }}
    runs-on: ubuntu-24.04
`

func TestRequiredChecks(t *testing.T) {
	ci, err := Parse("ci.yml", []byte(requiredWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	nightly, err := Parse("noc.yml", []byte("on: schedule\njobs:\n  nightly:\n    if: always()\n    runs-on: ubuntu-24.04\n"))
	if err != nil {
		t.Fatal(err)
	}
	workflows := []Workflow{ci, nightly}
	for _, tt := range []struct {
		check, want string
	}{
		{"fast-feedback", ""},
		{"integracja", ""},
		{"test", "warunek bez always()"},
		{"build", "warunek bez always()"},
		{"half", "warunek bez always()"},
		{"integration", "żadne zadanie"},
		{"nightly", "żadne zadanie"},
	} {
		found := RequiredChecks(workflows, []string{tt.check})
		switch {
		case tt.want == "" && len(found) > 0:
			t.Errorf("%s: %v", tt.check, found)
		case tt.want != "" && (len(found) != 1 || !strings.Contains(found[0], tt.want)):
			t.Errorf("%s: %v; oczekiwano %q", tt.check, found, tt.want)
		}
	}
}

func TestAlwaysRuns(t *testing.T) {
	for cond, want := range map[string]bool{
		"always()": true,
		"${{ always() && github.event_name == 'pull_request' }}": true,
		"${{ github.event_name == 'pull_request' && always() }}": true,
		"${{ always() && needs.build.result == 'success' }}":     false,
		"${{ !cancelled() }}":          false,
		"${{ always() || failure() }}": false,
		"":                             false,
	} {
		if got := alwaysRuns(cond); got != want {
			t.Errorf("alwaysRuns(%q) = %v; oczekiwano %v", cond, got, want)
		}
	}
}

func TestLayerless(t *testing.T) {
	w, err := Parse(CIFile, []byte("on: push\njobs:\n  test:\n    runs-on: x\n  nowe:\n    runs-on: x\n"))
	if err != nil {
		t.Fatal(err)
	}
	found := Layerless(w, map[string]string{"test": "W1", "stare": "W2"})
	if len(found) != 2 || !strings.Contains(found[0], "zadanie nowe bez warstwy") || !strings.Contains(found[1], "zadanie stare, którego nie ma") {
		t.Errorf("Layerless = %q", found)
	}
	w.File = Dir + "/inny.yml"
	if found := Layerless(w, nil); len(found) != 0 {
		t.Errorf("inny workflow oceniony: %q", found)
	}
}

func TestAggregatorCoverage(t *testing.T) {
	w, err := Parse("ci.yml", []byte(`on: [pull_request, push]
jobs:
  lint:
    runs-on: ubuntu-24.04
  test:
    runs-on: ubuntu-24.04
  build-publish:
    if: github.event_name == 'push'
    runs-on: ubuntu-24.04
  cluster:
    needs: [test]
    runs-on: ubuntu-24.04
  fast-feedback:
    needs: [test]
    if: ${{ always() && github.event_name == 'pull_request' }}
    runs-on: ubuntu-24.04
  integration:
    needs: [cluster]
    if: ${{ always() && github.event_name == 'pull_request' }}
    runs-on: ubuntu-24.04
`))
	if err != nil {
		t.Fatal(err)
	}
	layers := map[string]string{"lint": "W1", "test": "W1", "build-publish": "W1", "fast-feedback": "W1", "cluster": "W2", "integration": "W2"}
	aggregators := map[string]string{"fast-feedback": "W1", "integration": "W2"}
	found := AggregatorCoverage(w, layers, aggregators)
	// lint rusza na PR-ze, a fast-feedback na niego nie czeka; build-publish
	// biegnie tylko na push, więc go nie dotyczy.
	if len(found) != 1 || !strings.Contains(found[0], "zadanie zbierające fast-feedback nie czeka na lint (warstwa W1") {
		t.Errorf("AggregatorCoverage: %q", found)
	}
	noPR, _ := Parse("noc.yml", []byte("on: [schedule]\njobs:\n  lint:\n    runs-on: ubuntu-24.04\n  fast-feedback:\n    runs-on: ubuntu-24.04\n"))
	if found := AggregatorCoverage(noPR, layers, aggregators); len(found) != 0 {
		t.Errorf("workflow bez PR-ów: %q", found)
	}
}
