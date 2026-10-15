// Package contract to kontrakt inżynierski w kodzie (decyzja 50): progi
// bramek, z których biorą je bramki i z których powstaje tabela progów
// w docs/engineering-contracts.md, oraz sprawdzenie, że kontrakt, kod
// i DECYZJE się nie rozjechały.
//
// Próg żyje w jednym miejscu — tutaj — a kontrakt pokazuje tabelę
// wygenerowaną z tego pliku (`dft contract thresholds`). Liczba wpisana
// ręcznie w dwóch miejscach rozjedzie się z kodem (PLAN-D12); tabela
// porównywana z kodem — nie. Zmiana wartości w tym pliku to zmiana
// kontraktu: `dft check contract` na PR-ze wymaga wtedy wpisu
// w docs/DECYZJE.md w tym samym PR-ze.
package contract

import (
	"fmt"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
)

// File — plik progów względem korzenia repozytorium (pole chronione).
const File = "tools/internal/core/contract/thresholds.go"

// Progi bramek. Komentarz mówi, z czego wynika wartość.
const (
	// DrainMinRequests — poniżej tylu żądań na jeden drain pętla nie biegła
	// naprawdę i zero nieudanych nic nie dowodzi (§5.2).
	DrainMinRequests = 100
	// DrainTimeout — najdłuższy `kubectl drain` jednego węzła (§5.2).
	DrainTimeout = 180 * time.Second
	// TLSMinRequests — minimum żądań HTTPS w trakcie odnowienia (§5.4).
	TLSMinRequests = 50
	// TLSPropagation (Y) — od odnowionego certyfikatu do pierwszego
	// handshake'u z nowym numerem seryjnym. Zmierzone w CI z własnym CA:
	// ok. 2 s; 60 s to ~30× zapasu (§5.4).
	TLSPropagation = 60 * time.Second
	// RotationPhaseBudget — faza rotacji sekretu na jednym podzie:
	// refreshInterval ExternalSecret (1 min) + synchronizacja poda przez
	// kubelet (1 min) z zapasem; zmierzone do 84 s w CI (§5.5).
	RotationPhaseBudget = 4 * time.Minute
	// SelfHealBudget — przywrócenie jednej ręcznej zmiany przez Argo CD:
	// samonaprawa po 2 s, kolejna w serii trzy razy później (§5.6).
	SelfHealBudget = time.Minute
	// MaxImageUnpackedBytes — obraz rozpakowany w magazynie Dockera
	// (warunek Etapu 1, §4.4).
	MaxImageUnpackedBytes = 200_000_000
	// MaxOwnShutdown — koszt zamykania aplikacji ponad narzut `docker stop`
	// (warunek Etapu 1, §5.1).
	MaxOwnShutdown = 750 * time.Millisecond
	// InflightDelay — żądanie w locie przy SIGTERM; krótsze niż
	// shutdown_grace_seconds (3 s) z konfiguracji przykładowej (§5.1).
	InflightDelay = 1500 * time.Millisecond
	// RefusalTimeout — po tylu sekundach kontener bez konfiguracji uznaje
	// się za taki, który wstał, zamiast odmówić startu (§5.1).
	RefusalTimeout = 30 * time.Second
	// NegativeVariantBudget — cały przebieg bramki Etapu 1 na wariancie
	// negatywnym prawdziwego obrazu. Wariant zaliczony dopiero po limicie
	// to wynik dobry z niewłaściwego powodu (`sleep 300` zaliczany po pięciu
	// minutach); dziś najdłuższy ok. 42 s (§5.1, §8.1).
	NegativeVariantBudget = 2 * time.Minute
	// VulnsBaselineAncestors — ilu przodków commita bazowego (po pierwszym
	// rodzicu) skan obrazu na PR-ze sprawdza w rejestrze, gdy obrazu commita
	// bazowego jeszcze nie ma: main publikuje obraz ok. 10 min po scaleniu,
	// a każdy commit main ma obraz, chyba że jego build padł (§6.2).
	VulnsBaselineAncestors = 10
)

// Threshold — wiersz tabeli progów.
type Threshold struct {
	Clause string
	Name   string
	Value  string
	Where  string
}

// Thresholds — tabela progów w kolejności kontraktu.
func Thresholds() []Threshold {
	return []Threshold{
		{"§4.4", "rozmiar obrazu rozpakowanego", fmt.Sprintf("≤ %d B", MaxImageUnpackedBytes), "stages/lifecycle"},
		{"§5.1", "koszt własny zamykania ponad `docker stop`", fmt.Sprintf("≤ %v", MaxOwnShutdown), "stages/lifecycle"},
		{"§5.1", "żądanie w locie przy SIGTERM", fmt.Sprintf("%v, odpowiedź 200", InflightDelay), "stages/lifecycle"},
		{"§5.1", "odmowa startu bez konfiguracji", fmt.Sprintf("w ≤ %v", RefusalTimeout), "stages/lifecycle"},
		{"§5.1", "bramka na wariancie negatywnym obrazu", fmt.Sprintf("≤ %v", NegativeVariantBudget), "stages/lifecycle (testy z tagiem docker)"},
		{"§5.2", "żądania na jeden drain", fmt.Sprintf("≥ %d, 0 odpowiedzi innych niż 200", DrainMinRequests), "cmd/dft (drain)"},
		{"§5.2", "`kubectl drain` jednego węzła", fmt.Sprintf("≤ %v", DrainTimeout), "cmd/dft (drain)"},
		{"§5.2", "okres łaski poda ≥ X", "X = preStopSleepSeconds + shutdown_grace_seconds + 1 s (dziś 5 + 3 + 1 = 9 s; okres łaski 15 s)", "chart docfind, _helpers.tpl (render)"},
		{"§5.4", "żądania HTTPS w trakcie odnowienia", fmt.Sprintf("≥ %d, 0 nieudanych, 0 restartów proxy", TLSMinRequests), "cmd/dft (tls)"},
		{"§5.4", "nowy certyfikat na proxy (Y)", fmt.Sprintf("≤ %v od odnowienia", TLSPropagation), "stages/ingress"},
		{"§5.5", "faza rotacji sekretu na podzie", fmt.Sprintf("≤ %v", RotationPhaseBudget), "cmd/dft (rotation)"},
		{"§5.6", "samonaprawa jednej zmiany", fmt.Sprintf("≤ %v", SelfHealBudget), "cmd/dft (selfheal)"},
		{"§6.2", "punkt odniesienia skanu obrazu na PR-ze", fmt.Sprintf("obraz commita bazowego albo najbliższego z %d przodków", VulnsBaselineAncestors), "stages/vulns"},
		{"§6.2", "ważność wyjątku podatności", fmt.Sprintf("≤ %d dni", vuln.MaxExceptionDays), "core/vuln"},
	}
}

// Markdown — tabela progów do wklejenia w kontrakt między znacznikami.
func Markdown() string {
	var b strings.Builder
	b.WriteString("| Klauzula | Próg | Wartość | Gdzie w kodzie |\n|---|---|---|---|\n")
	for _, t := range Thresholds() {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", t.Clause, t.Name, t.Value, t.Where)
	}
	return b.String()
}
