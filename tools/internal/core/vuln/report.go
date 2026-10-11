package vuln

import (
	"fmt"
	"strings"
	"time"
)

// Report — raport bramki podatności w JSON.
type Report struct {
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

// Summary — raport jako Markdown (podsumowanie biegu, treść issue).
func (r Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Podatności obrazu api\n\n%s — %d podatności, blokujących: %d", r.Target, len(r.Findings), r.Blocking)
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
	b.WriteString("\nBazy: ")
	for i, db := range r.Databases {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s do %s (`%.12s`)", db.Ecosystem, db.Newest, db.SHA256)
	}
	b.WriteString("\n")
	return b.String()
}

// IssueTitle — tytuł issue nocnego skanu; po nim issue jest odnajdywane.
const IssueTitle = "Podatności w obrazie api (skan nocny)"

// IssueBody — treść issue nocnego skanu.
func (r Report) IssueBody() string {
	return r.Summary() + "\nIssue prowadzi `dft vulns-issue` z nocnego biegu workflowu ci — zamknie je sam, gdy skan nie znajdzie blokujących podatności.\n"
}
