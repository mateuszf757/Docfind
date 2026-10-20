package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cacheWorkflow = `name: ci
on:
  pull_request:
  push:
    branches: [main]
permissions:
  contents: read
jobs:
  czyta:
    runs-on: ubuntu-24.04
    steps:
      - name: pamięć podręczna Go (odczyt)
        uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: ~/.cache/go-build
          key: go-dft-${{ runner.os }}-${{ hashFiles('tools/go.mod', 'tools/go.sum') }}
      - uses: astral-sh/setup-uv@c18668ad3cf93ea998bef934396af7bb5c839dc7 # v10.2.0
        with:
          enable-cache: false
      - uses: actions/setup-go@0aaccfd150d50ccaeb58ebd88d36e91967a5f35b # v6.0.0
        with:
          cache: false
  publikuje:
    if: github.event_name == 'push'
    runs-on: ubuntu-24.04
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: ~/.cache/go-build
          key: go-dft-${{ hashFiles('tools/go.sum') }}
  promuje:
    if: github.event_name == 'push'
    runs-on: ubuntu-24.04
    environment: production
    steps:
      - name: przez akcję złożoną
        uses: ./.github/actions/go
  pisze:
    if: github.event_name == 'push' && github.ref == 'refs/heads/main'
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: ~/.cache/go-build
          key: go-dft-${{ hashFiles('tools/go.sum') }}
  pisze-z-pr:
    runs-on: ubuntu-24.04
    steps:
      - name: zapis bez warunku
        uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: ~/.cache/go-build
          key: go-dft-stały
      - name: razem
        uses: actions/cache@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: ~/.cache/go-build
          key: go-${{ hashFiles('tools/go.sum') }}
      - uses: astral-sh/setup-uv@c18668ad3cf93ea998bef934396af7bb5c839dc7 # v10.2.0
      - uses: actions/setup-python@a26af69be951a213d495a4c3e4e4022e16d87065 # v5.6.0
        with:
          cache: pip
`

func TestCacheRules(t *testing.T) {
	w, err := Parse(".github/workflows/ci.yml", []byte(cacheWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	if !w.Jobs["promuje"].Environment || w.Jobs["czyta"].Environment || len(w.Jobs["czyta"].Steps) != 3 {
		t.Fatalf("odczyt kroków i środowiska: %+v", w.Jobs)
	}
	actions := map[string][]Step{"./.github/actions/go": {{Name: "odczyt w akcji", Uses: "actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9", With: map[string]string{"key": "go-${{ hashFiles('tools/go.sum') }}"}}}}
	found := CacheRules(w, actions)
	want := []string{
		`zadanie pisze-z-pr, krok "zapis bez warunku": zapisuje pamięć podręczną w zadaniu, które może ruszyć na pull_request`,
		`zadanie pisze-z-pr, krok "zapis bez warunku": klucz pamięci podręcznej bez hashFiles(…)`,
		`zadanie pisze-z-pr, krok "razem": actions/cache odtwarza i zapisuje w jednym kroku`,
		`zadanie pisze-z-pr, krok "astral-sh/setup-uv@c18668ad3cf93ea998bef934396af7bb5c839dc7": astral-sh/setup-uv z wbudowaną pamięcią podręczną — ustaw enable-cache: "false"`,
		`zadanie pisze-z-pr, krok "actions/setup-python@a26af69be951a213d495a4c3e4e4022e16d87065": actions/setup-python z wbudowaną pamięcią podręczną`,
		`zadanie promuje, krok "./.github/actions/go → odczyt w akcji": odtwarza pamięć podręczną w zadaniu z prawem zapisu (environment)`,
		`zadanie publikuje, krok "actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9": odtwarza pamięć podręczną w zadaniu z prawem zapisu (packages)`,
	}
	if len(found) != len(want) {
		t.Fatalf("CacheRules: %d uwag, oczekiwano %d:\n%s", len(found), len(want), strings.Join(found, "\n"))
	}
	for i, w := range want {
		if !strings.Contains(found[i], w) {
			t.Errorf("%d: %q\n   oczekiwano fragmentu %q", i, found[i], w)
		}
	}
}

// Workflow bez pull_request: zapis w zadaniu bez warunku jest dozwolony.
func TestCacheRulesWithoutPullRequest(t *testing.T) {
	w, err := Parse("noc.yml", []byte(`on: {schedule: [{cron: "17 3 * * *"}]}
jobs:
  pisze:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/cache/save@55cc8345863c7cc4c66a329aec7e433d2d1c52a9 # v6.1.0
        with:
          path: ~/.cache/go-build
          key: go-${{ hashFiles('tools/go.sum') }}
`))
	if err != nil {
		t.Fatal(err)
	}
	if found := CacheRules(w, nil); len(found) != 0 {
		t.Errorf("CacheRules: %q", found)
	}
}

func TestLoadActions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "actions", "go")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name: go\nruns:\n  using: composite\n  steps:\n    - name: odczyt\n      uses: actions/cache/restore@55cc8345863c7cc4c66a329aec7e433d2d1c52a9\n      with:\n        key: k\n    - run: echo\n      shell: bash\n"
	if err := os.WriteFile(filepath.Join(dir, "action.yml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	actions, err := LoadActions(root)
	if err != nil {
		t.Fatal(err)
	}
	got := actions["./.github/actions/go"]
	if len(got) != 2 || got[0].Name != "odczyt" || got[0].With["key"] != "k" || got[1].Uses != "" {
		t.Errorf("LoadActions: %+v", actions)
	}
}

// Workflowy i akcje złożone repozytorium spełniają reguły pamięci podręcznej.
func TestRepositoryCacheRules(t *testing.T) {
	root := "../../../.."
	workflows, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := LoadActions(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range workflows {
		if found := CacheRules(w, actions); len(found) != 0 {
			t.Errorf("%s:\n%s", w.File, strings.Join(found, "\n"))
		}
	}
}
