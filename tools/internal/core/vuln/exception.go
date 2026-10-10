// Package vuln to polityka podatności z decyzji 37, wspólna dla skanu
// obrazu (OSV-Scanner) i kodu narzędzi (govulncheck): wyjątki z datą
// wygaśnięcia, waga z oceny CVSS i ocena, co blokuje.
//
// Baza podatności zmienia się codziennie, więc ten sam commit może jutro
// być czerwony. Dlatego:
//   - blokuje tylko to, co da się naprawić (jest wersja z poprawką),
//     o wadze HIGH albo CRITICAL — albo nieznanej, bo nieznana to nie
//     bezpieczna;
//   - na PR-ze blokuje tylko to, czego nie ma w obrazie bazowym (main):
//     podatność opublikowana wczoraj w pakiecie, którego PR nie dotyka, nie
//     jest winą PR-a, a jej naprawa to osobna zmiana;
//   - świadomie przyjęte podatności leżą w ci/vuln-exceptions.yaml
//     z uzasadnieniem i datą — wyjątek po dacie znów blokuje;
//   - raport zapisuje, z jakiej bazy powstał: suma i najnowszy wpis każdej
//     bazy ekosystemu, pobranej raz na bieg i użytej do obu skanów.
//
// Wyniki skanerów czytają adaptery (adapters/osv, adapters/gotool).
package vuln

import (
	"fmt"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// ExceptionsFile — wyjątki względem korzenia repozytorium.
const ExceptionsFile = "ci/vuln-exceptions.yaml"

// MaxExceptionDays — najdalsza data wygaśnięcia wyjątku od dziś. Wyjątek
// „na zawsze" to podatność, o której przestaje się pamiętać.
const MaxExceptionDays = 90

// Exception — świadomie przyjęta podatność w jednym pakiecie.
type Exception struct {
	// ID — CVE, GHSA, GO-… albo identyfikator OSV (np. ALPINE-CVE-…);
	// pasuje do identyfikatora i aliasów podatności.
	ID      string `json:"id"`
	Package string `json:"package"`
	// Expires — RRRR-MM-DD; od tego dnia wyjątek nie działa.
	Expires string `json:"expires"`
	Reason  string `json:"reason"`
	// Scope — image (pakiety obrazu, domyślnie) albo go (moduły narzędzi
	// z tools/, govulncheck).
	Scope string `json:"scope,omitempty"`
}

// Zakresy wyjątków.
const (
	ScopeImage = "image"
	ScopeGo    = "go"
)

// InScope zwraca wyjątki jednego zakresu.
func InScope(exceptions []Exception, scope string) []Exception {
	var out []Exception
	for _, e := range exceptions {
		s := e.Scope
		if s == "" {
			s = ScopeImage
		}
		if s == scope {
			out = append(out, e)
		}
	}
	return out
}

func (e Exception) expiry() (time.Time, error) {
	return time.Parse("2006-01-02", e.Expires)
}

// LoadExceptions czyta wyjątki ściśle (nieznane pole to błąd) i sprawdza
// każdy: identyfikator, pakiet, uzasadnienie i datę nie dalszą niż
// MaxExceptionDays od dziś. Wyjątki po dacie nie są błędem pliku —
// odrzuca je ocena, gdy pasują do znalezionej podatności.
func LoadExceptions(path string, today time.Time) ([]Exception, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Exceptions []Exception `json:"exceptions"`
	}
	if err := yaml.UnmarshalStrict(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var problems []string
	for i, e := range file.Exceptions {
		where := fmt.Sprintf("wyjątek %d (%s)", i+1, e.ID)
		if e.ID == "" || e.Package == "" {
			problems = append(problems, where+": id i package wymagane")
		}
		if len(strings.TrimSpace(e.Reason)) < 20 {
			problems = append(problems, where+": reason — uzasadnienie, nie etykieta")
		}
		if e.Scope != "" && e.Scope != ScopeImage && e.Scope != ScopeGo {
			problems = append(problems, fmt.Sprintf("%s: scope=%q — image albo go", where, e.Scope))
		}
		exp, err := e.expiry()
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: expires=%q — RRRR-MM-DD", where, e.Expires))
			continue
		}
		if exp.After(today.AddDate(0, 0, MaxExceptionDays)) {
			problems = append(problems, fmt.Sprintf("%s: expires %s — dalej niż %d dni od dziś", where, e.Expires, MaxExceptionDays))
		}
	}
	if len(problems) > 0 {
		return nil, cli.Unmet("%s: %s", path, strings.Join(problems, "; "))
	}
	return file.Exceptions, nil
}
