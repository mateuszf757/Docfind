// Package workflow to reguły workflowów GitHub Actions, których nie
// sprawdza ani actionlint, ani zizmor: pominięcie przechodzi łańcuchem needs
// (decyzja 47), a zadanie osiągalne z pull_request nie ma tokenu z prawem
// zapisu (decyzja 49).
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

// Workflow — plik workflowu, jego zdarzenia, uprawnienia i zadania.
type Workflow struct {
	// File — ścieżka względem korzenia repozytorium, do komunikatów.
	File string
	// Events — zdarzenia z `on`.
	Events []string
	// Permissions — uprawnienia tokenu na poziomie workflowu; nil — brak
	// klucza (domyślne repozytorium).
	Permissions *Permissions
	Jobs        map[string]Job
}

// Job — zadanie: bezpośrednie zależności, warunek (pusty — brak `if`)
// i uprawnienia (nil — dziedziczy z workflowu).
type Job struct {
	Needs       []string
	If          string
	Permissions *Permissions
}

// Permissions — uprawnienia tokenu: skrót (`read-all`, `write-all`) albo
// zakresy z poziomem (`read`, `write`, `none`).
type Permissions struct {
	All    string
	Scopes map[string]string
}

// Writes zwraca posortowane zakresy z prawem zapisu; `write-all` — „wszystkie”.
func (p *Permissions) Writes() []string {
	if p == nil {
		return nil
	}
	if p.All == "write-all" {
		return []string{"wszystkie (write-all)"}
	}
	var out []string
	for scope, level := range p.Scopes {
		if level == "write" {
			out = append(out, scope)
		}
	}
	slices.Sort(out)
	return out
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
	Needs       any `json:"needs"`
	If          any `json:"if"`
	Permissions any `json:"permissions"`
}

type rawWorkflow struct {
	// On — klucz `on`. YAML 1.1 czyta gołe `on` jako wartość logiczną, więc
	// po konwersji do JSON-a klucz bywa napisem "true".
	On          any               `json:"on"`
	True        any               `json:"true"`
	Permissions any               `json:"permissions"`
	Jobs        map[string]rawJob `json:"jobs"`
}

// Parse czyta workflow. Pola poza zdarzeniami, uprawnieniami, needs i if
// nie mają tu znaczenia, więc odczyt nie jest ścisły — ścisłość zapewnia
// actionlint.
func Parse(file string, raw []byte) (Workflow, error) {
	var r rawWorkflow
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return Workflow{}, fmt.Errorf("%s: %w", file, err)
	}
	on := r.On
	if on == nil {
		on = r.True
	}
	events, err := eventList(on)
	if err != nil {
		return Workflow{}, fmt.Errorf("%s: on: %w", file, err)
	}
	perms, err := permissions(r.Permissions)
	if err != nil {
		return Workflow{}, fmt.Errorf("%s: permissions: %w", file, err)
	}
	w := Workflow{File: file, Events: events, Permissions: perms, Jobs: map[string]Job{}}
	for name, j := range r.Jobs {
		needs, err := stringList(j.Needs)
		if err != nil {
			return Workflow{}, fmt.Errorf("%s: zadanie %s: needs: %w", file, name, err)
		}
		cond := ""
		if j.If != nil {
			cond = strings.TrimSpace(fmt.Sprint(j.If))
		}
		jobPerms, err := permissions(j.Permissions)
		if err != nil {
			return Workflow{}, fmt.Errorf("%s: zadanie %s: permissions: %w", file, name, err)
		}
		w.Jobs[name] = Job{Needs: needs, If: cond, Permissions: jobPerms}
	}
	return w, nil
}

// eventList — `on` jako napis, lista albo mapa zdarzeń.
func eventList(v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		out := make([]string, 0, len(t))
		for e := range t {
			out = append(out, e)
		}
		slices.Sort(out)
		return out, nil
	default:
		return stringList(v)
	}
}

// permissions — klucz `permissions`: brak (nil), skrót albo mapa zakresów.
func permissions(v any) (*Permissions, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return &Permissions{All: t}, nil
	case map[string]any:
		p := &Permissions{Scopes: map[string]string{}}
		for scope, level := range t {
			s, ok := level.(string)
			if !ok {
				return nil, fmt.Errorf("%s: %v nie jest poziomem uprawnień", scope, level)
			}
			p.Scopes[scope] = s
		}
		return p, nil
	default:
		return nil, fmt.Errorf("%v nie jest skrótem ani mapą uprawnień", v)
	}
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

// pullRequestEvents — zdarzenia, w których workflow biegnie na kodzie z PR-a.
var pullRequestEvents = []string{"pull_request", "pull_request_target"}

// Wzorce koniunkcji warunku, które rozstrzygają osiągalność z pull_request.
var (
	eventIs       = regexp.MustCompile(`^github\.event_name\s*==\s*'([^']+)'$`)
	eventIsNotPR  = regexp.MustCompile(`^github\.event_name\s*!=\s*'pull_request'$`)
	refIsTag      = regexp.MustCompile(`^github\.ref_type\s*==\s*'tag'$`)
	needSucceeded = regexp.MustCompile(`^needs\.([A-Za-z0-9_-]+)\.result\s*==\s*'success'$`)
)

// conjuncts dzieli warunek na człony koniunkcji najwyższego poziomu (poza
// nawiasami i napisami), bez `${{ }}` i zewnętrznych nawiasów.
func conjuncts(condition string) []string {
	c := strings.TrimSpace(condition)
	if inner, ok := strings.CutPrefix(c, "${{"); ok {
		c = strings.TrimSpace(strings.TrimSuffix(inner, "}}"))
	}
	var out []string
	depth, quoted, start := 0, false, 0
	for i := 0; i < len(c); i++ {
		switch {
		case c[i] == '\'':
			quoted = !quoted
		case quoted:
		case c[i] == '(':
			depth++
		case c[i] == ')':
			depth--
		case depth == 0 && strings.HasPrefix(c[i:], "&&"):
			out = append(out, strings.TrimSpace(c[start:i]))
			start = i + 2
			i++
		}
	}
	return append(out, strings.TrimSpace(c[start:]))
}

// reachableOnPR mówi, czy zadanie może ruszyć na zdarzeniu pull_request.
// Ocena jest zachowawcza: człon warunku, którego nie rozpoznaje, uznaje za
// spełniony. Zadanie bez funkcji statusu potrzebuje sukcesu wszystkich
// przodków, a jawne `needs.<zadanie>.result == 'success'` — osiągalności
// tego zadania.
func (w Workflow) reachableOnPR(name string, seen map[string]bool) bool {
	if seen[name] {
		return true
	}
	seen[name] = true
	job, ok := w.Jobs[name]
	if !ok {
		return true
	}
	if !HasStatusFunction(job.If) {
		for _, parent := range job.Needs {
			if !w.reachableOnPR(parent, seen) {
				return false
			}
		}
	}
	if job.If == "" {
		return true
	}
	for _, term := range conjuncts(job.If) {
		if m := eventIs.FindStringSubmatch(term); m != nil && !slices.Contains(pullRequestEvents, m[1]) {
			return false
		}
		if eventIsNotPR.MatchString(term) || refIsTag.MatchString(term) {
			return false
		}
		if m := needSucceeded.FindStringSubmatch(term); m != nil && !w.reachableOnPR(m[1], seen) {
			return false
		}
	}
	return true
}

// WriteOnPullRequest zwraca naruszenia reguły „prawo zapisu tylko poza PR-ami”
// (decyzja 49): zadanie, które może ruszyć na pull_request, nie dostaje
// tokenu z prawem zapisu — ani z własnych uprawnień, ani z workflowu.
// Kod z PR-a biegnie w zadaniu (program z drzewa PR-a, build obrazu
// z podbitymi zależnościami), a prawa zapisu do pakietów połączonych
// z repozytorium dają drogę do charta, który Argo wdraża na staging.
func WriteOnPullRequest(w Workflow) []string {
	triggered := false
	for _, e := range pullRequestEvents {
		triggered = triggered || slices.Contains(w.Events, e)
	}
	if !triggered {
		return nil
	}
	names := make([]string, 0, len(w.Jobs))
	for n := range w.Jobs {
		names = append(names, n)
	}
	slices.Sort(names)
	var found []string
	for _, name := range names {
		job := w.Jobs[name]
		perms := job.Permissions
		if perms == nil {
			perms = w.Permissions
		}
		writes := perms.Writes()
		if len(writes) == 0 || !w.reachableOnPR(name, map[string]bool{}) {
			continue
		}
		found = append(found, fmt.Sprintf(
			"%s: zadanie %s może ruszyć na pull_request, a ma prawo zapisu (%s) — kod z PR-a biegnie w zadaniu z takim tokenem; przenieś zapis do zadania, którego warunek wyklucza pull_request (decyzja 49)",
			w.File, name, strings.Join(writes, ", ")))
	}
	return found
}
