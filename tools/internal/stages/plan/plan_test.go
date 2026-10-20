package plan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
)

type changes []string

func (c changes) ChangedFiles(context.Context, string) ([]string, error) { return c, nil }

// packages — `go list -deps` w pamięci: zapamiętuje korzenie.
type packages struct {
	deps  []string
	roots []string
}

func (p *packages) Deps(_ context.Context, pkgs ...string) ([]string, error) {
	p.roots = pkgs
	return p.deps, nil
}

// module — moduł z plikami poleceń klastrowych w cmd/dft.
func module(t *testing.T, imports string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "dft"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range ci.ClusterRootFiles {
		body := "package main\n"
		if name == "cluster.go" {
			body += imports
		}
		if err := os.WriteFile(filepath.Join(dir, "cmd", "dft", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestPlan(t *testing.T) {
	dir := module(t, "import (\n\t\"fmt\"\n\t\"example.com/m/internal/stages/drain\"\n)\n")
	pkgs := &packages{deps: []string{"fmt", "example.com/m/internal/stages/drain", "example.com/m/internal/core/contract"}}
	p := Planner{ModuleDir: dir, Changes: changes{"tools/internal/stages/budget/budget.go", "tools/internal/core/contract/thresholds.go"}, Packages: pkgs}
	got, err := p.Plan(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile != ci.ProfileHeavy || !slices.Equal(got.Files[ci.ProfileHeavy], []string{"tools/internal/core/contract/thresholds.go"}) {
		t.Errorf("plan: %+v", got)
	}
	// Korzenie tylko z modułu — biblioteka standardowa nie jest korzeniem.
	if !slices.Equal(pkgs.roots, []string{"example.com/m/internal/stages/drain"}) {
		t.Errorf("korzenie: %q", pkgs.roots)
	}
}

func TestPlanErrors(t *testing.T) {
	if _, err := (Planner{ModuleDir: module(t, "")}).ClusterDirs(context.Background()); err == nil {
		t.Error("pliki klastrowe bez importów modułu przepuszczone")
	}
	if _, err := (Planner{ModuleDir: t.TempDir()}).ClusterDirs(context.Background()); err == nil {
		t.Error("katalog bez go.mod przepuszczony")
	}
	broken := Planner{ModuleDir: module(t, "import \"example.com/m/internal/x\"\n"), Changes: changesErr{}, Packages: &packages{}}
	if _, err := broken.Plan(context.Background(), "abc"); err == nil {
		t.Error("błąd gita przepuszczony")
	}
}

type changesErr struct{}

func (changesErr) ChangedFiles(context.Context, string) ([]string, error) {
	return nil, errors.New("nieznany commit")
}
