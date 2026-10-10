package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/promote"
	"github.com/mateuszf757/Docfind/tools/internal/stages/reposync"
	"github.com/mateuszf757/Docfind/tools/internal/stages/trial"
	"github.com/mateuszf757/Docfind/tools/internal/stages/vulns"
)

// runPromote promuje digest wydania na środowisko prod (decyzja 38).
func runPromote(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 1, 1, "dft promote <obraz@sha256:…>"); err != nil {
		return err
	}
	if err := requireTools("gh"); err != nil {
		return err
	}
	def, err := environment.Load(w.root, "prod")
	if err != nil {
		return err
	}
	p := promote.Promoter{Env: def, Repository: imageName("api"), Admission: w.admission(), Run: runURL()}
	// W CI zadanie promocji czeka na zgodę w środowisku GitHuba production;
	// poza CI nikt na nią nie czeka.
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		p.Approvals, p.GitHubRepo, p.GitHubEnvironment = w.gh(), os.Getenv("GITHUB_REPOSITORY"), "production"
	}
	promotion, err := p.Promote(ctx, args[0])
	if err != nil {
		return err
	}
	fmt.Fprintln(cli.Out, promotion.Install)
	if err := w.writeReport("promotion-"+def.Name, promotion); err != nil {
		return err
	}
	return appendSummary(promotion.Summary())
}

func runRepoSettings(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 1, "dft repo-settings [--dry-run]"); err != nil {
		return err
	}
	if len(args) == 1 && args[0] != "--dry-run" {
		return cli.Usage("użycie: dft repo-settings [--dry-run]")
	}
	if err := requireTools("gh"); err != nil {
		return err
	}
	return reposync.Settings{Root: w.root, GitHub: w.gh(), DryRun: len(args) == 1}.Apply(ctx)
}

// runTrial mówi, czy zadania CI skończyły okres próbny i ich statusy mogą
// trafić do wymaganych w regule main (decyzje 36 i 37).
func runTrial(ctx context.Context, w *workspace, args []string) error {
	const usage = "dft trial <N> <zadanie>…"
	if len(args) < 2 {
		return cli.Usage("użycie: %s", usage)
	}
	need, err := strconv.Atoi(args[0])
	if err != nil || need < 1 {
		return cli.Usage("użycie: %s — N to dodatnia liczba biegów", usage)
	}
	if err := requireTools("gh"); err != nil {
		return err
	}
	v := &cli.Verdict{}
	for _, job := range args[1:] {
		res, err := trial.Checker{Runs: w.gh(), Workflow: "ci.yml", Job: job, Branch: "main", Events: []string{"push", "schedule"}, Limit: 50}.Streak(ctx)
		if err != nil {
			return err
		}
		for _, r := range res.Counted {
			cli.Step("%s zielony: %s %s %.7s %s", job, r.CreatedAt, r.Event, r.HeadSHA, r.URL)
		}
		if res.Broken != nil {
			cli.Step("%s: serię przerywa %s %s — %s (%s)", job, res.Broken.CreatedAt, res.Broken.Event, res.Reason, res.Broken.URL)
		}
		if res.Streak < need {
			v.Fail("okres próbny %s trwa: %d z %d zielonych biegów z rzędu", job, res.Streak, need)
			continue
		}
		cli.Step("%s: %d zielonych biegów z rzędu — status można dopisać do .github/rulesets/main.json", job, res.Streak)
	}
	if err := v.Err("okres próbny"); err != nil {
		return err
	}
	cli.Step("wszystkie zadania po okresie próbnym — zastosuj regułę: ./bin/mise run repo:settings")
	return nil
}

// runVulnsIssue — wynik nocnego skanu do issue (stages/vulns).
func runVulnsIssue(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 1, 1, "dft vulns-issue <raport vulns-api.json>"); err != nil {
		return err
	}
	if err := requireTools("gh"); err != nil {
		return err
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var rep vuln.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return fmt.Errorf("raport %s: %w", args[0], err)
	}
	return vulns.SyncIssue(ctx, w.gh(), rep)
}
