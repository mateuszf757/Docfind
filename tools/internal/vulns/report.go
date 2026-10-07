package vulns

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
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

// SyncIssue przenosi wynik nocnego skanu do issue zamiast czerwieni na main
// (decyzja 37): blokujące podatności — issue otwarte albo zaktualizowane;
// brak blokujących — otwarte issue zamknięte z komentarzem.
func SyncIssue(ctx context.Context, r proc.Runner, rep Report) error {
	gh := func(args ...string) ([]byte, error) {
		res, err := r.Run(ctx, proc.Cmd{Name: "gh", Args: args})
		if err != nil {
			return nil, fmt.Errorf("gh %s: %w", strings.Join(args[:2], " "), err)
		}
		return res.Stdout, nil
	}
	out, err := gh("issue", "list", "--state", "open", "--search", IssueTitle+" in:title", "--json", "number,title")
	if err != nil {
		return err
	}
	var issues []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(out, &issues); err != nil {
		return fmt.Errorf("gh issue list: %w", err)
	}
	number := 0
	for _, i := range issues {
		if i.Title == IssueTitle {
			number = i.Number
		}
	}
	body := rep.Summary() + "\nIssue prowadzi `dft vulns-issue` z nocnego biegu workflowu ci — zamknie je sam, gdy skan nie znajdzie blokujących podatności.\n"
	switch {
	case rep.Blocking > 0 && number == 0:
		if _, err := gh("issue", "create", "--title", IssueTitle, "--body", body); err != nil {
			return err
		}
		cli.Step("issue „%s” otwarte: %d blokujących", IssueTitle, rep.Blocking)
	case rep.Blocking > 0:
		if _, err := gh("issue", "edit", strconv.Itoa(number), "--body", body); err != nil {
			return err
		}
		cli.Step("issue #%d zaktualizowane: %d blokujących", number, rep.Blocking)
	case number != 0:
		if _, err := gh("issue", "close", strconv.Itoa(number), "--comment", fmt.Sprintf("Skan z %s: brak blokujących podatności.", rep.ScannedAt.Format("2006-01-02"))); err != nil {
			return err
		}
		cli.Step("issue #%d zamknięte — brak blokujących podatności", number)
	default:
		cli.Step("brak blokujących podatności, brak otwartego issue")
	}
	return nil
}
