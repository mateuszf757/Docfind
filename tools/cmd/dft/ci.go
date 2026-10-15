package main

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

const ciUsage = "dft ci require"

// runCI — polecenia o samym CI. `dft ci require`: wynik zadania
// zbierającego warstwę (fast-feedback, integration) z wyników jego needs,
// które workflow podaje w zmiennej NEEDS (toJSON(needs)). Zielone tylko
// wtedy, gdy każde zadanie dało success — pominięte to nie zaliczone
// (decyzje 47 i 52).
func runCI(_ context.Context, _ *workspace, args []string) error {
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
