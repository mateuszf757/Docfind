package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/term"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/cloudflare"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/host"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/httpclient"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/kube"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/registry"
	"github.com/mateuszf757/Docfind/tools/internal/core/clock"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/fetch"
	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
	"github.com/mateuszf757/Docfind/tools/internal/stages/deploy"
	"github.com/mateuszf757/Docfind/tools/internal/stages/dnstoken"
	"github.com/mateuszf757/Docfind/tools/internal/stages/drain"
	"github.com/mateuszf757/Docfind/tools/internal/stages/evidence"
	"github.com/mateuszf757/Docfind/tools/internal/stages/identity"
	"github.com/mateuszf757/Docfind/tools/internal/stages/ingress"
)

// sourceArgs czyta, skąd wziąć obraz: bez flag — z definicji środowiska
// (build z drzewa albo obraz z charta, który wybierze Argo CD); --image
// repozytorium@sha256:… — z rejestru; --image-archive i --config-digest —
// archiwum z zadania build.
func sourceArgs(args []string) (deploy.Source, error) {
	const usage = "dft cluster up [--image <obraz>@sha256:… [--chart-package] | --image-archive <plik.tar> --config-digest sha256:…]"
	fs := flag.NewFlagSet("cluster up", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var src deploy.Source
	fs.StringVar(&src.Ref, "image", "", "")
	fs.BoolVar(&src.PackageChart, "chart-package", false, "")
	fs.StringVar(&src.Archive, "image-archive", "", "")
	fs.StringVar(&src.Config, "config-digest", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return src, cli.Usage("użycie: %s", usage)
	}
	switch {
	case src.Ref != "" && src.Archive == "" && src.Config == "":
		src.Kind = environment.SourceRegistry
	case src.Ref == "" && src.Archive != "" && src.Config != "" && !src.PackageChart:
		src.Kind = environment.SourceArchive
	case src.Ref == "" && src.Archive == "" && src.Config == "" && !src.PackageChart:
	default:
		return src, cli.Usage("użycie: %s", usage)
	}
	return src, nil
}

func runCluster(ctx context.Context, w *workspace, args []string) error {
	const usage = "dft cluster <up [źródło obrazu]|down|evidence>"
	if len(args) == 0 || (args[0] != "up" && len(args) > 1) {
		return cli.Usage("użycie: %s", usage)
	}
	def, err := w.environmentOf()
	if err != nil {
		return err
	}
	switch args[0] {
	case "up":
		src, err := sourceArgs(args[1:])
		if err != nil {
			return err
		}
		if err := requireTools("k3d", "kubectl", "helm", "docker"); err != nil {
			return err
		}
		if err := checkClock(ctx); err != nil {
			return err
		}
		d, err := w.deployer(def)
		if err != nil {
			return err
		}
		return d.Up(ctx, src)
	case "down":
		if err := requireTools("k3d"); err != nil {
			return err
		}
		d, err := w.deployer(def)
		if err != nil {
			return err
		}
		if err := d.Down(ctx); err != nil {
			return err
		}
		// Klucz odzyskiwania usuniętego klastra niczego już nie otworzy.
		return os.RemoveAll(stateDir(def.Cluster.Name))
	case "evidence":
		return runEvidence(ctx, w, def)
	default:
		return cli.Usage("użycie: %s", usage)
	}
}

// deployer składa etap wdrożenia z adapterami k3d, Dockera, hosta, Helma
// i Kubernetesa.
func (w *workspace) deployer(def environment.Definition) (deploy.Deployer, error) {
	p, err := pins.Load(w.root)
	if err != nil {
		return deploy.Deployer{}, err
	}
	return deploy.Deployer{
		Root: w.root, Env: def, Pins: p, Service: "api", Image: imageName("api"), LogDir: w.reportDir(),
		Clusters: w.k3d(), Engine: w.docker(), Host: host.Host{}, Charts: w.charts(), Releases: w.helm(),
		Connect: func(t cluster.Target) (deploy.Cluster, error) {
			c, err := kube.Connect(t, w.runner)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		Builder: w.builder(), Admission: w.admission(), Versions: w.git(), ChartRegistry: registry.Client{},
		Download: func(ctx context.Context, url, sha string) (string, error) {
			return fetch.URL(ctx, w.root, url, sha)
		},
		// Digest spakowanego charta idzie do wyjść kroku: zadanie chart
		// opublikuje tylko ten sam artefakt (decyzja 61).
		Package: func(ctx context.Context, image string) (string, string, error) {
			dir, err := os.MkdirTemp("", "dft-chart-")
			if err != nil {
				return "", "", err
			}
			archive, digest, err := w.chartPublisher().Package(ctx, image, dir)
			if err != nil {
				return "", "", err
			}
			return archive, digest, githubOutput("chart-digest=" + digest + "\n")
		},
		SecretStores: w.secretStores(def),
		DNSTokens: func(t cluster.Target) (deploy.DNSToken, error) {
			return w.dnsTokens(def, t)
		},
	}, nil
}

// runEvidence zbiera dowody z klastra środowiska do katalogu raportów —
// po biegu bramek, także po porażce. Braki ostrzegają, nie zmieniają kodu
// wyjścia: krok dowodów nie może przykryć wyniku bramki.
func runEvidence(ctx context.Context, w *workspace, def environment.Definition) error {
	if err := def.RequireCluster(); err != nil {
		return err
	}
	if err := requireTools("kubectl", "docker"); err != nil {
		return err
	}
	collector := evidence.Collector{
		Nodes:      w.k3d(),
		Name:       def.Cluster.Name,
		Namespaces: []string{def.App.Namespace, def.Gateway.Namespace, "envoy-gateway-system", "cert-manager", "openbao", "kube-system"},
		Dir:        filepath.Join(w.reportDir(), "evidence-"+def.Name),
	}
	if c, err := w.connect(def); err != nil {
		cli.Warn("klaster %s: %v", def.Cluster.Name, err)
	} else {
		collector.Cluster = c
	}
	missing, err := collector.Collect(ctx)
	if err != nil {
		return err
	}
	for _, m := range missing {
		cli.Warn("dowody: %s", m)
	}
	cli.Step("dowody: %s", collector.Dir)
	return nil
}

func runCheckDrain(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check drain   (DRAIN_NODE=<węzeł> — tylko ten węzeł)"); err != nil {
		return err
	}
	def, err := w.environmentOf()
	if err != nil {
		return err
	}
	c, err := w.connect(def)
	if err != nil {
		return err
	}
	_, err = w.drainGate(ctx, def, c, os.Getenv("DRAIN_NODE"))
	return err
}

// drainGate składa i uruchamia bramkę drainu na klastrze środowiska;
// node — tylko ten węzeł (pusty — każdy z repliką). Raport trafia do
// katalogu raportów także przy wyniku negatywnym.
func (w *workspace) drainGate(ctx context.Context, def environment.Definition, c *kube.Cluster, node string) (report drain.Report, err error) {
	if err := def.Require(environment.OpDrain); err != nil {
		return drain.Report{}, err
	}
	if err := requireTools("kubectl", "go", "k3d"); err != nil {
		return drain.Report{}, err
	}
	tmp, err := os.MkdirTemp("", "dft-drain-")
	if err != nil {
		return drain.Report{}, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()

	// Sonda budowana ze źródeł tego commita i importowana do węzłów — jak
	// obraz API w dev; obraz bez bazy, powtarzalny z konstrukcji.
	probe, err := drain.ProbeImage{Go: w.gotool(), Importer: w.k3d(), Cluster: def.Cluster.Name}.Prepare(ctx, tmp)
	if err != nil {
		return drain.Report{}, err
	}
	deployment := def.App.Release + "-api"
	gate := &drain.Gate{
		Cluster: c,
		Cfg: drain.Config{
			Namespace:     def.App.Namespace,
			Deployment:    deployment,
			ServiceURL:    fmt.Sprintf("http://%s.%s.svc.cluster.local/search?q=drain", deployment, def.App.Namespace),
			ProbeImage:    probe,
			Node:          node,
			MinRequests:   contract.DrainMinRequests,
			DrainTimeout:  contract.DrainTimeout,
			RolloutWait:   120 * time.Second,
			SettleAfter:   5 * time.Second,
			LeaseDuration: 60 * time.Second,
			LeaseRenew:    15 * time.Second,
			RunID:         time.Now().UTC().Format("20060102t150405") + fmt.Sprintf("-%d", os.Getpid()),
			ProbeLogDir:   w.reportDir(),
		},
	}
	report, err = gate.Run(ctx)
	return report, w.withReport("drain-"+def.Name, report, err)
}

func runCheckTLS(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check tls   (DOCFIND_TLS_RENEW_PRODUCTION=1 — także produkcyjny Let's Encrypt)"); err != nil {
		return err
	}
	def, err := w.environmentOf()
	if err != nil {
		return err
	}
	c, err := w.connect(def)
	if err != nil {
		return err
	}
	_, err = w.ingressGate(ctx, def, c)
	return err
}

// runCheckRollout — rolling update aplikacji pod ruchem przez bramę: Z
// i zero nieudanych żądań (decyzja 60). Na środowisku z Argo CD restart
// cofnęłaby samonaprawa, więc tam odmowa.
func runCheckRollout(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check rollout"); err != nil {
		return err
	}
	def, err := w.environmentOf()
	if err != nil {
		return err
	}
	if def.GitOps() {
		return cli.Unmet("środowisko %s wdraża Argo CD z samonaprawą — restart Deploymentu zostałby cofnięty; rollout pod ruchem sprawdza się na ci i dev", def.Name)
	}
	c, err := w.connect(def)
	if err != nil {
		return err
	}
	checker := &ingress.Checker{Cluster: c, Edge: httpclient.Client{}, Cfg: ingress.Config{
		GatewayNamespace: def.Gateway.Namespace, Gateway: def.Gateway.Name, AppNamespace: def.App.Namespace,
		Route: def.App.Release + "-api", HTTPPort: def.Cluster.Ports.HTTP, HTTPSPort: def.Cluster.Ports.HTTPS,
	}}
	report, err := checker.Rollout(ctx, def.App.Release+"-api", 3*time.Minute)
	return w.withReport("rollout-"+def.Name, report, err)
}

// ingressGate składa i uruchamia bramkę wejścia (TLS i odnowienie
// certyfikatu pod ruchem) na klastrze środowiska.
func (w *workspace) ingressGate(ctx context.Context, def environment.Definition, c *kube.Cluster) (ingress.Report, error) {
	if err := requireTools("cmctl"); err != nil {
		return ingress.Report{}, err
	}
	renewProduction, err := envFlag("DOCFIND_TLS_RENEW_PRODUCTION")
	if err != nil {
		return ingress.Report{}, err
	}
	checker := &ingress.Checker{
		Cluster: c,
		Edge:    httpclient.Client{},
		Cfg: ingress.Config{
			GatewayNamespace: def.Gateway.Namespace,
			Gateway:          def.Gateway.Name,
			AppNamespace:     def.App.Namespace,
			Route:            def.App.Release + "-api",
			HTTPPort:         def.Cluster.Ports.HTTP,
			HTTPSPort:        def.Cluster.Ports.HTTPS,
			RenewProduction:  renewProduction,
			AllowRenewal:     def.Allows(environment.OpTLSRenew),
			MinRequests:      contract.TLSMinRequests,
		},
	}
	report, err := checker.Run(ctx)
	return report, w.withReport("tls-"+def.Name, report, err)
}

// runCheckIdentity sprawdza, że na klastrze działa obraz, który miał trafić
// — ten sam test, którym kończy się `dft cluster up`, osobno.
func runCheckIdentity(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 1, "dft check identity [obraz:tag]"); err != nil {
		return err
	}
	def, err := w.environmentOf()
	if err != nil {
		return err
	}
	c, err := w.connect(def)
	if err != nil {
		return err
	}
	image := ""
	if len(args) == 1 {
		image = args[0]
	}
	_, err = w.identityGate(ctx, def, c, image)
	return err
}

// identityGate sprawdza tożsamość na podach i zapisuje raport.
func (w *workspace) identityGate(ctx context.Context, def environment.Definition, c *kube.Cluster, image string) ([]identity.Pod, error) {
	results, err := w.verifyIdentity(ctx, def, c, image)
	return results, w.withReport("identity-"+def.Name, results, err)
}

// verifyIdentity sprawdza tożsamość na podach aplikacji: obraz (podany albo
// ze specyfikacji Deploymentu), commit z obrazu i wersję wdrożenia
// z DOCFIND_VERSION Deploymentu.
func (w *workspace) verifyIdentity(ctx context.Context, def environment.Definition, c *kube.Cluster, image string) ([]identity.Pod, error) {
	deployment := def.App.Release + "-api"
	if image == "" {
		var err error
		if image, err = c.DeploymentImage(ctx, def.App.Namespace, deployment, "api"); err != nil {
			return nil, err
		}
	}
	version, found, err := c.DeploymentEnv(ctx, def.App.Namespace, deployment, "api", "DOCFIND_VERSION")
	if err != nil {
		return nil, err
	}
	switch {
	case !found:
		cli.Warn("Deployment %s bez DOCFIND_VERSION (wdrożenie sprzed decyzji 40) — wersji nie sprawdzam, commit tak", deployment)
	case version == "":
		cli.Warn("Deployment %s z pustym DOCFIND_VERSION (chart bez api.version) — /version poda unknown; wersji nie sprawdzam, commit tak", deployment)
	}
	want, err := identity.Expect(ctx, image, version, registry.Client{}, w.git())
	if err != nil {
		return nil, err
	}
	return identity.Verify(ctx, c, def.App.Namespace, deployment, "api", want)
}

// runCheckClusterNegatives uruchamia warianty negatywne na żywym klastrze
// środowiska (go test -tags cluster): to, co API server ma odrzucić.
func runCheckClusterNegatives(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check cluster-negatives"); err != nil {
		return err
	}
	if err := requireTools("go"); err != nil {
		return err
	}
	err := w.gotool().TestTagged(ctx, "cluster", "Rejected$")
	if cli.ExitCode(err) == cli.ExitUnmet {
		return cli.Unmet("warianty negatywne na klastrze: API server przyjął coś, co miał odrzucić — wyżej wynik go test")
	}
	return err
}

func runDNSToken(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft dns-token   (token z terminala bez echa albo z potoku; DOCFIND_ACME_ZONE — sprawdza też strefę)"); err != nil {
		return err
	}
	def, err := w.environmentOf()
	if err != nil {
		return err
	}
	if err := def.Require(environment.OpDNSToken); err != nil {
		return err
	}
	token, err := readSecret("Token API Cloudflare (nie będzie widoczny): ")
	if err != nil {
		return err
	}
	if err := dnstoken.Verify(ctx, cloudflare.API{}, token, def.Gateway.ACMEZone); err != nil {
		return err
	}
	sync, err := w.dnsTokens(def, def.Target(w.root))
	if err != nil {
		return err
	}
	return sync.Store(ctx, sensitive.New(token))
}

// dnsTokens składa token Cloudflare w OpenBao i jego Secret od ESO
// (stages/dnstoken) na klastrze t.
func (w *workspace) dnsTokens(def environment.Definition, t cluster.Target) (dnstoken.Sync, error) {
	c, err := kube.Connect(t, w.runner)
	if err != nil {
		return dnstoken.Sync{}, err
	}
	s, err := w.secretStore(def, t)
	if err != nil {
		return dnstoken.Sync{}, err
	}
	return dnstoken.Sync{Vault: s, Cluster: c}, nil
}

// readSecret czyta sekret z terminala bez echa albo z potoku (menedżer
// haseł) — nigdy z argumentów ani zmiennych środowiskowych.
func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return dnstoken.ReadToken(os.Stdin)
	}
	fmt.Fprint(cli.Err, prompt)
	raw, err := term.ReadPassword(fd)
	fmt.Fprintln(cli.Err)
	if err != nil {
		return "", err
	}
	return dnstoken.ReadToken(bytes.NewReader(raw))
}

// checkClock — zegar maszyny względem nagłówka Date API GitHuba przed
// operacjami, które porównują daty z cudzymi: certyfikaty, tokeny z czasem
// życia, daty wyjątków (decyzja 66). Rozjazd ponad próg to awaria
// środowiska (kod 2); brak odpowiedzi — ostrzeżenie, bo lokalny klaster
// działa bez sieci.
func checkClock(ctx context.Context) error {
	skew, err := host.Host{}.ClockSkew(ctx, host.GitHubAPI)
	if err != nil {
		cli.Warn("nie sprawdzono zegara: %v", err)
		return nil
	}
	if p := clock.Problem(skew); p != "" {
		return errors.New(p)
	}
	cli.Step("zegar maszyny zgodny z API GitHuba (różnica %v)", skew.Round(100*time.Millisecond))
	return nil
}
