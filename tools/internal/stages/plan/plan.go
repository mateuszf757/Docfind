// Package plan to profil integracji zmiany (decyzja 75): które z bramek
// klastrowych PR ma przejść przed scaleniem. Reguły ścieżek są w core/ci;
// tu jest graf pakietów klastrowych — importy plików tools/cmd/dft
// z poleceniami, które uruchamia zadanie cluster, i ich zależności.
//
// Plan liczy kod z commita bazowego, a z PR-a czyta tylko listę zmienionych
// plików: zmiana nie obniży sobie profilu, zmieniając reguły albo graf.
package plan

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
)

// Changes — pliki zmienione względem commita bazowego (adapters/git).
type Changes interface {
	ChangedFiles(ctx context.Context, base string) ([]string, error)
}

// Packages — zależności pakietów Go (adapters/gotool).
type Packages interface {
	Deps(ctx context.Context, pkgs ...string) ([]string, error)
}

// Planner — plan zmiany: moduł tools/ z commita bazowego w ModuleDir
// (katalog tools repozytorium), zmiany z repozytorium PR-a.
type Planner struct {
	ModuleDir string
	Changes   Changes
	Packages  Packages
}

// Plan zwraca profil zmiany względem commita base.
func (p Planner) Plan(ctx context.Context, base string) (ci.Plan, error) {
	changed, err := p.Changes.ChangedFiles(ctx, base)
	if err != nil {
		return ci.Plan{}, err
	}
	dirs, err := p.ClusterDirs(ctx)
	if err != nil {
		return ci.Plan{}, err
	}
	return ci.PlanFor(changed, dirs), nil
}

// ClusterDirs — katalogi pakietów modułu (od korzenia repozytorium, np.
// tools/internal/stages/drain), od których zależą polecenia klastrowe.
func (p Planner) ClusterDirs(ctx context.Context) (map[string]bool, error) {
	module, err := modulePath(filepath.Join(p.ModuleDir, "go.mod"))
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, name := range ci.ClusterRootFiles {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(p.ModuleDir, "cmd", "dft", name), nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, imp := range f.Imports {
			if path, err := strconv.Unquote(imp.Path.Value); err == nil && strings.HasPrefix(path, module+"/") {
				roots = append(roots, path)
			}
		}
	}
	if len(roots) == 0 {
		return nil, errors.New("pliki poleceń klastrowych bez importów modułu — graf pakietów klastrowych byłby pusty")
	}
	deps, err := p.Packages.Deps(ctx, roots...)
	if err != nil {
		return nil, err
	}
	dirs := map[string]bool{}
	for _, d := range deps {
		if rel, ok := strings.CutPrefix(d, module+"/"); ok {
			dirs["tools/"+rel] = true
		}
	}
	return dirs, nil
}

func modulePath(gomod string) (string, error) {
	f, err := os.Open(gomod)
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if m, ok := strings.CutPrefix(strings.TrimSpace(s.Text()), "module "); ok {
			return strings.TrimSpace(m), nil
		}
	}
	return "", fmt.Errorf("%s: brak dyrektywy module", gomod)
}
