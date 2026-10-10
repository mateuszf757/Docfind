// Package reposettings to ustawienia repozytorium zapisane jako kod:
// rulesety z .github/rulesets i środowiska GitHuba z .github/environments
// (decyzje 22 i 38). Pakiet czyta i sprawdza pliki; stosuje je etap
// reposettings przez adapter GitHuba.
package reposettings

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Katalogi ustawień względem korzenia repozytorium.
const (
	RulesetsDir     = ".github/rulesets"
	EnvironmentsDir = ".github/environments"
)

// Ruleset — reguła z .github/rulesets/<plik>.json: nazwa, po której reguła
// jest odnajdywana w repozytorium, i plik, który trafia do API bez zmian.
type Ruleset struct {
	Name string
	File string
}

// LoadRulesets czyta nazwy reguł; plik bez nazwy to błąd — po nazwie reguła
// jest aktualizowana zamiast dublowana.
func LoadRulesets(root string) ([]Ruleset, error) {
	files, err := filepath.Glob(filepath.Join(root, RulesetsDir, "*.json"))
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	var rulesets []Ruleset
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var r struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &r); err != nil || r.Name == "" {
			return nil, cli.Unmet("%s: brak nazwy reguły", file)
		}
		rulesets = append(rulesets, Ruleset{Name: r.Name, File: file})
	}
	return rulesets, nil
}

// Environment — środowisko GitHuba z .github/environments/<nazwa>.json.
type Environment struct {
	Name              string `json:"name"`
	WaitTimer         int    `json:"wait_timer"`
	PreventSelfReview bool   `json:"prevent_self_review"`
	// Reviewers — loginy użytkowników, których zgoda odblokowuje zadanie.
	Reviewers    []string     `json:"reviewers"`
	BranchPolicy BranchPolicy `json:"deployment_branch_policy"`
	// Policies — gałęzie (branch) i tagi (tag), z których wolno wdrażać.
	Policies []Policy `json:"policies"`
}

// BranchPolicy — skąd wolno wdrażać: chronione gałęzie albo własna lista.
type BranchPolicy struct {
	ProtectedBranches    bool `json:"protected_branches"`
	CustomBranchPolicies bool `json:"custom_branch_policies"`
}

// Policy — wzorzec gałęzi albo tagu dopuszczonych do środowiska.
type Policy struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// LoadEnvironments czyta definicje środowisk GitHuba ściśle: nieznane pole to
// literówka, która inaczej po cichu zostawiłaby środowisko bez ochrony.
func LoadEnvironments(root string) ([]Environment, error) {
	files, err := filepath.Glob(filepath.Join(root, EnvironmentsDir, "*.json"))
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	var envs []Environment
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var e Environment
		if err := dec.Decode(&e); err != nil {
			return nil, cli.Unmet("%s: %v", file, err)
		}
		if e.Name+".json" != filepath.Base(file) {
			return nil, cli.Unmet("%s: name=%q, a plik nazywa się %s", file, e.Name, filepath.Base(file))
		}
		if !e.BranchPolicy.CustomBranchPolicies || e.BranchPolicy.ProtectedBranches || len(e.Policies) == 0 {
			return nil, cli.Unmet("%s: środowisko bez własnej listy gałęzi i tagów przyjmuje wdrożenie z dowolnej gałęzi", file)
		}
		for _, p := range e.Policies {
			if p.Name == "" || (p.Type != "branch" && p.Type != "tag") {
				return nil, cli.Unmet("%s: polityka %+v — nazwa i typ branch albo tag", file, p)
			}
		}
		envs = append(envs, e)
	}
	return envs, nil
}
