package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/git"
	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/budget"
	"github.com/mateuszf757/Docfind/tools/internal/stages/mainhealth"
	"github.com/mateuszf757/Docfind/tools/internal/stages/plan"
)

const ciUsage = "dft ci require | main-health [--dry-run] | budget [--enforce] | go-cache | plan --base <commit> [--repo <katalog>]"

// runCI — polecenia o samym CI. `dft ci require`: wynik zadania
// zbierającego warstwę (fast-feedback, integration) z wyników jego needs,
// które workflow podaje w zmiennej NEEDS (toJSON(needs)). Zielone tylko
// wtedy, gdy każde zadanie dało success — pominięte to nie zaliczone
// (decyzje 47 i 52).
//
// `dft ci main-health`: czerwony main dłużej niż doba i noc, która nie
// przyszła, w jednym issue (stages/mainhealth, decyzja 54).
func runCI(ctx context.Context, w *workspace, args []string) error {
	// `dft ci go-cache`: kompilacja bramek kodu Go bez uruchamiania testów —
	// pisarz pamięci podręcznej kompilacji w CI (stages/gocode.Warm,
	// decyzja 70). Tag docker — warianty negatywne obrazu.
	if len(args) > 0 && args[0] == "go-cache" {
		if len(args) != 1 {
			return cli.Usage("użycie: %s", ciUsage)
		}
		if err := requireTools("go"); err != nil {
			return err
		}
		c, err := goChecker(w, goVulnMode{})
		if err != nil {
			return err
		}
		return c.Warm(ctx, "docker")
	}
	if len(args) > 0 && args[0] == "plan" {
		return ciPlan(ctx, w, args[1:])
	}
	if len(args) > 0 && args[0] == "budget" {
		if len(args) > 2 || (len(args) == 2 && args[1] != "--enforce") {
			return cli.Usage("użycie: %s", ciUsage)
		}
		if err := requireTools("gh"); err != nil {
			return err
		}
		return ciBudget(ctx, w, len(args) == 2)
	}
	if len(args) > 0 && args[0] == "main-health" {
		if len(args) > 2 || (len(args) == 2 && args[1] != "--dry-run") {
			return cli.Usage("użycie: %s", ciUsage)
		}
		if err := requireTools("gh"); err != nil {
			return err
		}
		// Daty z rejestru wyjątków kontraktu, które wypadają w ciągu tygodnia
		// (§8.7): to samo issue, bo to też stan main, który trzeba ruszyć.
		doc, err := os.ReadFile(filepath.Join(w.root, filepath.FromSlash(contract.Doc)))
		if err != nil {
			return err
		}
		_, dates, err := w.contractExceptions(string(doc))
		if err != nil {
			return err
		}
		c := mainhealth.Checker{Runs: w.gh(), Issues: w.gh(), Dates: dates}
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

// ciBudget — budżety warstw z ostatnich zielonych biegów (stages/budget,
// decyzja 57): raport i podsumowanie biegu; z --enforce przekroczenie
// w BudgetFail biegach to NIESPEŁNIONE, bez — ostrzeżenie.
func ciBudget(ctx context.Context, w *workspace, enforce bool) error {
	v := &cli.Verdict{}
	var reports []budget.Report
	for _, event := range []string{"pull_request", "push", "schedule"} {
		rep, err := budget.Measure(ctx, w.gh(), event, contract.BudgetRuns)
		if err != nil {
			return err
		}
		reports = append(reports, rep)
		fail, warn := rep.Verdict()
		for _, f := range warn {
			cli.Warn("%s", f)
		}
		for _, f := range fail {
			if enforce {
				v.Fail("%s", f)
			} else {
				cli.Warn("%s (bez --enforce: ostrzeżenie)", f)
			}
		}
		if err := appendSummary(rep.Markdown()); err != nil {
			return err
		}
	}
	if err := w.writeReport("budget", reports); err != nil {
		return err
	}
	for _, rep := range reports {
		for _, layer := range slices.Sorted(maps.Keys(rep.Layers)) {
			s := rep.Layers[layer]
			cli.Step("%s %s: p50 %v, p95 %v, budżet %v, przekroczony w %d z %d", rep.Event, layer, s.P50.Round(time.Second), s.P95.Round(time.Second), s.Budget, s.Exceeded, s.N)
		}
	}
	return v.Err("budżety CI")
}

// ciPlan — `dft ci plan`: profil integracji zmiany (stages/plan, decyzja 75).
// Kod planu to drzewo, z którego biegnie dft — w CI commit bazowy PR-a —
// a zmienione pliki są z repozytorium --repo (PR), czytane tylko przez git.
// Wynik: wiersz w logu, wyjście kroku `profile`, podsumowanie i raport
// plan.json.
func ciPlan(ctx context.Context, w *workspace, args []string) error {
	fs := flag.NewFlagSet("ci plan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var base, repo string
	fs.StringVar(&base, "base", "", "")
	fs.StringVar(&repo, "repo", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || base == "" {
		return cli.Usage("użycie: %s", ciUsage)
	}
	if err := requireTools("go", "git"); err != nil {
		return err
	}
	dir := w.root
	if repo != "" {
		abs, err := filepath.Abs(repo)
		if err != nil {
			return err
		}
		dir = abs
	}
	planner := plan.Planner{ModuleDir: filepath.Join(w.root, "tools"), Changes: git.Repo{Dir: dir, Runner: w.runner}, Packages: w.gotool()}
	p, err := planner.Plan(ctx, base)
	if err != nil {
		return err
	}
	var md strings.Builder
	fmt.Fprintf(&md, "### Profil integracji zmiany: %s\n\nDecyzja 75; reguły w `core/ci`, kod planu z commita bazowego.\n\n| Profil | Pliki |\n|---|---|\n", p.Profile)
	for _, profile := range []ci.Profile{ci.ProfileHeavy, ci.ProfileMedium, ci.ProfileFast} {
		files := p.Files[profile]
		if len(files) == 0 {
			continue
		}
		shown := files[:min(len(files), 10)]
		more := ""
		if len(files) > len(shown) {
			more = fmt.Sprintf(" i %d innych", len(files)-len(shown))
		}
		cli.Step("%s: %s%s", profile, strings.Join(shown, ", "), more)
		fmt.Fprintf(&md, "| %s | `%s`%s |\n", profile, strings.Join(shown, "`, `"), more)
	}
	cli.Step("profil zmiany: %s (decyzja 75)", p.Profile)
	if err := githubOutput("profile=" + string(p.Profile) + "\n"); err != nil {
		return err
	}
	if err := appendSummary(md.String()); err != nil {
		return err
	}
	return w.writeReport("plan", p)
}
