// Package workflow to reguły grafu zadań workflowów GitHub Actions, których
// nie sprawdza ani actionlint, ani zizmor (decyzja 47).
//
// Zadanie bez funkcji statusu w `if` (albo bez `if`) dostaje domyślne
// success(), a ono patrzy na wszystkich przodków, nie tylko na bezpośrednie
// needs: „a failure or skip applies to all jobs in the dependency chain from
// the point of failure or skip onwards” (dokumentacja `jobs.<id>.needs`).
// Tak na tagu v0.3.1 pominięty build — przodek vulns — pominął promote,
// choć release, attest-release i vulns skończyły się sukcesem, a bieg był
// zielony (bieg 38164714816). Pominięte zadanie nie jest zaliczone, ale
// wygląda na szare, nie na czerwone.
package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Dir — katalog workflowów względem korzenia repozytorium.
const Dir = ".github/workflows"

// Workflow — plik workflowu i jego zadania.
type Workflow struct {
	// File — ścieżka względem korzenia repozytorium, do komunikatów.
	File string
	Jobs map[string]Job
}

// Job — zadanie: bezpośrednie zależności i warunek (pusty — brak `if`).
type Job struct {
	Needs []string
	If    string
}

// statusFunction — funkcje statusu GitHub Actions. Obecność którejkolwiek
// w `if` wyłącza domyślne success() — wtedy zadanie samo mówi, na które
// wyniki patrzy.
var statusFunction = regexp.MustCompile(`\b(success|failure|always|cancelled)\s*\(\s*\)`)

// HasStatusFunction mówi, czy warunek zawiera funkcję statusu.
func HasStatusFunction(condition string) bool {
	return statusFunction.MatchString(condition)
}

type rawJob struct {
	Needs any `json:"needs"`
	If    any `json:"if"`
}

type rawWorkflow struct {
	Jobs map[string]rawJob `json:"jobs"`
}

// Parse czyta workflow. Pola poza needs i if nie mają tu znaczenia, więc
// odczyt nie jest ścisły — ścisłość zapewnia actionlint.
func Parse(file string, raw []byte) (Workflow, error) {
	var r rawWorkflow
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return Workflow{}, fmt.Errorf("%s: %w", file, err)
	}
	w := Workflow{File: file, Jobs: map[string]Job{}}
	for name, j := range r.Jobs {
		needs, err := stringList(j.Needs)
		if err != nil {
			return Workflow{}, fmt.Errorf("%s: zadanie %s: needs: %w", file, name, err)
		}
		cond := ""
		if j.If != nil {
			cond = strings.TrimSpace(fmt.Sprint(j.If))
		}
		w.Jobs[name] = Job{Needs: needs, If: cond}
	}
	return w, nil
}

// stringList — needs jako jeden napis albo lista napisów.
func stringList(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("element %v nie jest nazwą zadania", e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%v nie jest nazwą zadania ani listą nazw", v)
	}
}

// Load czyta wszystkie workflowy repozytorium (*.yml i *.yaml).
func Load(root string) ([]Workflow, error) {
	var files []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		found, err := filepath.Glob(filepath.Join(root, Dir, pattern))
		if err != nil {
			return nil, err
		}
		files = append(files, found...)
	}
	if len(files) == 0 {
		return nil, errors.New("brak workflowów w " + Dir)
	}
	slices.Sort(files)
	workflows := make([]Workflow, 0, len(files))
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		w, err := Parse(rel, raw)
		if err != nil {
			return nil, err
		}
		workflows = append(workflows, w)
	}
	return workflows, nil
}

// ancestors zwraca wszystkich przodków zadania (przechodnio po needs),
// posortowanych. Nieznane nazwy w needs pomija — zgłasza je actionlint.
func (w Workflow) ancestors(name string) []string {
	seen := map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		for _, parent := range w.Jobs[n].Needs {
			if seen[parent] {
				continue
			}
			if _, ok := w.Jobs[parent]; !ok {
				continue
			}
			seen[parent] = true
			visit(parent)
		}
	}
	visit(name)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// SkipChain zwraca naruszenia reguły „pominięte to nie zaliczone”: zadanie,
// którego przodek ma warunek (więc bywa pomijany), a które samo nie ma
// funkcji statusu w `if`. Takie zadanie jest pomijane razem z przodkiem,
// także gdy jego bezpośrednie needs są zielone. Poprawka: `!cancelled()`
// i jawne `needs.<zadanie>.result == 'success'` dla każdej zależności.
func SkipChain(w Workflow) []string {
	names := make([]string, 0, len(w.Jobs))
	for n := range w.Jobs {
		names = append(names, n)
	}
	slices.Sort(names)
	var found []string
	for _, name := range names {
		job := w.Jobs[name]
		if HasStatusFunction(job.If) {
			continue
		}
		var conditional []string
		for _, a := range w.ancestors(name) {
			if w.Jobs[a].If != "" {
				conditional = append(conditional, a)
			}
		}
		if len(conditional) == 0 {
			continue
		}
		found = append(found, fmt.Sprintf(
			"%s: zadanie %s nie ma funkcji statusu w if, a przodkowie z warunkiem (%s) bywają pomijani — ich pominięcie pominie też %s, nawet gdy bezpośrednie needs są zielone; dopisz !cancelled() i needs.<zadanie>.result == 'success' (decyzja 47)",
			w.File, name, strings.Join(conditional, ", "), name))
	}
	return found
}
