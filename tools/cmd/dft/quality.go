package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/git"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/kube"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/kubeconform"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/lint"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/osv"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/registry"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/uv"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/stages/charts"
	"github.com/mateuszf757/Docfind/tools/internal/stages/gocode"
	"github.com/mateuszf757/Docfind/tools/internal/stages/versions"
	"github.com/mateuszf757/Docfind/tools/internal/stages/vulns"
)

const checkUsage = "dft check <versions|reproducible-tools|workflows|secrets|python|charts> | <runtime|base|reproducible|negatives> [usługa] | <drain|tls|identity|cluster-negatives|openbao|rotation|selfheal> | published <obraz@digest> <digest konfiguracji> | attestation <obraz@digest> | go [--vulns-baseline <commit>] [--vulns-report-only] | vulns [źródło obrazu]"

// checks — bramki `dft check <nazwa>`.
var checks = map[string]func(ctx context.Context, w *workspace, args []string) error{
	"versions":           runCheckVersions,
	"go":                 runCheckGo,
	"reproducible-tools": runCheckReproducibleTools,
	"workflows":          runCheckWorkflows,
	"secrets":            runCheckSecrets,
	"python":             runCheckPython,
	"charts":             runCheckCharts,
	"runtime":            runCheckRuntime,
	"base":               runCheckBase,
	"reproducible":       runCheckReproducible,
	"negatives":          runCheckNegatives,
	"published":          runCheckPublished,
	"attestation":        runCheckAttestation,
	"vulns":              runCheckVulns,
	"drain":              runCheckDrain,
	"tls":                runCheckTLS,
	"identity":           runCheckIdentity,
	"cluster-negatives":  runCheckClusterNegatives,
	"openbao":            runCheckOpenBao,
	"rotation":           runCheckRotation,
	"selfheal":           runCheckSelfHeal,
}

func runCheck(ctx context.Context, w *workspace, args []string) error {
	if len(args) == 0 {
		return cli.Usage("użycie: %s", checkUsage)
	}
	check, ok := checks[args[0]]
	if !ok {
		return cli.Usage("użycie: %s", checkUsage)
	}
	return check(ctx, w, args[1:])
}

// runTest to zadanie test z CI: wersje, workflowy i skrypty, sekrety
// w historii gita, Python aplikacji, charty, kod narzędzi. Kończy się na pierwszej grupie, która nie
// przeszła — każda zakłada, że poprzednie przeszły (dawniej ci/run-tests.sh).
// Flagi govulncheck — goVulnFlags.
func runTest(ctx context.Context, w *workspace, args []string) error {
	mode, err := parseGoVulnFlags("dft test", args)
	if err != nil {
		return err
	}
	// Wymagania na starcie i zgłaszane razem: brak narzędzia w połowie biegu
	// wyglądałby jak błąd w kodzie, a nie w środowisku.
	if err := requireTools("uv", "shellcheck", "actionlint", "zizmor", "gitleaks", "helm", "kubeconform", "kubectl", "go"); err != nil {
		return err
	}
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"spójność wersji", func() error { return checkVersions(ctx, w) }},
		{"workflowy i skrypty", func() error { return w.workflows().Check(ctx) }},
		{"sekrety w historii gita", func() error { return checkSecrets(ctx, w) }},
		{"Python aplikacji", func() error { return checkPython(ctx, w) }},
		{"charty", func() error { return checkCharts(ctx, w) }},
		{"kod Go", func() error { return checkGo(ctx, w, mode) }},
	} {
		cli.Step("— %s —", step.name)
		if err := step.run(); err != nil {
			return err
		}
	}
	cli.Step("OK")
	return nil
}

func (w *workspace) workflows() lint.Workflows {
	return lint.Workflows{Runner: w.runner, Root: w.root, Out: os.Stdout, Err: os.Stderr}
}

// checkSecrets — gitleaks na całej historii (warunek Etapu 4). W raporcie
// i na wyjściu nie ma wartości sekretów: miejsce, reguła i odcisk do
// ewentualnego wyjątku.
func checkSecrets(ctx context.Context, w *workspace) error {
	shallow, err := w.git().Shallow(ctx)
	if err != nil {
		return err
	}
	if shallow {
		return errors.New("płytki klon — skan objąłby tylko część historii: `git fetch --unshallow` (w CI actions/checkout z fetch-depth: 0)")
	}
	leaks, err := lint.Secrets{
		Runner: w.runner, Root: w.root, Report: filepath.Join(w.reportDir(), "gitleaks.json"),
		Out: os.Stdout, Err: os.Stderr,
	}.Check(ctx)
	for _, l := range leaks {
		fmt.Fprintf(cli.Err, "SEKRET: %s w %s:%d (commit %.12s) — odcisk: %s\n", l.RuleID, l.File, l.StartLine, l.Commit, l.Fingerprint)
	}
	return err
}

func checkVersions(ctx context.Context, w *workspace) error {
	return versions.Check(ctx, w.root, versions.Tools{Kubectl: kube.Kubectl{Runner: w.runner}, UV: uv.UV{Runner: w.runner}}, runtime.Version(), clientGoVersion())
}

func checkPython(ctx context.Context, w *workspace) error {
	return w.python().Check(ctx, "../../deploy/config/app.schema.json")
}

func checkCharts(ctx context.Context, w *workspace) (err error) {
	p, err := pins.Load(w.root)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "dft-charts-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
	return charts.Checker{
		Root: w.root, Pins: p, Helm: w.helm(), Charts: w.charts(), AppConfig: w.python(),
		Validator: kubeconform.Kubeconform{
			Runner: w.runner, Out: os.Stdout, Err: os.Stderr,
			KubernetesVersion: p.Get(pins.KubernetesVersion), SchemaCommit: p.Get(pins.KubeconformSchemaCommit),
			CacheDir: filepath.Join(w.root, ".cache", "kubeconform"),
		},
	}.Run(ctx, tmp)
}

// goVulnMode — tryb govulncheck w `dft test` i `dft check go`, ten sam co
// skanu obrazu (decyzja 37):
//
//	bez flag                  blokuje każda osiągalna podatność z poprawką
//	--vulns-baseline REV      kod tools/ z REV jako punkt odniesienia:
//	                          blokuje tylko to, czego tam nie ma (PR: commit
//	                          bazowy; lokalnie np. origin/main)
//	--vulns-report-only       raport bez blokowania (main, nocą)
type goVulnMode struct {
	baseline   string
	reportOnly bool
}

func parseGoVulnFlags(command string, args []string) (goVulnMode, error) {
	usage := command + " [--vulns-baseline <commit>] [--vulns-report-only]"
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var mode goVulnMode
	fs.StringVar(&mode.baseline, "vulns-baseline", "", "")
	fs.BoolVar(&mode.reportOnly, "vulns-report-only", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return mode, cli.Usage("użycie: %s", usage)
	}
	return mode, nil
}

// goChecker — bramki kodu narzędzi z wyjątkami podatności z zakresu go.
func goChecker(w *workspace, mode goVulnMode) (gocode.Checker, error) {
	today := time.Now().UTC()
	exceptions, err := vuln.LoadExceptions(filepath.Join(w.root, vuln.ExceptionsFile), today)
	if err != nil {
		return gocode.Checker{}, err
	}
	moduleDir := filepath.Join(w.root, "tools")
	return gocode.Checker{
		ModuleDir: moduleDir,
		Go:        w.gotool(),
		// GitHub Actions ustawia CI=true. Tam brak kompilatora C dla -race
		// to awaria, a nie po cichu pominięty detektor wyścigów.
		RequireRace:    os.Getenv("CI") == "true",
		Exceptions:     vuln.InScope(exceptions, vuln.ScopeGo),
		Today:          today,
		VulnBaseline:   mode.baseline,
		Sources:        git.Repo{Dir: moduleDir, Runner: w.runner},
		VulnReportOnly: mode.reportOnly,
		Publish: func(rep vuln.Report) error {
			return errors.Join(w.writeReport(reportVulnsGo, rep), appendSummary(rep.Summary()))
		},
	}, nil
}

func checkGo(ctx context.Context, w *workspace, mode goVulnMode) error {
	c, err := goChecker(w, mode)
	if err != nil {
		return err
	}
	return c.All(ctx)
}

func runCheckVersions(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check versions"); err != nil {
		return err
	}
	if err := requireTools("kubectl", "uv"); err != nil {
		return err
	}
	return checkVersions(ctx, w)
}

func runCheckGo(ctx context.Context, w *workspace, args []string) error {
	mode, err := parseGoVulnFlags("dft check go", args)
	if err != nil {
		return err
	}
	if err := requireTools("go"); err != nil {
		return err
	}
	return checkGo(ctx, w, mode)
}

func runCheckWorkflows(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check workflows"); err != nil {
		return err
	}
	if err := requireTools("shellcheck", "actionlint", "zizmor"); err != nil {
		return err
	}
	return w.workflows().Check(ctx)
}

func runCheckSecrets(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check secrets"); err != nil {
		return err
	}
	if err := requireTools("gitleaks"); err != nil {
		return err
	}
	return checkSecrets(ctx, w)
}

func runCheckPython(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check python"); err != nil {
		return err
	}
	if err := requireTools("uv"); err != nil {
		return err
	}
	return checkPython(ctx, w)
}

func runCheckCharts(ctx context.Context, w *workspace, args []string) error {
	if err := expectArgs(args, 0, 0, "dft check charts"); err != nil {
		return err
	}
	if err := requireTools("helm", "kubeconform", "uv"); err != nil {
		return err
	}
	return checkCharts(ctx, w)
}

func runCheckReproducibleTools(ctx context.Context, w *workspace, args []string) (err error) {
	if err := expectArgs(args, 0, 0, "dft check reproducible-tools"); err != nil {
		return err
	}
	if err := requireTools("go"); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "dft-reproducible-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
	c, err := goChecker(w, goVulnMode{})
	if err != nil {
		return err
	}
	binaries, err := c.Reproducible(ctx, tmp)
	if len(binaries) > 0 {
		var table strings.Builder
		table.WriteString("### Powtarzalność narzędzi Go\n\n| Program | SHA-256 |\n|---|---|\n")
		for _, b := range binaries {
			cli.Step("%s  %s", b.SHA256, b.Name)
			fmt.Fprintf(&table, "| %s | `%s` |\n", b.Name, b.SHA256)
		}
		if summaryErr := appendSummary(table.String()); summaryErr != nil {
			return errors.Join(err, summaryErr)
		}
	}
	if err == nil {
		cli.Step("binarki powtarzalne: build z pamięcią podręczną i od zera dały te same bajty")
	}
	return err
}

// Raporty podatności w .cache/reports — nocą oba trafiają do jednego issue
// (runVulnsIssue).
const (
	reportVulnsImage = "vulns-api"
	reportVulnsGo    = "vulns-go"
)

// runCheckVulns skanuje obraz API i ocenia podatności polityką z decyzji 37.
//
//	bez flag                  obraz zbudowany z tego drzewa (docker save)
//	--image-archive P [--config-digest D]   archiwum z zadania build
//	--image R                 obraz z rejestru (tag albo digest)
//	--baseline-commit SHA     obraz z main dla tego commita jako punkt
//	                          odniesienia: blokuje tylko to, czego tam nie ma
//	--report-only             raport bez blokowania (main, nocą)
func runCheckVulns(ctx context.Context, w *workspace, args []string) error {
	const usage = "dft check vulns [--image-archive <plik> [--config-digest sha256:…] | --image <obraz>] [--baseline-commit <sha>] [--report-only]"
	fs := flag.NewFlagSet("check vulns", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var target vulns.Target
	fs.StringVar(&target.Archive, "image-archive", "", "")
	fs.StringVar(&target.Config, "config-digest", "", "")
	fs.StringVar(&target.Image, "image", "", "")
	baselineCommit := fs.String("baseline-commit", "", "")
	reportOnly := fs.Bool("report-only", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || (target.Archive != "" && target.Image != "") || (target.Config != "" && target.Archive == "") {
		return cli.Usage("użycie: %s", usage)
	}
	if err := requireTools("osv-scanner"); err != nil {
		return err
	}
	today := time.Now().UTC()
	exceptions, err := vuln.LoadExceptions(filepath.Join(w.root, vuln.ExceptionsFile), today)
	if err != nil {
		return err
	}
	if target.Archive == "" && target.Image == "" {
		if err := requireTools("docker"); err != nil {
			return err
		}
		if target.Local, err = builtImage(ctx, w, "api"); err != nil {
			return err
		}
	}
	scan := vulns.Scan{
		Scanner:    osv.Scanner{Runner: w.runner, DBDir: filepath.Join(w.root, ".cache", "osv-scanner")},
		Registry:   registry.Client{},
		Images:     w.docker(),
		Exceptions: vuln.InScope(exceptions, vuln.ScopeImage),
		Today:      today,
		Repository: imageName("api"),
	}
	rep, err := scan.Run(ctx, target, *baselineCommit, *reportOnly)
	// Raport i podsumowanie także przy wyniku negatywnym — awaria skanu
	// raportu nie ma.
	if err != nil && cli.ExitCode(err) != cli.ExitUnmet {
		return err
	}
	if reportErr := w.writeReport(reportVulnsImage, rep); reportErr != nil {
		return errors.Join(err, reportErr)
	}
	return errors.Join(err, appendSummary(rep.Summary()))
}
