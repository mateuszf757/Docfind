package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/kube"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/registry"
	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/drain"
	"github.com/mateuszf757/Docfind/tools/internal/stages/identity"
	"github.com/mateuszf757/Docfind/tools/internal/stages/ingress"
	"github.com/mateuszf757/Docfind/tools/internal/stages/promote"
	"github.com/mateuszf757/Docfind/tools/internal/stages/release"
	"github.com/mateuszf757/Docfind/tools/internal/stages/reposync"
	"github.com/mateuszf757/Docfind/tools/internal/stages/trial"
	"github.com/mateuszf757/Docfind/tools/internal/stages/vulns"
)

// runPromote promuje wydanie na środowisko prod (decyzje 38 i 40): digest
// z main z atestacją wydania o numerze z tagu.
func runPromote(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 2, 2, "dft promote <obraz@sha256:…> <vX.Y.Z>"); err != nil {
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
	promotion, err := p.Promote(ctx, args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Fprintln(cli.Out, promotion.Install)
	if err := w.writeReport("promotion-"+def.Name, promotion); err != nil {
		return err
	}
	return appendSummary(promotion.Summary())
}

// runRelease — wydanie u autora (decyzja 40): akceptacja na stagingu
// obrazu, który tam stoi, i tag z adnotacją na jego commicie. Tagu nie
// wypycha: push robi autor, a CI na tagu dopisuje numer w rejestrze,
// wystawia atestację wydania i czeka na zgodę na promocję.
func runRelease(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 1, 2, `dft release <vX.Y.Z> ["tytuł wydania"]`); err != nil {
		return err
	}
	if err := requireTools("gh", "kubectl", "go", "k3d", "cmctl"); err != nil {
		return err
	}
	def, err := environment.Load(w.root, "staging")
	if err != nil {
		return err
	}
	cli.Step("akceptacja na %s (%s), kontekst %s", def.Name, def.Stage, def.Cluster.Context)
	c, err := w.connect(def)
	if err != nil {
		return err
	}
	title := ""
	if len(args) == 2 {
		title = args[1]
	}
	r, err := release.Acceptor{
		Staging: def, Repository: imageName("api"), Cluster: c, Admission: w.admission(),
		Gates: acceptanceGates{w: w, def: def, cluster: c}, Git: w.git(),
	}.Accept(ctx, args[0], title)
	if err != nil {
		return err
	}
	if err := w.writeReport("release-"+r.Version, r); err != nil {
		return err
	}
	cli.Step("tag %s gotowy; wypchnięcie uruchamia wydanie w CI: git push origin %s", r.Tag(), r.Tag())
	return nil
}

// acceptanceGates — bramki akceptacji na stagingu: te same co `dft check
// identity`, `drain` i `tls`, z raportami w katalogu raportów.
type acceptanceGates struct {
	w       *workspace
	def     environment.Definition
	cluster *kube.Cluster
}

func (g acceptanceGates) Identity(ctx context.Context) ([]identity.Pod, error) {
	return g.w.identityGate(ctx, g.def, g.cluster, "")
}

func (g acceptanceGates) Drain(ctx context.Context) (drain.Report, error) {
	return g.w.drainGate(ctx, g.def, g.cluster, "")
}

func (g acceptanceGates) Ingress(ctx context.Context) (ingress.Report, error) {
	return g.w.ingressGate(ctx, g.def, g.cluster)
}

// runReleaseImage — wydanie w CI na wypchniętym tagu: zapis z tagu,
// pochodzenie obrazu z main i tag z numerem w rejestrze — ten sam digest,
// bez builda. Wyjścia kroku: obraz, digest, numer oraz typ i treść
// predykatu atestacji wydania dla następnych zadań.
func runReleaseImage(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 1, 1, "dft release-image <vX.Y.Z>   (w CI na wypchniętym tagu)"); err != nil {
		return err
	}
	if err := requireTools("gh"); err != nil {
		return err
	}
	head, err := w.git().Commit(ctx)
	if err != nil {
		return err
	}
	r, err := release.Publisher{Repository: imageName("api"), Tags: w.gh(), Admission: w.admission(), Registry: registry.Client{}}.Publish(ctx, args[0], head)
	if err != nil {
		return err
	}
	repository, digest, err := artifact.ParseRef(r.Image)
	if err != nil {
		return err
	}
	predicate, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := githubOutput(fmt.Sprintf("image=%s\ndigest=%s\nversion=%s\npredicate-type=%s\npredicate=%s\n",
		repository, digest, r.Version, artifact.ReleasePredicate, predicate)); err != nil {
		return err
	}
	if err := w.writeReport("release-"+r.Version, r); err != nil {
		return err
	}
	return appendSummary(fmt.Sprintf("### Wydanie %s\n\n%s\n\n| Obraz (ten sam digest co na stagingu) | Commit | Akceptacja na stagingu |\n|---|---|---|\n| `%s:%s@%s` | `%s` | %s: %s |\n",
		r.Version, r.Title, repository, r.Version, digest, artifact.ShortCommit(r.Commit), r.AcceptedAt, r.Staging))
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
