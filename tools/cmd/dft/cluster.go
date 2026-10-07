package main

import (
	"bytes"
	"context"
	"errors"
	stdflag "flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/deploy"
	"github.com/mateuszf757/Docfind/tools/internal/dnstoken"
	"github.com/mateuszf757/Docfind/tools/internal/drain"
	"github.com/mateuszf757/Docfind/tools/internal/env"
	"github.com/mateuszf757/Docfind/tools/internal/github"
	"github.com/mateuszf757/Docfind/tools/internal/kube"
	"github.com/mateuszf757/Docfind/tools/internal/pins"
	"github.com/mateuszf757/Docfind/tools/internal/registry"
	"github.com/mateuszf757/Docfind/tools/internal/tlscheck"
)

// environmentOf wczytuje definicję wybranego środowiska (DOCFIND_ENV, domyślnie dev).
func environmentOf(e *environment) (env.Environment, error) {
	def, err := env.Load(e.root, env.Selected())
	if err != nil {
		return env.Environment{}, err
	}
	if def.Cluster.Provider == env.ProviderNone {
		cli.Step("środowisko %s (%s), bez klastra", def.Name, def.Stage)
	} else {
		cli.Step("środowisko %s (%s), kontekst %s", def.Name, def.Stage, def.Cluster.Context)
	}
	return def, nil
}

func targetOf(e *environment, def env.Environment) kube.Target {
	return kube.Target{Kubeconfig: def.KubeconfigPath(e.root), Context: def.Cluster.Context, APIServer: def.Cluster.APIServer}
}

// connect łączy się z klastrem środowiska przez strażnika: klaster musi być
// w definicji, a kubeconfig wskazywać jego kontekst i serwer.
func connect(e *environment, def env.Environment) (*kube.Clients, error) {
	if err := def.RequireCluster(); err != nil {
		return nil, err
	}
	return kube.Connect(targetOf(e, def))
}

// artifactArgs czyta, skąd wziąć obraz: bez flag — build z drzewa;
// --image repozytorium@sha256:… — z rejestru; --image-archive i
// --config-digest — archiwum z zadania build.
func artifactArgs(args []string) (deploy.Artifact, error) {
	const usage = "dft cluster up [--image <obraz>@sha256:… | --image-archive <plik.tar> --config-digest sha256:…]"
	fs := stdflag.NewFlagSet("cluster up", stdflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var art deploy.Artifact
	fs.StringVar(&art.Ref, "image", "", "")
	fs.StringVar(&art.Archive, "image-archive", "", "")
	fs.StringVar(&art.ConfigDigest, "config-digest", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return art, cli.Usage("użycie: %s", usage)
	}
	switch {
	case art.Ref != "" && art.Archive == "" && art.ConfigDigest == "":
		art.Source = env.SourceRegistry
	case art.Ref == "" && art.Archive != "" && art.ConfigDigest != "":
		art.Source = env.SourceArchive
	case art.Ref == "" && art.Archive == "" && art.ConfigDigest == "":
		art.Source = env.SourceLocal
	default:
		return art, cli.Usage("użycie: %s", usage)
	}
	return art, nil
}

func runCluster(ctx context.Context, e *environment, args []string) error {
	const usage = "dft cluster <up [źródło obrazu]|down>"
	if len(args) == 0 || (args[0] == "down" && len(args) > 1) {
		return cli.Usage("użycie: %s", usage)
	}
	def, err := environmentOf(e)
	if err != nil {
		return err
	}
	p, err := pins.Load(e.root)
	if err != nil {
		return err
	}
	d := deploy.Deployer{Root: e.root, Env: def, Runner: e.runner, Pins: p, Builder: builder(e), Service: "api", Image: imageName("api")}
	switch args[0] {
	case "up":
		art, err := artifactArgs(args[1:])
		if err != nil {
			return err
		}
		if err := requireTools("k3d", "kubectl", "helm", "docker"); err != nil {
			return err
		}
		return d.Up(ctx, art)
	case "down":
		if err := requireTools("k3d"); err != nil {
			return err
		}
		return d.Down(ctx)
	default:
		return cli.Usage("użycie: %s", usage)
	}
}

func runCheckDrain(ctx context.Context, e *environment, args []string) (err error) {
	if err := expectArgs(args, 0, 0, "dft check drain   (DRAIN_NODE=<węzeł> — tylko ten węzeł)"); err != nil {
		return err
	}
	def, err := environmentOf(e)
	if err != nil {
		return err
	}
	if err := def.Require(env.OpDrain); err != nil {
		return err
	}
	if err := requireTools("kubectl", "go", "k3d"); err != nil {
		return err
	}
	target := targetOf(e, def)
	clients, err := connect(e, def)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "dft-drain-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()

	// Sonda budowana ze źródeł tego commita i importowana do węzłów — jak
	// obraz API w dev; obraz bez bazy, powtarzalny z konstrukcji.
	binary, err := drain.BuildProbeBinary(ctx, e.runner, filepath.Join(e.root, "tools"), tmp)
	if err != nil {
		return err
	}
	img, err := drain.ProbeImage(binary)
	if err != nil {
		return err
	}
	ref, err := drain.ProbeTag(img)
	if err != nil {
		return err
	}
	cli.Step("sonda %s", ref)
	if err := drain.ImportProbe(ctx, e.runner, img, ref, def.Cluster.Name, tmp); err != nil {
		return err
	}

	deployment := def.App.Release + "-api"
	gate := &drain.Gate{
		Core:    clients.Core,
		Kubectl: e.runner,
		Target:  target,
		Cfg: drain.Config{
			Namespace:     def.App.Namespace,
			Deployment:    deployment,
			Container:     "api",
			ServiceURL:    fmt.Sprintf("http://%s.%s.svc.cluster.local/search?q=drain", deployment, def.App.Namespace),
			ProbeImage:    ref,
			Node:          os.Getenv("DRAIN_NODE"),
			MinRequests:   100,
			DrainTimeout:  180 * time.Second,
			RolloutWait:   120 * time.Second,
			SettleAfter:   5 * time.Second,
			LeaseDuration: 60 * time.Second,
			LeaseRenew:    15 * time.Second,
			RunID:         time.Now().UTC().Format("20060102t150405") + fmt.Sprintf("-%d", os.Getpid()),
		},
	}
	report, err := gate.Run(ctx)
	if reportErr := writeReport(e, "drain-"+def.Name, report); reportErr != nil {
		return errors.Join(err, reportErr)
	}
	return err
}

func runCheckTLS(ctx context.Context, e *environment, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check tls   (DOCFIND_TLS_RENEW_PRODUCTION=1 — także produkcyjny Let's Encrypt)"); err != nil {
		return err
	}
	def, err := environmentOf(e)
	if err != nil {
		return err
	}
	if err := requireTools("cmctl"); err != nil {
		return err
	}
	renewProduction, err := flag("DOCFIND_TLS_RENEW_PRODUCTION")
	if err != nil {
		return err
	}
	target := targetOf(e, def)
	clients, err := connect(e, def)
	if err != nil {
		return err
	}
	checker := &tlscheck.Checker{
		Core: clients.Core, Dynamic: clients.Dynamic, Cmctl: e.runner,
		Cfg: tlscheck.Config{
			GatewayNamespace: def.Gateway.Namespace,
			Gateway:          def.Gateway.Name,
			AppNamespace:     def.App.Namespace,
			Route:            def.App.Release + "-api",
			HTTPPort:         def.Cluster.Ports.HTTP,
			HTTPSPort:        def.Cluster.Ports.HTTPS,
			RenewProduction:  renewProduction,
			AllowRenewal:     def.Allows(env.OpTLSRenew),
			MinRequests:      50,
			KubectlArgs:      target.KubectlArgs(),
		},
	}
	report, err := checker.Run(ctx)
	if reportErr := writeReport(e, "tls-"+def.Name, report); reportErr != nil {
		return errors.Join(err, reportErr)
	}
	return err
}

// runCheckIdentity sprawdza, że na klastrze działa obraz zbudowany z tego
// drzewa — ten sam test, którym kończy się `dft cluster up`, osobno.
func runCheckIdentity(ctx context.Context, e *environment, args []string) error {
	if err := expectArgs(args, 0, 1, "dft check identity [obraz:tag]"); err != nil {
		return err
	}
	def, err := environmentOf(e)
	if err != nil {
		return err
	}
	clients, err := connect(e, def)
	if err != nil {
		return err
	}
	deployment := def.App.Release + "-api"
	image := ""
	if len(args) == 1 {
		image = args[0]
	} else {
		dep, err := clients.Core.AppsV1().Deployments(def.App.Namespace).Get(ctx, deployment, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, c := range dep.Spec.Template.Spec.Containers {
			if c.Name == "api" {
				image = c.Image
			}
		}
	}
	want, err := expectedIdentity(ctx, e, image)
	if err != nil {
		return err
	}
	results, err := kube.VerifyDeployment(ctx, clients.Core, def.App.Namespace, deployment, "api", want)
	if reportErr := writeReport(e, "identity-"+def.Name, results); reportErr != nil {
		return errors.Join(err, reportErr)
	}
	return err
}

// expectedIdentity — czego oczekiwać od podów z obrazem image. Obraz po
// digeście pochodzi z rejestru i niesie wersję i commit w etykietach; to,
// co tam leży, jest punktem odniesienia. Obraz z tagiem lokalnym zbudowano
// z drzewa roboczego — punktem odniesienia jest git.
func expectedIdentity(ctx context.Context, e *environment, image string) (kube.Expected, error) {
	if strings.Contains(image, "@") {
		// repozytorium:tag@digest — tag jest tylko etykietą, pobiera się digest.
		ref, err := name.NewDigest(image)
		if err != nil {
			return kube.Expected{}, fmt.Errorf("obraz %q: %w", image, err)
		}
		published, err := registry.Inspect(ref.Context().Name()+"@"+ref.DigestStr(), registry.Options(ctx)...)
		if err != nil {
			return kube.Expected{}, err
		}
		cli.Step("punkt odniesienia: etykiety obrazu w rejestrze (%s)", ref.DigestStr())
		return kube.Expected{Image: image, Version: published.Labels[registry.VersionLabel], Commit: published.Labels[registry.RevisionLabel]}, nil
	}
	repo := repoOf(e)
	version, err := repo.Version(ctx)
	if err != nil {
		return kube.Expected{}, err
	}
	commit, err := repo.Commit(ctx)
	if err != nil {
		return kube.Expected{}, err
	}
	return kube.Expected{Image: image, Version: version, Commit: commit}, nil
}

func runDNSToken(ctx context.Context, e *environment, args []string) error {
	if err := expectArgs(args, 0, 0, "dft dns-token   (token z terminala bez echa albo z potoku; DOCFIND_ACME_ZONE — sprawdza też strefę)"); err != nil {
		return err
	}
	def, err := environmentOf(e)
	if err != nil {
		return err
	}
	if err := def.Require(env.OpDNSToken); err != nil {
		return err
	}
	var token string
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprint(cli.Err, "Token API Cloudflare (nie będzie widoczny): ")
		raw, err := term.ReadPassword(fd)
		fmt.Fprintln(cli.Err)
		if err != nil {
			return err
		}
		token, err = dnstoken.ReadToken(bytes.NewReader(raw))
		if err != nil {
			return err
		}
	} else {
		token, err = dnstoken.ReadToken(os.Stdin)
		if err != nil {
			return err
		}
	}
	if err := (dnstoken.Verifier{BaseURL: dnstoken.DefaultAPI}).Verify(ctx, token, def.Gateway.ACMEZone); err != nil {
		return err
	}
	clients, err := connect(e, def)
	if err != nil {
		return err
	}
	if err := dnstoken.Store(ctx, clients.Core, token); err != nil {
		return err
	}
	cli.Step("Secret %s/%s zapisany", dnstoken.SecretNamespace, dnstoken.SecretName)
	return nil
}

func runRepoSettings(ctx context.Context, e *environment, args []string) error {
	if err := expectArgs(args, 0, 1, "dft repo-settings [--dry-run]"); err != nil {
		return err
	}
	if len(args) == 1 && args[0] != "--dry-run" {
		return cli.Usage("użycie: dft repo-settings [--dry-run]")
	}
	if err := requireTools("gh"); err != nil {
		return err
	}
	return github.Settings{Root: e.root, Runner: e.runner, DryRun: len(args) == 1}.Apply(ctx)
}
