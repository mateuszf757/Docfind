package lint

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Pliki gitleaks w repozytorium — podawane wprost, nigdy szukane przez
// gitleaks w skanowanym katalogu.
const (
	GitleaksConfig = "ci/gitleaks.toml"
	GitleaksIgnore = "ci/gitleaks-ignore"
)

// leaksFound — kod wyjścia gitleaks przy znalezionych sekretach. Inny niż
// domyślne 1, które gitleaks zwraca też przy awarii (brak katalogu, zły
// config): wynik negatywny nie może wyglądać jak awaria i odwrotnie.
const leaksFound = 7

// Leak — ustalenie gitleaks z raportu. Raport powstaje z --redact, więc
// wartości sekretu w nim nie ma; Fingerprint (commit:plik:reguła:linia)
// to wpis do GitleaksIgnore.
type Leak struct {
	RuleID      string `json:"RuleID"`
	File        string `json:"File"`
	StartLine   int    `json:"StartLine"`
	Commit      string `json:"Commit"`
	Fingerprint string `json:"Fingerprint"`
}

// Secrets — skan historii gita repozytorium w Root.
type Secrets struct {
	Runner proc.Runner
	Root   string
	// Report — raport JSON (bez wartości sekretów).
	Report string
	// Out i Err — wyjście gitleaks na bieżąco.
	Out, Err io.Writer
}

// Check skanuje całą historię sprawdzanego commita (HEAD) — z diffem
// każdego scalenia względem pierwszego rodzica, więc także treść dopisaną
// przy rozwiązywaniu konfliktu, której zwykły `git log -p` nie pokazuje.
//
// HEAD, a nie wszystkie refy: wynik ma zależeć od tego, co sprawdzamy, a nie
// od innych gałęzi w klonie. Z --all PR oceniał cudze gałęzie swoją
// konfiguracją — gałąź, która w sobie dodaje wyjątek, wywracała PR, który go
// jeszcze nie ma. W CI na PR-ze HEAD to scalenie z main, więc historia main
// jest w skanie; inne gałęzie sprawdzają ich własne PR-y, a push z sekretem
// zatrzymuje push protection GitHuba (decyzja 41).
//
// Konfiguracja i lista wyjątków tylko z ci/, podane wprost: domyślnie
// gitleaks bierze .gitleaks.toml i .gitleaksignore ze skanowanego
// katalogu, więc PR mógłby dopisać plik, który wyłącza skan dla samego
// siebie. Z tego samego powodu komentarz `gitleaks:allow` niczego nie
// zwalnia — wyjątek to odcisk w ci/gitleaks-ignore, widoczny w recenzji.
func (s Secrets) Check(ctx context.Context) ([]Leak, error) {
	if err := os.MkdirAll(filepath.Dir(s.Report), 0o755); err != nil {
		return nil, err
	}
	cli.Step("gitleaks: cała historia gita")
	_, err := s.Runner.Run(ctx, proc.Cmd{
		Name: "gitleaks",
		Args: []string{
			"git", "--no-banner", "--no-color", "--redact",
			"--exit-code", fmt.Sprint(leaksFound),
			"--config", filepath.Join(s.Root, GitleaksConfig),
			"--gitleaks-ignore-path", filepath.Join(s.Root, GitleaksIgnore),
			"--ignore-gitleaks-allow",
			"--log-opts=--diff-merges=first-parent HEAD",
			"--report-format", "json", "--report-path", s.Report,
			s.Root,
		},
		Stdout: s.Out, Stderr: s.Err,
	})
	switch code := proc.ExitCode(err); {
	case err == nil:
		return nil, nil
	case code == leaksFound:
		leaks, readErr := readLeaks(s.Report)
		if readErr != nil {
			return nil, readErr
		}
		return leaks, cli.Unmet("gitleaks: %d sekretów w historii gita — wyżej miejsca; sekret unieważnij u wystawcy, sama zmiana historii go nie odbiera", len(leaks))
	default:
		return nil, fmt.Errorf("gitleaks nie wykonał się — awaria narzędzia, nie wynik: %v", err)
	}
}

func readLeaks(path string) ([]Leak, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var leaks []Leak
	if err := json.Unmarshal(raw, &leaks); err != nil {
		return nil, fmt.Errorf("raport gitleaks %s: %w", path, err)
	}
	return leaks, nil
}
