package workflow

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// DependabotFile — konfiguracja Dependabota względem korzenia repozytorium.
const DependabotFile = ".github/dependabot.yml"

// ActionDirs — katalogi akcji złożonych (z action.yml albo action.yaml),
// względem korzenia repozytorium, posortowane.
func ActionDirs(root string) ([]string, error) {
	var dirs []string
	for _, name := range []string{"action.yml", "action.yaml"} {
		found, err := filepath.Glob(filepath.Join(root, ActionsDir, "*", name))
		if err != nil {
			return nil, err
		}
		for _, f := range found {
			rel, err := filepath.Rel(root, filepath.Dir(f))
			if err != nil {
				return nil, err
			}
			dirs = append(dirs, filepath.ToSlash(rel))
		}
	}
	slices.Sort(dirs)
	return slices.Compact(dirs), nil
}

// UncoveredActions zwraca katalogi akcji złożonych, których nie obejmuje
// żaden wpis github-actions w konfiguracji Dependabota. `directory: /`
// czyta tylko .github/workflows i action.yml w korzeniu, więc przypięcia
// w akcjach złożonych bez wpisu w `directories` starzeją się po cichu —
// tak build-image została przy upload-artifact v7.0.1, gdy ci.yml dostał
// v7.0.2. Wzorce `directories` to ścieżki od korzenia z `*` na jeden
// segment (path.Match); `**` traktowane jest jak dowolny przyrostek.
func UncoveredActions(config []byte, dirs []string) ([]string, error) {
	var cfg struct {
		Updates []struct {
			Ecosystem   string   `json:"package-ecosystem"`
			Directory   string   `json:"directory"`
			Directories []string `json:"directories"`
		} `json:"updates"`
	}
	if err := yaml.Unmarshal(config, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", DependabotFile, err)
	}
	var patterns []string
	for _, u := range cfg.Updates {
		if u.Ecosystem != "github-actions" {
			continue
		}
		if u.Directory != "" {
			patterns = append(patterns, u.Directory)
		}
		patterns = append(patterns, u.Directories...)
	}
	var uncovered []string
	for _, dir := range dirs {
		if !slices.ContainsFunc(patterns, func(p string) bool { return covers(p, "/"+dir) }) {
			uncovered = append(uncovered, dir)
		}
	}
	return uncovered, nil
}

func covers(pattern, dir string) bool {
	pattern = "/" + strings.Trim(pattern, "/")
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		return dir == prefix || strings.HasPrefix(dir, prefix+"/")
	}
	ok, err := path.Match(pattern, dir)
	return err == nil && ok
}

// DependabotCoverage — UncoveredActions dla repozytorium w root.
func DependabotCoverage(root string) ([]string, error) {
	dirs, err := ActionDirs(root)
	if err != nil || len(dirs) == 0 {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(DependabotFile)))
	if err != nil {
		return nil, err
	}
	return UncoveredActions(raw, dirs)
}
