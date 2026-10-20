package workflow

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestUncoveredActions(t *testing.T) {
	dirs := []string{".github/actions/build-image", ".github/actions/setup"}
	for _, tt := range []struct {
		name   string
		config string
		want   []string
	}{
		{"tylko korzeń", `version: 2
updates:
  - package-ecosystem: github-actions
    directory: /
`, dirs},
		{"wzorzec na akcje złożone", `version: 2
updates:
  - package-ecosystem: github-actions
    directories: ["/", "/.github/actions/*"]
`, nil},
		{"jedna akcja wprost", `version: 2
updates:
  - package-ecosystem: github-actions
    directories: ["/", "/.github/actions/build-image/"]
`, []string{".github/actions/setup"}},
		{"podwójna gwiazdka", `version: 2
updates:
  - package-ecosystem: github-actions
    directories: ["/.github/**"]
`, nil},
		{"katalog w innym ekosystemie się nie liczy", `version: 2
updates:
  - package-ecosystem: github-actions
    directory: /
  - package-ecosystem: docker
    directories: ["/.github/actions/*"]
`, dirs},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := UncoveredActions([]byte(tt.config), dirs)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("UncoveredActions = %q, oczekiwano %q", got, tt.want)
			}
		})
	}
	if _, err := UncoveredActions([]byte("updates: ["), dirs); err == nil {
		t.Error("niepoprawny YAML bez błędu")
	}
}

func TestDependabotCoverage(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DependabotCoverage(root)
	if err != nil || got != nil {
		t.Fatalf("bez akcji złożonych: %q, %v", got, err)
	}
	write(".github/actions/build-image/action.yml", "name: build\n")
	write(".github/actions/notatki/README.md", "bez action.yml to nie akcja\n")
	write(DependabotFile, "version: 2\nupdates:\n  - package-ecosystem: github-actions\n    directory: /\n")
	got, err = DependabotCoverage(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{".github/actions/build-image"}) {
		t.Errorf("DependabotCoverage = %q", got)
	}
}

// Konfiguracja repozytorium obejmuje każdą akcję złożoną.
func TestRepositoryDependabotCoversActions(t *testing.T) {
	got, err := DependabotCoverage("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("akcje złożone poza Dependabotem: %q", got)
	}
}
