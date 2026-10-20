package workflow

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Step — krok zadania albo akcji złożonej: nazwa, akcja (`uses`, pusta dla
// `run`), warunek i wejścia jako napisy.
type Step struct {
	Name string
	Uses string
	If   string
	With map[string]string
}

type rawStep struct {
	Name string         `json:"name"`
	Uses string         `json:"uses"`
	If   any            `json:"if"`
	With map[string]any `json:"with"`
}

func steps(raw []rawStep) []Step {
	out := make([]Step, 0, len(raw))
	for _, r := range raw {
		s := Step{Name: r.Name, Uses: strings.TrimSpace(r.Uses), With: map[string]string{}}
		if r.If != nil {
			s.If = strings.TrimSpace(fmt.Sprint(r.If))
		}
		for k, v := range r.With {
			s.With[k] = strings.TrimSpace(fmt.Sprint(v))
		}
		out = append(out, s)
	}
	return out
}

// LoadActions czyta kroki akcji złożonych z ActionsDir, pod kluczem, którym
// workflow je wywołuje (`./.github/actions/<nazwa>`).
func LoadActions(root string) (map[string][]Step, error) {
	dirs, err := ActionDirs(root)
	if err != nil {
		return nil, err
	}
	actions := map[string][]Step{}
	for _, dir := range dirs {
		var raw []byte
		for _, name := range []string{"action.yml", "action.yaml"} {
			if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), name)); err == nil {
				raw = b
				break
			}
		}
		var a struct {
			Runs struct {
				Steps []rawStep `json:"steps"`
			} `json:"runs"`
		}
		if err := yaml.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
		actions["./"+dir] = steps(a.Runs.Steps)
	}
	return actions, nil
}

// builtinCache — akcje setup-* z wbudowaną pamięcią podręczną: wejście,
// które ją włącza, i wartość, która wyłącza. Pusta wartość wyłączająca —
// pamięć działa tylko z niepustym wejściem.
var builtinCache = map[string]struct{ input, off string }{
	"actions/setup-go":     {"cache", "false"},
	"astral-sh/setup-uv":   {"enable-cache", "false"},
	"actions/setup-node":   {"package-manager-cache", "false"},
	"actions/setup-python": {"cache", ""},
	"actions/setup-java":   {"cache", ""},
}

// CacheRules — pamięć podręczna CI (decyzja 70, §6.8 kontraktu):
//   - `actions/cache` odtwarza i zapisuje w jednym kroku — zakaz; osobno
//     `restore` i `save`;
//   - setup-* z włączoną wbudowaną pamięcią — zakaz;
//   - `actions/cache/restore` tylko w zadaniu bez prawa zapisu i bez
//     `environment`: zatruty wpis to wykonanie kodu w każdym zadaniu, które
//     go odtworzy, a tu z tokenem do rejestru, atestacji albo issues;
//   - `actions/cache/save` tylko w zadaniu, które nie rusza na pull_request;
//   - klucz z `hashFiles(…)` — inaczej wpis nie unieważnia się przy zmianie
//     zależności.
//
// zizmor (1.30.1, 1.31.1) nie zgłasza `actions/cache/restore`, nawet
// w zadaniu publikującym, a actionlint 1.7.12 nie zna `cache-mode` — stąd
// reguła tutaj. Kroki akcji złożonych liczą się w zadaniu, które je wywołuje.
func CacheRules(w Workflow, actions map[string][]Step) []string {
	var found []string
	for _, name := range slices.Sorted(maps.Keys(w.Jobs)) {
		job := w.Jobs[name]
		perms := job.Permissions
		if perms == nil {
			perms = w.Permissions
		}
		writes := perms.Writes()
		for _, s := range expand(job.Steps, actions) {
			where := fmt.Sprintf("%s: zadanie %s, krok %q", w.File, name, s.label())
			action, _, _ := strings.Cut(s.Uses, "@")
			switch {
			case action == "actions/cache":
				found = append(found, where+": actions/cache odtwarza i zapisuje w jednym kroku — osobno actions/cache/restore w czytelniku i actions/cache/save w zadaniu poza PR-ami (decyzja 70)")
			case action == "actions/cache/restore":
				if len(writes) > 0 || job.Environment {
					what := strings.Join(writes, ", ")
					if job.Environment {
						what = strings.TrimPrefix(what+", environment", ", ")
					}
					found = append(found, fmt.Sprintf("%s: odtwarza pamięć podręczną w zadaniu z prawem zapisu (%s) — zatruty wpis wykonałby się z tym tokenem; buduj tu od zera (decyzja 70)", where, what))
				}
				found = append(found, keyRule(where, s)...)
			case action == "actions/cache/save":
				if w.reachableOnPR(name, map[string]bool{}) && triggeredByPR(w) {
					found = append(found, where+": zapisuje pamięć podręczną w zadaniu, które może ruszyć na pull_request — pisze tylko zadanie, którego warunek wyklucza pull_request (decyzja 70)")
				}
				found = append(found, keyRule(where, s)...)
			default:
				if c, ok := builtinCache[action]; ok {
					v, set := s.With[c.input]
					enabled := (c.off == "" && v != "") || (c.off != "" && (!set || v != c.off))
					if enabled {
						found = append(found, fmt.Sprintf("%s: %s z wbudowaną pamięcią podręczną — ustaw %s: %q (decyzja 70)", where, action, c.input, c.off))
					}
				}
			}
		}
	}
	return found
}

func keyRule(where string, s Step) []string {
	if !strings.Contains(s.With["key"], "hashFiles(") {
		return []string{where + ": klucz pamięci podręcznej bez hashFiles(…) — wpis nie unieważni się przy zmianie zależności (decyzja 70)"}
	}
	return nil
}

func triggeredByPR(w Workflow) bool {
	return slices.ContainsFunc(pullRequestEvents, func(e string) bool { return slices.Contains(w.Events, e) })
}

// expand rozwija wywołania akcji złożonych w ich kroki.
func expand(list []Step, actions map[string][]Step) []Step {
	var out []Step
	for _, s := range list {
		if inner, ok := actions[s.Uses]; ok {
			for _, i := range inner {
				i.Name = s.Uses + " → " + i.label()
				out = append(out, i)
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

func (s Step) label() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Uses
}
