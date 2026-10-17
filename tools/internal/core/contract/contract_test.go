package contract

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const repoRoot = "../../../.."

// known — repozytorium atrapy: tyle strażników i decyzji, ile używa document.
var known = Known{
	Tasks:     []string{"test", "check:runtime", "cluster:drain"},
	Commands:  []string{"check", "contract"},
	Checks:    []string{"contract", "workflows"},
	Packages:  []string{"stages/lifecycle", "cmd/dft"},
	Decisions: []int{14, 32, 50},
}

// document — najmniejszy kontrakt, który przechodzi: tabela z kodu między
// znacznikami, klauzula §0 bez pól, dwie klauzule ze wszystkimi polami.
func document(table string) string {
	return "# Kontrakt\n\n## 0. Zasady\n\n**0.1 Zasada.** Bez pól, `dft check contract` (decyzja 50).\n\n" +
		"## 4. Obraz\n\n**4.1 Obraz.** Treść.\nEgzekwuje: `check:runtime` w `stages/lifecycle`. Dowód: raport.\n" +
		"Warstwa: W1. Wariant negatywny: za duży obraz, `gitleaks:allow` w kodzie. Decyzja: 14, 32.\n\n" +
		"**4.2 Rozmiar.** Treść.\nEgzekwuje: [ŻYCZENIE: `dft ci budget`, `ci:require`,\n`stages/upgrade` i decyzja 77]. Dowód: brak.\n" +
		"Warstwa: W4. Wariant\nnegatywny: brak. Decyzja: nowa przed Etapem 6.\n\n" +
		"## 10. Progi\n\n" + TableStart + "\n" + table + TableEnd + "\n"
}

func TestCheckDocumentAcceptsGeneratedTable(t *testing.T) {
	if found := CheckDocument(document(Markdown()), known); len(found) > 0 {
		t.Fatalf("poprawny kontrakt odrzucony: %v", found)
	}
}

func TestCheckDocumentRejects(t *testing.T) {
	for _, tt := range []struct {
		name, old, new, want string
	}{
		{"ręcznie zmieniony próg", "≥ 100", "≥ 10", "tabela progów różni się od kodu"},
		{"brak znacznika końca tabeli", TableEnd, "", "brak tabeli progów"},
		{"klauzula bez strażnika", "Egzekwuje: `check:runtime` w `stages/lifecycle`.", "", "klauzula 4.1 bez pola „Egzekwuje:”"},
		{"klauzula bez wariantu negatywnego", "Wariant negatywny: za duży obraz, `gitleaks:allow` w kodzie.", "", "klauzula 4.1 bez pola „Wariant negatywny:”"},
		{"klauzula bez decyzji", "Decyzja: 14, 32.", "", "klauzula 4.1 bez pola „Decyzja:”"},
		{"nieznane zadanie mise", "`check:runtime`", "`check:runtimes`", "`check:runtimes`: brak zadania mise"},
		{"nieznana bramka dft", "`dft check contract`", "`dft check contracts`", "dft nie ma bramki \"contracts\""},
		{"nieznane polecenie dft", "`dft check contract`", "`dft verify contract`", "dft nie ma polecenia \"verify\""},
		{"nieznany pakiet", "`stages/lifecycle`", "`stages/lifecycles`", "`stages/lifecycles`: brak pakietu"},
		{"nieznana decyzja w polu", "Decyzja: 14, 32.", "Decyzja: 14, 33.", "decyzji 33 nie ma"},
		{"nieznana decyzja w tekście", "(decyzja 50)", "(decyzja 51)", "decyzji 51 nie ma"},
		{"strażnik poza znacznikiem życzenia", "[ŻYCZENIE: `dft ci budget`,", "`dft ci budget` [ŻYCZENIE:", "dft nie ma polecenia \"ci\""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			text := strings.Replace(document(Markdown()), tt.old, tt.new, 1)
			if text == document(Markdown()) {
				t.Fatalf("zamiana %q nie zmieniła kontraktu — popraw test", tt.old)
			}
			found := CheckDocument(text, known)
			if len(found) != 1 || !strings.Contains(found[0], tt.want) {
				t.Fatalf("oczekiwano jednego naruszenia z %q, jest: %q", tt.want, found)
			}
		})
	}
}

// TestCheckDocumentFieldsStayInSection — pola za nagłówkiem kolejnej
// sekcji nie należą do klauzuli przed nim.
func TestCheckDocumentFieldsStayInSection(t *testing.T) {
	text := strings.Replace(document(Markdown()), "Treść.\nEgzekwuje: [ŻYCZENIE", "Treść.\n\n## 5. Działanie\n\nEgzekwuje: [ŻYCZENIE", 1)
	found := CheckDocument(text, known)
	if len(found) != len(Fields) {
		t.Fatalf("oczekiwano %d brakujących pól klauzuli 4.2, jest: %q", len(Fields), found)
	}
	for i, f := range Fields {
		if !strings.Contains(found[i], "klauzula 4.2 bez pola „"+f+":”") {
			t.Errorf("naruszenie %d: %q", i, found[i])
		}
	}
}

func TestReplaceTable(t *testing.T) {
	edited := document(strings.Replace(Markdown(), "≥ 100", "≥ 10", 1))
	fixed, err := ReplaceTable(edited)
	if err != nil {
		t.Fatal(err)
	}
	if fixed != document(Markdown()) {
		t.Errorf("tabela po wstawieniu:\n%s", fixed)
	}
	if again, _ := ReplaceTable(fixed); again != fixed {
		t.Error("drugie wstawienie zmieniło kontrakt")
	}
	if _, err := ReplaceTable("# Kontrakt bez znaczników\n"); err == nil {
		t.Error("kontrakt bez znaczników bez błędu")
	}
}

func TestMiseTasksAndDecisions(t *testing.T) {
	toml := "[env]\nX = 1\n\n[tasks.test]\nrun = \"ci/dft test\"\n\n[tasks.\"check:runtime\"]\nrun = \"x\"\n# [tasks.zakomentowane]\n"
	if got := MiseTasks(toml); !slices.Equal(got, []string{"test", "check:runtime"}) {
		t.Errorf("MiseTasks = %q", got)
	}
	decisions := "# Decyzje\n\n## Premisa\n\n## 1. Kubernetes\n\n### 1.1 Szczegół\n\n## 50. Kontrakt\n\nTekst ## 51. w wierszu.\n"
	if got := DecisionNumbers(decisions); !slices.Equal(got, []int{1, 50}) {
		t.Errorf("DecisionNumbers = %v", got)
	}
}

// contents — atrapa treści w commicie bazowym i sprawdzanym.
func contents(files map[string][2]string) Contents {
	return func(path string) ([]byte, []byte, error) {
		f, ok := files[path]
		if !ok {
			return nil, nil, errors.New("nieoczekiwany odczyt " + path)
		}
		var before, after []byte
		if f[0] != "" {
			before = []byte(f[0])
		}
		if f[1] != "" {
			after = []byte(f[1])
		}
		return before, after, nil
	}
}

func TestCheckChange(t *testing.T) {
	const ci = ".github/workflows/ci.yml"
	const job = "jobs:\n  test:\n    runs-on: ubuntu-24.04\n    steps:\n      - uses: actions/checkout@aaaa # v1\n      - run: ./bin/mise run test\n"
	bumped := strings.Replace(job, "@aaaa # v1", "@bbbb # v2", 1)
	disabled := strings.Replace(job, "- run: ./bin/mise run test", "- run: ./bin/mise run test\n        if: false", 1)
	for _, tt := range []struct {
		name    string
		changed []string
		files   map[string][2]string
		want    []string
	}{
		{"zwykły kod", []string{"services/api/src/main.py", "tools/cmd/dft/main.go"}, nil, nil},
		{"próg bez decyzji", []string{File, "tools/cmd/dft/cluster.go"}, nil, []string{File}},
		{"próg z decyzją", []string{File, Decisions}, nil, nil},
		{"kontrakt bez decyzji", []string{Doc}, nil, []string{Doc}},
		{"wyjątek gitleaks bez decyzji", []string{"ci/gitleaks-ignore"}, nil, []string{"ci/gitleaks-ignore"}},
		{"ruleset i środowisko bez decyzji", []string{".github/rulesets/main.json", ".github/environments/production.json", "README.md"}, nil, []string{".github/rulesets/main.json", ".github/environments/production.json"}},
		{"wyjątek podatności to nie pole chronione", []string{"ci/vuln-exceptions.yaml"}, nil, nil},
		{"podobna nazwa to nie pole chronione", []string{"ci/gitleaks.toml.bak", ".github/rulesets-old/x.json"}, nil, nil},
		{"podbicie akcji przez Dependabota", []string{ci}, map[string][2]string{ci: {job, bumped}}, nil},
		{"krok bramki wyłączony warunkiem", []string{ci}, map[string][2]string{ci: {job, disabled}}, []string{ci}},
		{"nowy workflow", []string{".github/workflows/nowy.yml"}, map[string][2]string{".github/workflows/nowy.yml": {"", job}}, []string{".github/workflows/nowy.yml"}},
		{"skrypt obok akcji złożonej", []string{".github/actions/build-image/run.sh"}, nil, []string{".github/actions/build-image/run.sh"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			found, err := CheckChange(tt.changed, contents(tt.files))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, f := range found {
				got = append(got, strings.SplitN(f, ":", 2)[0])
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("CheckChange(%v) = %v; oczekiwano naruszeń dla %v", tt.changed, found, tt.want)
			}
		})
	}
	if _, err := CheckChange([]string{ci}, contents(map[string][2]string{ci: {job, "jobs: ["}})); err == nil {
		t.Error("niepoprawny YAML po zmianie bez błędu")
	}
}

// TestProtectedPathsExist — pole chronione, którego nie ma w repozytorium
// (zmieniona nazwa pliku, przeniesiony katalog), przestaje cokolwiek chronić.
func TestProtectedPathsExist(t *testing.T) {
	for _, p := range append([]string{Decisions}, Protected...) {
		if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(p))); err != nil {
			t.Errorf("pole chronione %s: %v", p, err)
		}
	}
}

// registryFields — pola konfiguracji bramek, których wartość pochodzi
// z rejestru progów. Liczba wpisana wprost w polu (`MinRequests: 10`) to
// próg poza tabelą kontraktu i poza polem chronionym.
var registryFields = []string{"MinRequests", "DrainTimeout", "Budget", "MaxImageUnpackedBytes", "MaxOwnShutdown", "RefusalTimeout", "InflightDelay"}

// TestGatesUseRegistry — w kodzie produkcyjnym narzędzi pole progu bramki
// nie dostaje literału ani stałej z pakietu time: bierze ją z rejestru
// (contract.X) albo przepisuje z innej konfiguracji.
func TestGatesUseRegistry(t *testing.T) {
	tools := filepath.Join(repoRoot, "tools")
	fset := token.NewFileSet()
	var checked int
	err := filepath.WalkDir(tools, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || !slices.Contains(registryFields, key.Name) {
				return true
			}
			checked++
			if literal(kv.Value) {
				t.Errorf("%s: %s z literałem — próg bramki bierze się z rejestru %s", fset.Position(kv.Pos()), key.Name, File)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < len(registryFields) {
		t.Errorf("sprawdzonych pól progów: %d — reguła nie widzi kodu bramek", checked)
	}
}

// literal — wyrażenie z literałem albo ze stałą pakietu time.
func literal(e ast.Expr) bool {
	var found bool
	ast.Inspect(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			found = true
		case *ast.SelectorExpr:
			if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "time" {
				found = true
			}
		}
		return !found
	})
	return found
}

func TestLiteral(t *testing.T) {
	for src, want := range map[string]bool{
		"10":                             true,
		"180 * time.Second":              true,
		"time.Minute":                    true,
		"contract.DrainMinRequests":      false,
		"c.Limits.MaxImageUnpackedBytes": false,
	} {
		e, err := parser.ParseExpr(src)
		if err != nil {
			t.Fatal(err)
		}
		if got := literal(e); got != want {
			t.Errorf("literal(%s) = %v; oczekiwano %v", src, got, want)
		}
	}
}

func TestThresholdsMatchConstants(t *testing.T) {
	md := Markdown()
	for _, want := range []string{"≥ 100, 0 odpowiedzi innych niż 200", "≤ 3m0s", "≥ 50, 0 nieudanych", "≤ 1m0s od odnowienia", "≤ 4m0s", "≤ 200000000 B", "≤ 750ms", "≤ 90 dni", "≤ 2m0s"} {
		if !strings.Contains(md, want) {
			t.Errorf("tabela progów bez %q:\n%s", want, md)
		}
	}
	if n := strings.Count(md, "\n"); n != len(Thresholds())+2 {
		t.Errorf("tabela ma %d wierszy, a progów jest %d", n, len(Thresholds()))
	}
}

func TestCheckExceptions(t *testing.T) {
	today := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	clauses := []string{"6.5", "8.3"}
	ok := Exception{Clause: "6.5", What: "klucz", Reason: "bo", Expires: "2027-01-15", Ref: "decyzja 22"}
	soon := ok
	soon.Expires = "2026-10-20"
	problems, warnings := CheckExceptions([]Exception{ok, soon}, clauses, today)
	if len(problems) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "wygasa 2026-10-20") {
		t.Errorf("ważne wyjątki: %q / %q", problems, warnings)
	}
	for name, change := range map[string]func(*Exception){
		"wygasły":          func(e *Exception) { e.Expires = "2026-10-17" },
		"za daleko":        func(e *Exception) { e.Expires = "2027-02-01" },
		"zła data":         func(e *Exception) { e.Expires = "15.01.2027" },
		"obca klauzula":    func(e *Exception) { e.Clause = "9.9" },
		"bez uzasadnienia": func(e *Exception) { e.Reason = "" },
	} {
		e := ok
		change(&e)
		if problems, _ := CheckExceptions([]Exception{e}, clauses, today); len(problems) != 1 {
			t.Errorf("%s: %q", name, problems)
		}
	}
	if _, err := ParseExceptions([]byte("exceptions:\n  - clause: \"6.5\"\n    wygasa: jutro\n")); err == nil {
		t.Error("nieznane pole przyjęte")
	}
}

// TestRepositoryExceptions: rejestr z repozytorium jest czytelny, a jego
// klauzule istnieją w kontrakcie.
func TestRepositoryExceptions(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(ExceptionsFile)))
	if err != nil {
		t.Fatal(err)
	}
	list, err := ParseExceptions(raw)
	if err != nil || len(list) == 0 {
		t.Fatalf("rejestr: %v, %v", list, err)
	}
	doc, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(Doc)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if !slices.Contains(Clauses(string(doc)), e.Clause) {
			t.Errorf("wyjątek od nieistniejącej klauzuli %s", e.Clause)
		}
	}
}

func TestCommands(t *testing.T) {
	text := "```bash\n./bin/mise run test   # ok\nMISE_ENV=staging ./bin/mise run cluster:drain\n./bin/mise run cluster:drian\n" +
		"./bin/mise exec -- ci/dft check contract\n./bin/mise exec -- ci/dft check kontrakt\nci/dft verify\n```\n" +
		"Proza: dft sprawdza progi, ci/dft jest binarką, a `./bin/mise run cluster:drian` raz jeszcze.\n" +
		"Fragment złamany: `./bin/mise run\nrepo:setings` i `dft contract thresholds`, `dft deploy`.\n" +
		"   ```bash\n   ci/dft check contract\n   ```\n"
	found := Commands("README.md", text, known)
	want := []string{"`./bin/mise run cluster:drian`", "`./bin/mise run repo:setings`", "`dft check kontrakt`", "`dft verify`", "`dft deploy`"}
	if len(found) != len(want) {
		t.Fatalf("Commands = %q", found)
	}
	for i, w := range want {
		if !strings.Contains(found[i], w) {
			t.Errorf("%d: %q, oczekiwano %q", i, found[i], w)
		}
	}
}
