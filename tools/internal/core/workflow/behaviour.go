package workflow

import (
	"fmt"
	"path"
	"reflect"
	"strings"

	"sigs.k8s.io/yaml"
)

// ActionsDir — katalog akcji złożonych względem korzenia repozytorium.
const ActionsDir = ".github/actions"

// IsDefinition mówi, czy plik to definicja workflowu albo akcji złożonej —
// plik, którego zmianę ocenia SameBehaviour, a nie porównanie bajtów.
func IsDefinition(file string) bool {
	switch ext := path.Ext(file); {
	case path.Dir(file) == Dir:
		return ext == ".yml" || ext == ".yaml"
	case strings.HasPrefix(file, ActionsDir+"/"):
		base := path.Base(file)
		return base == "action.yml" || base == "action.yaml"
	}
	return false
}

// SameBehaviour mówi, czy dwie wersje workflowu albo akcji złożonej działają
// tak samo: równe drzewa YAML po pominięciu komentarzy (parser ich nie
// zwraca), nazw kroków i wersji akcji w `uses` (podbicia Dependabota,
// decyzja 22). Każda inna różnica — warunek, polecenie, uprawnienie,
// zadanie, `needs`, limit czasu, `continue-on-error`, nazwa zadania (nazwa
// statusu w regule) — to zmiana działania. nil — pliku nie ma.
func SameBehaviour(before, after []byte) (bool, error) {
	if before == nil || after == nil {
		return before == nil && after == nil, nil
	}
	var b, a any
	if err := yaml.Unmarshal(before, &b); err != nil {
		return false, fmt.Errorf("wersja przed zmianą: %w", err)
	}
	if err := yaml.Unmarshal(after, &a); err != nil {
		return false, fmt.Errorf("wersja po zmianie: %w", err)
	}
	return reflect.DeepEqual(normalize(b), normalize(a)), nil
}

// normalize — drzewo YAML bez tego, co nie zmienia działania.
func normalize(node any) any {
	switch n := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, v := range n {
			switch k {
			case "steps":
				out[k] = normalizeSteps(v)
			case "uses":
				out[k] = withoutVersion(v)
			default:
				out[k] = normalize(v)
			}
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			out[i] = normalize(v)
		}
		return out
	default:
		return n
	}
}

// normalizeSteps — kroki bez nazw: nazwa kroku to etykieta w logu, nie
// działanie (nazwa zadania już tak — to nazwa statusu).
func normalizeSteps(steps any) any {
	list, ok := steps.([]any)
	if !ok {
		return normalize(steps)
	}
	out := make([]any, len(list))
	for i, s := range list {
		if step, ok := s.(map[string]any); ok {
			cp := make(map[string]any, len(step))
			for k, v := range step {
				if k != "name" {
					cp[k] = v
				}
			}
			s = cp
		}
		out[i] = normalize(s)
	}
	return out
}

// withoutVersion — akcja bez wersji: `actions/checkout@<sha>` →
// `actions/checkout`. Zmiana akcji na inną zostaje różnicą.
func withoutVersion(uses any) any {
	s, ok := uses.(string)
	if !ok || strings.HasPrefix(s, "./") {
		return uses
	}
	if i := strings.LastIndex(s, "@"); i > 0 {
		return s[:i]
	}
	return s
}
