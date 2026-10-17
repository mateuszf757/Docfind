package main

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/mainhealth"
)

const ciUsage = "dft ci require | main-health [--dry-run]"

// runCI — polecenia o samym CI. `dft ci require`: wynik zadania
// zbierającego warstwę (fast-feedback, integration) z wyników jego needs,
// które workflow podaje w zmiennej NEEDS (toJSON(needs)). Zielone tylko
// wtedy, gdy każde zadanie dało success — pominięte to nie zaliczone
// (decyzje 47 i 52).
//
// `dft ci main-health`: czerwony main dłużej niż doba i noc, która nie
// przyszła, w jednym issue (stages/mainhealth, decyzja 54).
func runCI(ctx context.Context, w *workspace, args []string) error {
	if len(args) > 0 && args[0] == "main-health" {
		if len(args) > 2 || (len(args) == 2 && args[1] != "--dry-run") {
			return cli.Usage("użycie: %s", ciUsage)
		}
		if err := requireTools("gh"); err != nil {
			return err
		}
		c := mainhealth.Checker{Runs: w.gh(), Issues: w.gh()}
		if len(args) == 1 {
			return c.Sync(ctx)
		}
		// --dry-run: tylko odczyt biegów, bez zmian w issue.
		found, err := c.Findings(ctx)
		if err != nil {
			return err
		}
		for _, f := range found {
			cli.Warn("%s", f)
		}
		cli.Step("[próba] ustaleń: %d — issue bez zmian", len(found))
		return nil
	}
	if len(args) != 1 || args[0] != "require" {
		return cli.Usage("użycie: %s", ciUsage)
	}
	raw := os.Getenv("NEEDS")
	if raw == "" {
		return cli.Usage("%s: brak zmiennej NEEDS — w workflowie NEEDS: ${{ toJSON(needs) }}", ciUsage)
	}
	var needs map[string]ci.Need
	if err := json.Unmarshal([]byte(raw), &needs); err != nil {
		return cli.Usage("%s: NEEDS to nie JSON z wynikami zadań: %v", ciUsage, err)
	}
	if len(needs) == 0 {
		return cli.Usage("%s: NEEDS bez zadań — zadanie zbierające bez needs niczego nie sprawdza", ciUsage)
	}
	unmet := ci.Unmet(needs)
	for _, job := range slices.Sorted(maps.Keys(needs)) {
		cli.Step("%s: %s", job, needs[job].Result)
	}
	if len(unmet) > 0 {
		v := &cli.Verdict{}
		for _, u := range unmet {
			v.Fail("%s — zadanie warstwy bez sukcesu; pominięte to nie zaliczone", u)
		}
		return v.Err("warstwa")
	}
	cli.Step("wszystkie %d zadań warstwy zielone", len(needs))
	return nil
}
