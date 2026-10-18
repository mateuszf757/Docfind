package workflow

import (
	"strings"
	"testing"
)

const behaviourBase = `name: ci
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-24.04
    timeout-minutes: 15
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - name: testy
        run: ./bin/mise run test
  build:
    needs: test
    if: ${{ !cancelled() && needs.test.result == 'success' }}
    runs-on: ubuntu-24.04
    permissions:
      contents: read
    steps:
      - uses: ./.github/actions/build-image
`

func TestSameBehaviour(t *testing.T) {
	for _, tt := range []struct {
		name     string
		old, new string
		same     bool
	}{
		{"podbicie wersji akcji (Dependabot)", "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1", "actions/checkout@0123456789abcdef0123456789abcdef01234567 # v7.1.0", true},
		{"komentarz", "    timeout-minutes: 15\n", "    # limit z pomiaru\n    timeout-minutes: 15\n", true},
		{"nazwa kroku", "- name: testy", "- name: wersje, lint, testy", true},
		{"inna akcja", "actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", "someone/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", false},
		{"polecenie bramki", "run: ./bin/mise run test", "run: \"true\"", false},
		{"warunek zadania", "if: ${{ !cancelled() && needs.test.result == 'success' }}", "if: ${{ !cancelled() }}", false},
		{"uprawnienia", "contents: read", "contents: write", false},
		{"limit czasu", "timeout-minutes: 15", "timeout-minutes: 60", false},
		{"krok wyłączony warunkiem", "      - name: testy\n", "      - name: testy\n        if: false\n", false},
		{"krok, którego błąd się pomija", "      - name: testy\n", "      - name: testy\n        continue-on-error: true\n", false},
		{"nazwa zadania to nazwa statusu", "  build:\n    needs", "  build:\n    name: build-x\n    needs", false},
		{"zdarzenia", "on: [push, pull_request]", "on: [push]", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := strings.Replace(behaviourBase, tt.old, tt.new, 1)
			if changed == behaviourBase {
				t.Fatalf("zamiana %q nie zmieniła workflowu — popraw test", tt.old)
			}
			same, err := SameBehaviour([]byte(behaviourBase), []byte(changed))
			if err != nil {
				t.Fatal(err)
			}
			if same != tt.same {
				t.Errorf("SameBehaviour = %v; oczekiwano %v", same, tt.same)
			}
		})
	}
}

func TestSameBehaviourMissingFile(t *testing.T) {
	for _, tt := range []struct {
		name          string
		before, after []byte
		same          bool
	}{
		{"nowy workflow", nil, []byte(behaviourBase), false},
		{"usunięty workflow", []byte(behaviourBase), nil, false},
		{"bez pliku po obu stronach", nil, nil, true},
	} {
		same, err := SameBehaviour(tt.before, tt.after)
		if err != nil || same != tt.same {
			t.Errorf("%s: SameBehaviour = %v, %v; oczekiwano %v", tt.name, same, err, tt.same)
		}
	}
	if _, err := SameBehaviour([]byte(behaviourBase), []byte("jobs: [")); err == nil {
		t.Error("niepoprawny YAML bez błędu")
	}
}

func TestIsDefinition(t *testing.T) {
	for file, want := range map[string]bool{
		".github/workflows/ci.yml":                  true,
		".github/workflows/nightly.yaml":            true,
		".github/workflows/README.md":               false,
		".github/workflows/sub/x.yml":               false,
		".github/actions/build-image/action.yml":    true,
		".github/actions/build-image/run.sh":        false,
		".github/dependabot.yml":                    false,
		"tools/internal/core/workflow/testdata.yml": false,
	} {
		if got := IsDefinition(file); got != want {
			t.Errorf("IsDefinition(%s) = %v; oczekiwano %v", file, got, want)
		}
	}
}
