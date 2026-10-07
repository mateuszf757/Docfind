package main

import (
	"context"
	"encoding/json"
	"errors"
	stdflag "flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/docker"
	"github.com/mateuszf757/Docfind/tools/internal/registry"
	"github.com/mateuszf757/Docfind/tools/internal/vulns"
)

// runCheckVulns skanuje obraz API i ocenia podatności polityką z decyzji 37.
//
//	bez flag                  obraz zbudowany z tego drzewa (docker save)
//	--image-archive P [--config-digest D]   archiwum z zadania build
//	--image R                 obraz z rejestru (tag albo digest)
//	--baseline-commit SHA     obraz z main dla tego commita jako punkt
//	                          odniesienia: blokuje tylko to, czego tam nie ma
//	--report-only             raport bez blokowania (main, nocą)
func runCheckVulns(ctx context.Context, e *environment, args []string) (err error) {
	const usage = "dft check vulns [--image-archive <plik> [--config-digest sha256:…] | --image <obraz>] [--baseline-commit <sha>] [--report-only]"
	fs := stdflag.NewFlagSet("check vulns", stdflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	archive := fs.String("image-archive", "", "")
	configDigest := fs.String("config-digest", "", "")
	image := fs.String("image", "", "")
	baselineCommit := fs.String("baseline-commit", "", "")
	reportOnly := fs.Bool("report-only", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || (*archive != "" && *image != "") || (*configDigest != "" && *archive == "") {
		return cli.Usage("użycie: %s", usage)
	}
	if err := requireTools("osv-scanner"); err != nil {
		return err
	}
	today := time.Now().UTC()
	exceptions, err := vulns.LoadExceptions(filepath.Join(e.root, vulns.ExceptionsFile), today)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "dft-vulns-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()

	rep := vulns.Report{Scanner: "osv-scanner", ScannedAt: today, ReportOnly: *reportOnly}
	target := filepath.Join(tmp, "obraz.tar")
	switch {
	case *archive != "":
		rep.Target = *archive
		target = *archive
		if *configDigest != "" {
			img, err := tarball.ImageFromPath(*archive, nil)
			if err != nil {
				return fmt.Errorf("archiwum %s: %w", *archive, err)
			}
			if c, err := img.ConfigName(); err != nil {
				return err
			} else if c.String() != *configDigest {
				return cli.Unmet("archiwum %s: obraz z konfiguracją %s, a zadanie build sprawdziło %s", *archive, c, *configDigest)
			}
		}
	case *image != "":
		rep.Target = *image
		if err := registry.SaveImage(*image, target, registry.Options(ctx)...); err != nil {
			return err
		}
	default:
		if err := requireTools("docker"); err != nil {
			return err
		}
		ref, err := builtImage(ctx, e, "api")
		if err != nil {
			return err
		}
		rep.Target = ref
		if err := (docker.Client{Runner: e.runner}).Save(ctx, ref, target); err != nil {
			return err
		}
	}

	scanner := vulns.Scanner{Runner: e.runner, DBDir: filepath.Join(e.root, ".cache", "osv-scanner")}
	cli.Step("osv-scanner: %s (bazy pobierane raz na bieg)", rep.Target)
	findings, err := scanner.ScanArchive(ctx, target, filepath.Join(tmp, "osv.json"), true)
	if err != nil {
		return err
	}
	if rep.Databases, err = vulns.Databases(scanner.DBDir); err != nil {
		return err
	}
	for _, db := range rep.Databases {
		cli.Step("baza %s: %d wpisów, najnowszy %s, sha256 %.12s", db.Ecosystem, db.Entries, db.Newest, db.SHA256)
	}

	policy := vulns.Policy{Exceptions: vulns.InScope(exceptions, vulns.ScopeImage), Today: today}
	if *baselineCommit != "" {
		if len(*baselineCommit) < 12 {
			return cli.Usage("--baseline-commit: pełny SHA commita")
		}
		ref := imageName("api") + ":" + (*baselineCommit)[:12]
		base := filepath.Join(tmp, "baseline.tar")
		switch err := registry.SaveImage(ref, base, registry.Options(ctx)...); {
		case errors.Is(err, registry.ErrNotFound):
			cli.Warn("obrazu bazowego %s nie ma w rejestrze — bez porównania blokuje każda naprawialna HIGH i CRITICAL", ref)
		case err != nil:
			return err
		default:
			// Bez pobierania baz: obraz bazowy oceniony tą samą migawką.
			baseline, err := scanner.ScanArchive(ctx, base, filepath.Join(tmp, "osv-baseline.json"), false)
			if err != nil {
				return err
			}
			rep.Baseline = ref
			policy.Baseline = baseline
			cli.Step("punkt odniesienia %s: %d podatności", ref, len(baseline))
		}
	}

	rep.Findings, rep.Unused = policy.Evaluate(findings)
	for _, f := range rep.Findings {
		if f.Blocking {
			rep.Blocking++
		}
		if f.Blocking || f.Severity == vulns.SeverityCritical || f.Severity == vulns.SeverityHigh || f.Severity == vulns.SeverityUnknown {
			mark := "  "
			if f.Blocking {
				mark = "✗ "
			}
			fmt.Fprintf(cli.Out, "%s%s %s %s (%s %s) poprawka: %s %s\n", mark, f.Severity, f.ID, f.Package, f.Version, f.Ecosystem, strings.Join(f.Fixed, ", "), f.Note)
		}
	}
	for _, u := range rep.Unused {
		cli.Warn("wyjątek %s nie pasuje do żadnej podatności — do usunięcia z %s", u, vulns.ExceptionsFile)
	}
	if reportErr := writeReport(e, "vulns-api", rep); reportErr != nil {
		return reportErr
	}
	if err := appendSummary(rep.Summary()); err != nil {
		return err
	}
	cli.Step("podatności: %d, blokujących: %d", len(rep.Findings), rep.Blocking)
	if rep.Blocking > 0 && !*reportOnly {
		return cli.Unmet("%d naprawialnych podatności HIGH/CRITICAL bez wyjątku — napraw albo dopisz wyjątek z datą do %s", rep.Blocking, vulns.ExceptionsFile)
	}
	return nil
}

// runVulnsIssue — wynik nocnego skanu do issue (vulns.SyncIssue).
func runVulnsIssue(ctx context.Context, e *environment, args []string) error {
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
	var rep vulns.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		return fmt.Errorf("raport %s: %w", args[0], err)
	}
	return vulns.SyncIssue(ctx, e.runner, rep)
}
