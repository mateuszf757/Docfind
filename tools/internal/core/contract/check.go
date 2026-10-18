package contract

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/workflow"
)

// Doc — kontrakt względem korzenia repozytorium.
const Doc = "docs/engineering-contracts.md"

// Decisions — rejestr decyzji; zmiana pola chronionego idzie razem z nim.
const Decisions = "docs/DECYZJE.md"

// Znaczniki tabeli progów w kontrakcie: między nimi leży dokładnie wynik
// `dft contract thresholds`.
const (
	TableStart = "<!-- progi: początek — generuje `dft contract thresholds --write`, nie edytuj ręcznie -->"
	TableEnd   = "<!-- progi: koniec -->"
)

// Fields — pola każdej klauzuli poza §0 (§0.1 kontraktu).
var Fields = []string{"Egzekwuje", "Dowód", "Warstwa", "Wariant negatywny", "Decyzja"}

// Protected — pola chronione kontraktu (§8.3): progi, sam kontrakt,
// konfiguracja skanera sekretów i jego wyjątki, reguły repozytorium,
// środowiska GitHuba, konfiguracja Dependabota, workflowy i akcje złożone.
// Ścieżka zakończona „/” obejmuje katalog. Zmiana któregokolwiek wymaga
// w tym samym diffie zmiany docs/DECYZJE.md — inaczej najprostszą
// „naprawą” czerwonej bramki jest poluzowanie progu albo warunku w tym
// samym PR-ze, który ta bramka ocenia. Workflowy i akcje ocenia się po
// działaniu (workflow.SameBehaviour), resztę po bajtach.
//
// Wyjątki podatności (ci/vuln-exceptions.yaml) nie są polem chronionym:
// każdy ma uzasadnienie i datę wygaśnięcia ≤ 90 dni, sprawdzane przez
// core/vuln — sam wygasa. Wyjątek gitleaks nie wygasa.
var Protected = []string{
	File, Doc, "ci/gitleaks.toml", "ci/gitleaks-ignore",
	".github/rulesets/", ".github/environments/", ".github/dependabot.yml",
	workflow.Dir + "/", workflow.ActionsDir + "/",
}

// Known — to, na co kontrakt może się powołać poza znacznikiem [ŻYCZENIE]:
// zadania mise, polecenia i bramki dft, pakiety narzędzi, decyzje.
type Known struct {
	Tasks    []string
	Commands []string
	Checks   []string
	// Packages — względem tools/internal albo tools: stages/release, cmd/dft.
	Packages  []string
	Decisions []int
}

var (
	// clauseHeading — nagłówek klauzuli: **N.M Nazwa.**
	clauseHeading = regexp.MustCompile(`^\*\*(\d+)\.(\d+)\b`)
	// wish — znacznik [ŻYCZENIE] albo [ŻYCZENIE: czego brakuje]. Strażnik
	// i decyzja w środku to plan, a nie odwołanie: nie muszą istnieć.
	wish     = regexp.MustCompile(`\[ŻYCZENIE[^\]]*\]`)
	codeSpan = regexp.MustCompile("`([^`]+)`")
	dftCall  = regexp.MustCompile(`^(?:ci/)?dft ([a-z][a-z-]*)(?: ([a-z][a-z-]*))?`)
	miseRun  = regexp.MustCompile(`^\./bin/mise run ([a-z][a-z:-]*)`)
	miseTask = regexp.MustCompile(`^[a-z]+:[a-z][a-z-]*$`)
	pkgPath  = regexp.MustCompile(`^(?:stages|core|adapters|kit|cmd)/[a-z]+$`)
	// decisionField — lista numerów na początku pola „Decyzja:”; dalej
	// wolny tekst („nowa przed Etapem 6” to nie decyzja 6).
	decisionField = regexp.MustCompile(`Decyzja:\s*(\d+(?:\s*,\s*\d+)*)`)
	decisionProse = regexp.MustCompile(`\b[Dd]ecyzj[aęiy] (\d+)\b`)
	miseHeader    = regexp.MustCompile(`(?m)^\[tasks\.(?:"([^"]+)"|([A-Za-z0-9_-]+))\]`)
	decisionHead  = regexp.MustCompile(`(?m)^## (\d+)\. `)
)

// CheckDocument sprawdza kontrakt:
//   - tabela progów między znacznikami jest równa tabeli z kodu;
//   - każda klauzula poza §0 ma pola Fields;
//   - zadanie mise, polecenie albo bramka dft i pakiet narzędzi, na które
//     kontrakt się powołuje poza znacznikiem [ŻYCZENIE], istnieją;
//   - decyzje, na które się powołuje, są w docs/DECYZJE.md.
func CheckDocument(text string, known Known) []string {
	var found []string
	fail := func(format string, args ...any) {
		found = append(found, fmt.Sprintf("%s: ", Doc)+fmt.Sprintf(format, args...))
	}
	start := strings.Index(text, TableStart)
	end := strings.Index(text, TableEnd)
	switch {
	case start < 0 || end < 0 || end < start:
		fail("brak tabeli progów między znacznikami %q i %q", TableStart, TableEnd)
	case strings.TrimSpace(text[start+len(TableStart):end]) != strings.TrimSpace(Markdown()):
		fail("tabela progów różni się od kodu (%s) — `ci/dft contract thresholds --write`", File)
	}

	// Wiersze bez znaczników [ŻYCZENIE] — znacznik przez kilka wierszy
	// zostawia puste wiersze, więc numeracja się zgadza.
	plain := strings.Split(wish.ReplaceAllStringFunc(text, func(m string) string {
		return strings.Repeat("\n", strings.Count(m, "\n"))
	}), "\n")
	var where string
	var clause strings.Builder
	flush := func() {
		if strings.HasPrefix(where, "klauzula ") && !strings.HasPrefix(where, "klauzula 0.") {
			joined := strings.Join(strings.Fields(clause.String()), " ")
			for _, f := range Fields {
				if !strings.Contains(joined, f+":") {
					fail("%s bez pola „%s:”", where, f)
				}
			}
		}
		clause.Reset()
	}
	for i, line := range strings.Split(text, "\n") {
		if m := clauseHeading.FindStringSubmatch(line); m != nil {
			flush()
			where = "klauzula " + m[1] + "." + m[2]
		} else if strings.HasPrefix(line, "## ") {
			flush()
			where = strings.TrimSpace(strings.TrimPrefix(line, "## "))
		}
		clause.WriteString(line)
		clause.WriteByte('\n')
		for _, problem := range references(plain[i], known) {
			fail("%s — %s", where, problem)
		}
	}
	flush()
	return found
}

// references — odwołania w wierszu, których nie ma w repozytorium.
func references(line string, known Known) []string {
	var problems []string
	for _, m := range codeSpan.FindAllStringSubmatch(line, -1) {
		span := m[1]
		switch {
		case dftCall.MatchString(span):
			c := dftCall.FindStringSubmatch(span)
			switch {
			case !slices.Contains(known.Commands, c[1]):
				problems = append(problems, fmt.Sprintf("`%s`: dft nie ma polecenia %q", span, c[1]))
			case c[1] == "check" && c[2] != "" && !slices.Contains(known.Checks, c[2]):
				problems = append(problems, fmt.Sprintf("`%s`: dft nie ma bramki %q", span, c[2]))
			}
		case miseRun.MatchString(span):
			if task := miseRun.FindStringSubmatch(span)[1]; !slices.Contains(known.Tasks, task) {
				problems = append(problems, fmt.Sprintf("`%s`: brak zadania mise %q", span, task))
			}
		case taskLike(span, known.Tasks):
			if !slices.Contains(known.Tasks, span) {
				problems = append(problems, fmt.Sprintf("`%s`: brak zadania mise", span))
			}
		case pkgPath.MatchString(span):
			if !slices.Contains(known.Packages, span) {
				problems = append(problems, fmt.Sprintf("`%s`: brak pakietu w tools/", span))
			}
		}
	}
	var numbers []string
	if m := decisionField.FindStringSubmatch(line); m != nil {
		numbers = append(numbers, strings.Split(m[1], ",")...)
	}
	for _, m := range decisionProse.FindAllStringSubmatch(line, -1) {
		numbers = append(numbers, m[1])
	}
	for _, s := range numbers {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || !slices.Contains(known.Decisions, n) {
			problems = append(problems, fmt.Sprintf("decyzji %s nie ma w %s", strings.TrimSpace(s), Decisions))
		}
	}
	return problems
}

// taskLike — `x:y`, gdzie x: to przedrostek któregoś zadania mise (check:,
// cluster:, test:…). `gitleaks:allow` to nie odwołanie do zadania.
func taskLike(span string, tasks []string) bool {
	if !miseTask.MatchString(span) {
		return false
	}
	prefix := span[:strings.Index(span, ":")+1]
	return slices.ContainsFunc(tasks, func(t string) bool { return strings.HasPrefix(t, prefix) })
}

// MiseTasks — nazwy zadań z mise.toml.
func MiseTasks(toml string) []string {
	var tasks []string
	for _, m := range miseHeader.FindAllStringSubmatch(toml, -1) {
		tasks = append(tasks, m[1]+m[2])
	}
	return tasks
}

// DecisionNumbers — numery decyzji z nagłówków docs/DECYZJE.md.
func DecisionNumbers(text string) []int {
	var numbers []int
	for _, m := range decisionHead.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			numbers = append(numbers, n)
		}
	}
	return numbers
}

// ReplaceTable wstawia między znaczniki kontraktu tabelę progów z kodu.
func ReplaceTable(text string) (string, error) {
	start := strings.Index(text, TableStart)
	end := strings.Index(text, TableEnd)
	if start < 0 || end < 0 || end < start {
		return "", fmt.Errorf("%s: brak znaczników tabeli progów %q i %q", Doc, TableStart, TableEnd)
	}
	return text[:start+len(TableStart)] + "\n" + Markdown() + text[end:], nil
}

// Contents — treść pliku w commicie bazowym i w sprawdzanym; nil — pliku
// tam nie ma.
type Contents func(path string) (before, after []byte, err error)

// CheckChange sprawdza zmianę względem commita bazowego: pole chronione
// zmienione bez zmiany docs/DECYZJE.md to naruszenie. Workflow albo akcja,
// które działają tak samo (podbicie wersji akcji, komentarz, nazwa kroku),
// nie są zmianą.
func CheckChange(changed []string, contents Contents) ([]string, error) {
	if slices.Contains(changed, Decisions) {
		return nil, nil
	}
	var found []string
	for _, path := range changed {
		if !isProtected(path) {
			continue
		}
		if workflow.IsDefinition(path) {
			before, after, err := contents(path)
			if err != nil {
				return nil, err
			}
			same, err := workflow.SameBehaviour(before, after)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			if same {
				continue
			}
		}
		found = append(found, fmt.Sprintf("%s: pole chronione kontraktu zmienione bez wpisu w %s — zmiana progu, kontraktu, skanera sekretów, reguł, środowisk albo działania workflowu to decyzja z kosztem i warunkiem zmiany zdania (§8.3, decyzja 50)", path, Decisions))
	}
	return found, nil
}

func isProtected(path string) bool {
	for _, p := range Protected {
		if path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(path, p)) {
			return true
		}
	}
	return false
}
