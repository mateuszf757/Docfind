// Package mainhealth to zdrowie gałęzi main widziane z biegów CI (§3.6
// kontraktu): main czerwony dłużej niż doba i noc, która nie przyszła.
// Wynik idzie do jednego issue, a nie na czerwono do biegu — czerwony
// bieg, który zgłasza czerwony main, sam podtrzymywałby czerwień.
package mainhealth

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// IssueTitle — tytuł issue; po nim issue jest odnajdywane.
const IssueTitle = "Zdrowie main: czerwony bieg albo brak nocy"

// Runs — biegi workflowu (adapters/github).
type Runs interface {
	WorkflowRuns(ctx context.Context, workflow, branch string, limit int) ([]ci.Run, error)
}

// Issues — issue repozytorium (adapters/github).
type Issues interface {
	FindIssue(ctx context.Context, title string) (int, error)
	CreateIssue(ctx context.Context, title, body string) error
	EditIssue(ctx context.Context, number int, body string) error
	CloseIssue(ctx context.Context, number int, comment string) error
}

// Checker sprawdza biegi workflowu ci na main.
type Checker struct {
	Runs   Runs
	Issues Issues
	// Now — zegar; nil — time.Now.
	Now func() time.Time
}

// red — wynik biegu, który znaczy czerwony main; anulowany i pominięty bieg
// nic nie mówią o kodzie.
var red = []string{"failure", "timed_out", "startup_failure"}

// Findings — co jest nie tak z main, od najważniejszego.
func (c Checker) Findings(ctx context.Context) ([]string, error) {
	runs, err := c.Runs.WorkflowRuns(ctx, "ci.yml", "main", 50)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	var found []string
	var since *ci.Run
	var latest *ci.Run
	for i, run := range runs {
		if run.Event != "push" && run.Event != "schedule" {
			continue
		}
		if latest == nil {
			latest = &runs[i]
		}
		if !contains(red, run.Conclusion) {
			if run.Conclusion == "success" {
				break
			}
			continue
		}
		since = &runs[i]
	}
	if since != nil && latest != nil && contains(red, latest.Conclusion) {
		if at, err := time.Parse(time.RFC3339, since.CreatedAt); err == nil {
			if d := now().Sub(at); d > contract.MainRedLimit {
				found = append(found, fmt.Sprintf("main czerwony od %s (%s): ostatni bieg %s — %s. Revert przed poprawką; w tym czasie tylko poprawki (§3.6 kontraktu).", at.Format("2006-01-02 15:04 UTC"), d.Round(time.Hour), latest.URL, latest.Conclusion))
			}
		}
	}
	var night *ci.Run
	for i, run := range runs {
		if run.Event == "schedule" {
			night = &runs[i]
			break
		}
	}
	switch {
	case night == nil:
		found = append(found, fmt.Sprintf("brak biegu nocnego wśród %d ostatnich biegów main — harmonogram wyłączony albo pomijany", len(runs)))
	default:
		if at, err := time.Parse(time.RFC3339, night.CreatedAt); err == nil {
			if d := now().Sub(at); d > contract.NightlyGap {
				found = append(found, fmt.Sprintf("ostatni bieg nocny %s, %s temu (próg %v): GitHub pomija harmonogram w szczycie i wyłącza go po 60 dniach bez aktywności — %s", at.Format("2006-01-02 15:04 UTC"), d.Round(time.Hour), contract.NightlyGap, night.URL))
			}
		}
	}
	return found, nil
}

// Sync otwiera, aktualizuje albo zamyka issue według Findings. Błąd tylko
// przy awarii narzędzia: zły stan main to treść issue, nie wynik biegu.
func (c Checker) Sync(ctx context.Context) error {
	found, err := c.Findings(ctx)
	if err != nil {
		return err
	}
	number, err := c.Issues.FindIssue(ctx, IssueTitle)
	if err != nil {
		return err
	}
	for _, f := range found {
		cli.Warn("%s", f)
	}
	body := "- " + strings.Join(found, "\n- ") + "\n\nIssue prowadzi `dft ci main-health` z biegów workflowu ci na main (push, noc) — zamknie je sam, gdy main będzie zielony, a noc regularna.\n"
	switch {
	case len(found) > 0 && number == 0:
		if err := c.Issues.CreateIssue(ctx, IssueTitle, body); err != nil {
			return err
		}
		cli.Step("issue „%s” otwarte", IssueTitle)
	case len(found) > 0:
		if err := c.Issues.EditIssue(ctx, number, body); err != nil {
			return err
		}
		cli.Step("issue #%d zaktualizowane", number)
	case number != 0:
		if err := c.Issues.CloseIssue(ctx, number, "main zielony, noc regularna."); err != nil {
			return err
		}
		cli.Step("issue #%d zamknięte — main zielony, noc regularna", number)
	default:
		cli.Step("main zielony albo czerwony krócej niż %v, noc regularna", contract.MainRedLimit)
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
