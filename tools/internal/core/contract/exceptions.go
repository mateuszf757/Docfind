package contract

import (
	"fmt"
	"regexp"
	"slices"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
)

// ExceptionsFile — rejestr wyjątków od klauzul kontraktu (§0.4, decyzja 64).
const ExceptionsFile = "ci/contract-exceptions.yaml"

// Exception — wyjątek od klauzuli: czego dotyczy, dlaczego, do kiedy i na
// podstawie której decyzji albo PR-a.
type Exception struct {
	Clause  string `json:"clause"`
	What    string `json:"what"`
	Reason  string `json:"reason"`
	Expires string `json:"expires"`
	Ref     string `json:"ref"`
}

// ParseExceptions czyta rejestr wyjątków ściśle: nieznane pole to literówka.
func ParseExceptions(raw []byte) ([]Exception, error) {
	var file struct {
		Exceptions []Exception `json:"exceptions"`
	}
	if err := yaml.UnmarshalStrict(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", ExceptionsFile, err)
	}
	return file.Exceptions, nil
}

// clauseID — numer klauzuli w nagłówku kontraktu.
var clauseID = regexp.MustCompile(`(?m)^\*\*(\d+\.\d+)\b`)

// Clauses — numery klauzul kontraktu.
func Clauses(doc string) []string {
	var out []string
	for _, m := range clauseID.FindAllStringSubmatch(doc, -1) {
		out = append(out, m[1])
	}
	return out
}

// CheckExceptions — naruszenia rejestru (wyjątek bez pól, nieznana
// klauzula, data nieczytelna, po terminie albo dalej niż 90 dni)
// i ostrzeżenia (wygasa w ciągu ExceptionWarning).
func CheckExceptions(list []Exception, clauses []string, today time.Time) (problems, warnings []string) {
	limit := today.AddDate(0, 0, vuln.MaxExceptionDays)
	for _, e := range list {
		name := fmt.Sprintf("%s: wyjątek od §%s (%s)", ExceptionsFile, e.Clause, e.What)
		expires, err := time.Parse(time.DateOnly, e.Expires)
		switch {
		case e.Clause == "" || e.What == "" || e.Reason == "" || e.Ref == "":
			problems = append(problems, name+" bez klauzuli, opisu, uzasadnienia albo odwołania")
		case !slices.Contains(clauses, e.Clause):
			problems = append(problems, name+": takiej klauzuli nie ma w kontrakcie")
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: data %q spoza RRRR-MM-DD", name, e.Expires))
		case !today.Before(expires):
			problems = append(problems, fmt.Sprintf("%s wygasł %s — obróć/napraw albo przedłuż z uzasadnieniem i wpisem w DECYZJE", name, e.Expires))
		case expires.After(limit):
			problems = append(problems, fmt.Sprintf("%s: data %s dalej niż %d dni", name, e.Expires, vuln.MaxExceptionDays))
		case !expires.After(today.Add(ExceptionWarning)):
			warnings = append(warnings, fmt.Sprintf("%s wygasa %s", name, e.Expires))
		}
	}
	return problems, warnings
}
