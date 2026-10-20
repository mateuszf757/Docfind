package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
	"github.com/mateuszf757/Docfind/tools/internal/stages/budget"
	"github.com/mateuszf757/Docfind/tools/internal/stages/mainhealth"
	"github.com/mateuszf757/Docfind/tools/internal/stages/plan"
)

const ciUsage = "dft ci require | main-health [--dry-run] | budget [--enforce] | go-cache | plan --base <commit> [--repo <katalog>] | plan-trial | container <zadanie mise> [argumenty]"

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
	if len(args) > 0 && args[0] == "container" {
		if len(args) < 2 {
			return cli.Usage("użycie: %s", ciUsage)
		}
		if err := requireTools("docker"); err != nil {
			return err
		}
		return ciContainer(ctx, w, args[1], args[2:])
	}
	if len(args) > 0 && args[0] == "plan-trial" {
		if len(args) != 1 {
			return cli.Usage("użycie: %s", ciUsage)
		}
		if err := requireTools("gh"); err != nil {
			return err
		}
		return ciPlanTrial(ctx, w)
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

// ciPlanTrial — `dft ci plan-trial`: tryb cienia planu (stages/plan.Shadow,
// decyzja 77) — profil z artefaktu planu kontra wynik cluster na ostatnich
// biegach PR-ów. Tylko informuje: rozjazd to ostrzeżenie do przejrzenia,
// a nie czerwona noc; gotowość do włączenia planu (krok 9) — wiersz w logu
// i podsumowaniu.
func ciPlanTrial(ctx context.Context, w *workspace) (err error) {
	tmp, err := os.MkdirTemp("", "dft-plan-trial-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
	res, err := plan.Shadow(ctx, w.gh(), tmp, 100)
	if err != nil {
		return err
	}
	counts := fmt.Sprintf("fast %d, medium %d, heavy %d", res.ByProfile[ci.ProfileFast], res.ByProfile[ci.ProfileMedium], res.ByProfile[ci.ProfileHeavy])
	cli.Step("plan w trybie cienia: %d biegów PR-ów z planem (%s)", res.Runs, counts)
	for _, m := range res.Mismatches {
		cli.Warn("rozjazd planu z klastrem: %s", m)
	}
	verdict := fmt.Sprintf("jeszcze nie: %d z %d biegów z planem, rozjazdów: %d", res.Runs, contract.PlanShadowRuns, len(res.Mismatches))
	if res.Ready() {
		verdict = fmt.Sprintf("gotowe do włączenia planu: %d biegów bez rozjazdu (krok 9, decyzja 77)", res.Runs)
	}
	cli.Step("%s", verdict)
	md := fmt.Sprintf("### Plan w trybie cienia\n\n%d biegów PR-ów z planem (%s), rozjazdów: %d. %s.\n", res.Runs, counts, len(res.Mismatches), verdict)
	for _, m := range res.Mismatches {
		md += "- " + m + "\n"
	}
	if err := appendSummary(md); err != nil {
		return err
	}
	return w.writeReport("plan-trial", res)
}

// ciImageFiles — pliki, z których powstaje obraz ci-light: jego tag to ich
// suma, więc obraz buduje się od nowa tylko po ich zmianie.
var ciImageFiles = []string{"ci/images/Dockerfile", "ci/images/Dockerfile.dockerignore", "bin/mise"}

// ciImageTag — tag obrazu ci-light z sumy jego plików.
func ciImageTag(root string) (string, error) {
	h := sha256.New()
	for _, rel := range ciImageFiles {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %d\n", rel, len(raw))
		h.Write(raw)
	}
	return "docfind-ci-light:" + hex.EncodeToString(h.Sum(nil))[:12], nil
}

// containerArgs — `docker run` dla zadania mise w obrazie ci-light:
// repozytorium pod tą samą ścieżką (narzędzia z .mise repozytorium działają
// bez zmian), pamięć podręczna Go w katalogu hosta, żeby kolejne biegi były
// ciepłe. Docker rootless mapuje roota kontenera na użytkownika hosta;
// klasyczny — dostaje jego uid i gid, żeby pliki w repozytorium nie należały
// do roota.
func containerArgs(root, cache, image string, rootless bool, uid, gid int, task string, taskArgs []string) []string {
	args := []string{"run", "--rm", "-v", root + ":" + root, "-w", root, "-v", cache + ":/cache",
		"-e", "GOCACHE=/cache/go-build", "-e", "GOMODCACHE=/cache/go-mod"}
	if !rootless {
		args = append(args, "--user", fmt.Sprintf("%d:%d", uid, gid), "-e", "HOME=/tmp")
	}
	args = append(args, image, "./bin/mise", "run", task)
	if len(taskArgs) > 0 {
		args = append(append(args, "--"), taskArgs...)
	}
	return args
}

// ciContainer — `dft ci container <zadanie> [argumenty]`: zadanie mise
// w obrazie ci-light (decyzja 78), np. `test -- --part go` z detektorem
// wyścigów, którego host bez kompilatora C nie uruchomi. Obraz buduje się
// przy pierwszym użyciu i po zmianie jego plików. W kontenerze nie ma
// Dockera — zadania z obrazami i klastrem biegną na hoście.
func ciContainer(ctx context.Context, w *workspace, task string, taskArgs []string) error {
	image, err := ciImageTag(w.root)
	if err != nil {
		return err
	}
	d := w.docker()
	exists, err := d.ImageExists(ctx, image)
	if err != nil {
		return err
	}
	if !exists {
		cli.Step("build obrazu %s (ci/images/Dockerfile, etap light)", image)
		if err := d.BuildTarget(ctx, filepath.Join(w.root, "ci", "images", "Dockerfile"), "light", image, w.root, os.Stdout, os.Stderr); err != nil {
			return err
		}
	}
	rootless, err := d.Rootless(ctx)
	if err != nil {
		return err
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	cache := filepath.Join(base, "docfind-ci")
	for _, dir := range []string{"go-build", "go-mod"} {
		if err := os.MkdirAll(filepath.Join(cache, dir), 0o755); err != nil {
			return err
		}
	}
	cli.Step("zadanie %s w %s", task, image)
	return containerResult(task, d.RunAttached(ctx, containerArgs(w.root, cache, image, rootless, os.Getuid(), os.Getgid(), task, taskArgs), os.Stdout, os.Stderr))
}

// containerResult — kod zadania z kontenera jako wynik dft: 1 to warunki
// niespełnione (już wypisane w kontenerze), inny kod — awaria.
func containerResult(task string, err error) error {
	switch code := proc.ExitCode(err); {
	case err == nil:
		return nil
	case code == cli.ExitUnmet:
		return &cli.UnmetError{Msg: fmt.Sprintf("zadanie %s w kontenerze: warunki niespełnione", task), Reported: true}
	default:
		return fmt.Errorf("zadanie %s w kontenerze: %w", task, err)
	}
}
