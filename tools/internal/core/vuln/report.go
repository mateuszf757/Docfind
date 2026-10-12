package vuln

import (
	"fmt"
	"strings"
	"time"
)

// Report — raport bramki podatności w JSON.
type Report struct {
	// Subject — co skanowano, w dopełniaczu („obrazu api"): nagłówek raportu
	// i sekcji issue.
	Subject    string     `json:"subject"`
	Target     string     `json:"target"`
	Baseline   string     `json:"baseline,omitempty"`
	Scanner    string     `json:"scanner"`
	ScannedAt  time.Time  `json:"scanned_at"`
	Databases  []Database `json:"databases"`
	Findings   []Finding  `json:"findings"`
	Blocking   int        `json:"blocking"`
	Unused     []string   `json:"unused_exceptions,omitempty"`
	ReportOnly bool       `json:"report_only"`
}

// Summary — raport jako Markdown (podsumowanie biegu, sekcja issue).
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Podatności %s\n\n%s — %d podatności, blokujących: %d", r.Subject, r.Target, len(r.Findings), r.Blocking)
	if r.Baseline != "" {
		fmt.Fprintf(&b, ", punkt odniesienia: %s", r.Baseline)
	}
	if r.ReportOnly {
		b.WriteString(" (tylko raport)")
	}
	b.WriteString("\n\n| Waga | Podatność | Pakiet | Poprawka | Blokuje | Uwagi |\n|---|---|---|---|---|---|\n")
	for _, f := range r.Findings {
		block := ""
		if f.Blocking {
			block = "tak"
		}
		fmt.Fprintf(&b, "| %s | %s | %s %s | %s | %s | %s |\n", f.Severity, f.ID, f.Package, f.Version, strings.Join(f.Fixed, ", "), block, strings.ReplaceAll(f.Note, "|", "/"))
	}
	fmt.Fprintf(&b, "\n%s, bazy: ", r.Scanner)
	for i, db := range r.Databases {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s do %s", db.Ecosystem, db.Newest)
		// Baza Go (vuln.go.dev) nie ma sumy pliku — migawkę wyznacza data.
		if db.SHA256 != "" {
			fmt.Fprintf(&b, " (`%.12s`)", db.SHA256)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// IssueTitle — tytuł issue nocnego skanu; po nim issue jest odnajdywane.
const IssueTitle = "Podatności w obrazie api i narzędziach Go (skan nocny)"

// IssueBody — treść issue nocnego skanu: raport każdego skanu, który się
// wykonał, i lista tych, które raportu nie dały.
func IssueBody(reports []Report, missing []string) string {
	var b strings.Builder
	for _, r := range reports {
		b.WriteString(r.Summary())
		b.WriteString("\n")
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, "**Bez raportu:** %s — skan się nie wykonał, więc o jego podatnościach nic nie wiadomo.\n\n", strings.Join(missing, ", "))
	}
	b.WriteString("Issue prowadzi `dft vulns-issue` z nocnego biegu workflowu ci — zamknie je sam, gdy wszystkie skany wykonają się bez blokujących podatności.\n")
	return b.String()
}
