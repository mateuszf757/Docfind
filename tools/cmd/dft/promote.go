package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/deploy"
	"github.com/mateuszf757/Docfind/tools/internal/env"
	"github.com/mateuszf757/Docfind/tools/internal/github"
)

// promotion — raport promocji w JSON.
type promotion struct {
	Environment string          `json:"environment"`
	Release     deploy.Verified `json:"release"`
	Install     string          `json:"install"`
	Run         string          `json:"run,omitempty"`
}

// runPromote promuje digest wydania na środowisko prod (decyzja 38).
//
// W CI biegnie w zadaniu z `environment: production`, więc rusza dopiero po
// zgodzie autora; najpierw sprawdza, że ta zgoda w ogóle jest wymagana.
// Potem sprawdza obraz tak, jak przed wdrożeniem na prod: wydanie X.Y.Z,
// pochodzenie i SBOM w rejestrze, atestacja GitHuba z tagu tej wersji.
// Do Etapu 10 prod nie ma klastra: promocja kończy się zapisem (GitHub
// Deployment zadania z linkiem do atestacji digestu, raport) i instalacją,
// jaką wykonałby klient.
func runPromote(ctx context.Context, e *environment, args []string) error {
	if err := expectArgs(args, 1, 1, "dft promote <obraz@sha256:…>"); err != nil {
		return err
	}
	if err := requireTools("gh"); err != nil {
		return err
	}
	def, err := env.Load(e.root, "prod")
	if err != nil {
		return err
	}
	cli.Step("promocja na %s (%s)", def.Name, def.Stage)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		if err := github.RequireApproval(ctx, e.runner, os.Getenv("GITHUB_REPOSITORY"), "production"); err != nil {
			return err
		}
		cli.Step("środowisko GitHuba production wymaga zgody — zadanie ruszyło po niej")
	}
	d := deploy.Deployer{Root: e.root, Env: def, Runner: e.runner, Image: imageName("api"), Attest: attestVerifier(e).Verify}
	verified, err := d.VerifyRegistry(ctx, args[0])
	if err != nil {
		return err
	}

	// Instalacja u klienta: chart z tagu wydania, obraz po digeście
	// i wartości z definicji prod — te same, które `dft test` renderuje
	// i sprawdza. Chart OCI zastąpi ścieżkę w Etapie 10.
	values := append(def.AppValues(), "api.image.tag="+verified.Tag, "api.image.digest="+verified.Digest)
	var install strings.Builder
	fmt.Fprintf(&install, "git checkout v%s\nhelm upgrade --install %s deploy/charts/docfind --namespace %s", verified.Version, def.App.Release, def.App.Namespace)
	for _, v := range values {
		fmt.Fprintf(&install, " \\\n  --set %s", v)
	}

	rep := promotion{Environment: def.Name, Release: verified, Install: install.String()}
	if server, repo, run := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_RUN_ID"); run != "" {
		rep.Run = server + "/" + repo + "/actions/runs/" + run
	}
	if def.Cluster.Provider == env.ProviderNone {
		cli.Step("środowisko %s nie ma jeszcze klastra (Etap 10): zapis promocji i instalacja dla klienta", def.Name)
	}
	cli.Step("promowany %s (wersja %s, commit %.12s)", verified.Image, verified.Version, verified.Commit)
	fmt.Fprintln(cli.Out, rep.Install)
	if err := writeReport(e, "promotion-"+def.Name, rep); err != nil {
		return err
	}
	return appendSummary(fmt.Sprintf("### Promocja na %s\n\n| Wersja | Obraz | Commit |\n|---|---|---|\n| %s | `%s` | `%s` |\n\nInstalacja u klienta:\n\n```bash\n%s\n```\n",
		def.Name, verified.Version, verified.Image, verified.Commit, rep.Install))
}
