// Package vulns to bramka podatności obrazu (decyzja 37): skan archiwum
// obrazu, ocena polityką z core/vuln — na PR-ze względem obrazu z main
// — i wynik nocnego skanu przeniesiony do issue zamiast czerwieni na main.
package vulns

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/contract"
	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/ociarchive"
)

// Scanner — skaner archiwów obrazów (adapters/osv).
type Scanner interface {
	Scan(ctx context.Context, archive string, download bool) ([]vuln.Finding, error)
	Databases() ([]vuln.Database, error)
}

// Registry — obrazy z rejestru (adapters/registry). Brak obrazu —
// artifact.ErrNotFound.
type Registry interface {
	Save(ctx context.Context, ref, path string) error
}

// History — historia gita (adapters/git): commit i jego przodkowie po
// pierwszym rodzicu, od najnowszego. Nieznany commit —
// artifact.ErrUnknownCommit.
type History interface {
	FirstParents(ctx context.Context, commit string, limit int) ([]string, error)
}

// Images — obrazy w lokalnym demonie (adapters/docker).
type Images interface {
	Save(ctx context.Context, ref, path string) error
}

// Target — skanowany obraz: archiwum z zadania build (z digestem
// konfiguracji, który to zadanie sprawdziło), obraz z rejestru albo obraz
// zbudowany lokalnie.
type Target struct {
	Archive string
	Config  string
	Image   string
	Local   string
}

// Scan — jeden skan.
type Scan struct {
	Scanner  Scanner
	Registry Registry
	Images   Images
	// Exceptions — wyjątki z zakresu obrazu.
	Exceptions []vuln.Exception
	Today      time.Time
	// Repository — obraz aplikacji; obraz bazowy to Repository:<sha12>.
	Repository string
	// History — przodkowie commita bazowego, gdy jego obrazu nie ma
	// w rejestrze.
	History History
}

// Run skanuje obraz i ocenia podatności. baselineCommit — commit na main,
// którego obraz jest punktem odniesienia (blokuje tylko to, czego tam nie
// ma); reportOnly — raport bez blokowania (main, nocą). Raport wraca także
// przy wyniku negatywnym.
func (s Scan) Run(ctx context.Context, target Target, baselineCommit string, reportOnly bool) (vuln.Report, error) {
	rep := vuln.Report{Subject: "obrazu api", Scanner: "osv-scanner", ScannedAt: s.Today, ReportOnly: reportOnly}
	tmp, err := os.MkdirTemp("", "dft-vulns-")
	if err != nil {
		return rep, err
	}
	defer os.RemoveAll(tmp)

	archive := filepath.Join(tmp, "obraz.tar")
	switch {
	case target.Archive != "":
		rep.Target, archive = target.Archive, target.Archive
		if target.Config != "" {
			img, err := ociarchive.Open(target.Archive)
			if err != nil {
				return rep, err
			}
			config, err := img.Config()
			if err != nil {
				return rep, err
			}
			if config != target.Config {
				return rep, cli.Unmet("archiwum %s: obraz z konfiguracją %s, a zadanie build sprawdziło %s", target.Archive, config, target.Config)
			}
		}
	case target.Image != "":
		rep.Target = target.Image
		if err := s.Registry.Save(ctx, target.Image, archive); err != nil {
			return rep, err
		}
	default:
		rep.Target = target.Local
		if err := s.Images.Save(ctx, target.Local, archive); err != nil {
			return rep, err
		}
	}

	cli.Step("osv-scanner: %s (bazy pobierane raz na bieg)", rep.Target)
	findings, err := s.Scanner.Scan(ctx, archive, true)
	if err != nil {
		return rep, err
	}
	if rep.Databases, err = s.Scanner.Databases(); err != nil {
		return rep, err
	}
	for _, db := range rep.Databases {
		cli.Step("baza %s: %d wpisów, najnowszy %s, sha256 %.12s", db.Ecosystem, db.Entries, db.Newest, db.SHA256)
	}

	policy := vuln.Policy{Exceptions: s.Exceptions, Today: s.Today}
	if baselineCommit != "" {
		if len(baselineCommit) < 12 {
			return rep, cli.Usage("--baseline-commit: pełny SHA commita")
		}
		base := filepath.Join(tmp, "baseline.tar")
		switch ref, err := s.baseline(ctx, baselineCommit, base); {
		case errors.Is(err, artifact.ErrNotFound):
			cli.Warn("obrazu bazowego %s ani obrazu żadnego z %d przodków nie ma w rejestrze — bez porównania blokuje każda naprawialna HIGH i CRITICAL", s.image(baselineCommit), contract.VulnsBaselineAncestors)
		case err != nil:
			return rep, err
		default:
			if wanted := s.image(baselineCommit); ref != wanted {
				// Obraz main pojawia się w rejestrze ok. 10 min po scaleniu:
				// PR zaktualizowany na świeży main w tym oknie nie ma czego
				// porównać. Bez obrazu przodka blokowałaby go każda
				// podatność, także sprzed jego zmiany.
				rep.BaselineFor = wanted
				cli.Warn("obrazu bazowego %s nie ma jeszcze w rejestrze — punkt odniesienia: obraz przodka %s; podatność wprowadzona między nimi liczy się jako nowa", wanted, ref)
			}
			// Bez pobierania baz: obraz bazowy oceniony tą samą migawką.
			baseline, err := s.Scanner.Scan(ctx, base, false)
			if err != nil {
				return rep, err
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
		if f.Blocking || f.Severity == vuln.SeverityCritical || f.Severity == vuln.SeverityHigh || f.Severity == vuln.SeverityUnknown {
			mark := "  "
			if f.Blocking {
				mark = "✗ "
			}
			fmt.Fprintf(cli.Out, "%s%s %s %s (%s %s) poprawka: %s %s\n", mark, f.Severity, f.ID, f.Package, f.Version, f.Ecosystem, strings.Join(f.Fixed, ", "), f.Note)
		}
	}
	for _, u := range rep.Unused {
		cli.Warn("wyjątek %s nie pasuje do żadnej podatności — do usunięcia z %s", u, vuln.ExceptionsFile)
	}
	cli.Step("podatności: %d, blokujących: %d", len(rep.Findings), rep.Blocking)
	if rep.Blocking > 0 && !reportOnly {
		return rep, cli.Unmet("%d naprawialnych podatności HIGH/CRITICAL bez wyjątku — napraw albo dopisz wyjątek z datą do %s", rep.Blocking, vuln.ExceptionsFile)
	}
	return rep, nil
}

// image — obraz commita w rejestrze.
func (s Scan) image(commit string) string {
	return s.Repository + ":" + artifact.ShortCommit(commit)
}

// baseline zapisuje w path obraz commita bazowego, a gdy go nie ma —
// obraz najbliższego przodka po pierwszym rodzicu, najwyżej
// contract.VulnsBaselineAncestors wstecz. Zwraca referencję zapisanego
// obrazu; żadnego — artifact.ErrNotFound. Historię gita czyta dopiero,
// gdy obrazu commita bazowego brakuje.
func (s Scan) baseline(ctx context.Context, commit, path string) (string, error) {
	ref := s.image(commit)
	err := s.Registry.Save(ctx, ref, path)
	if !errors.Is(err, artifact.ErrNotFound) || s.History == nil {
		return ref, err
	}
	commits, err := s.History.FirstParents(ctx, commit, contract.VulnsBaselineAncestors+1)
	if err != nil {
		return "", fmt.Errorf("przodkowie commita bazowego (w CI checkout z fetch-depth: 0): %w", err)
	}
	for _, c := range commits[min(1, len(commits)):] {
		ref := s.image(c)
		if err := s.Registry.Save(ctx, ref, path); !errors.Is(err, artifact.ErrNotFound) {
			return ref, err
		}
	}
	return "", fmt.Errorf("%s: %w", ref, artifact.ErrNotFound)
}

// Issues — issue repozytorium (adapters/github).
type Issues interface {
	FindIssue(ctx context.Context, title string) (int, error)
	CreateIssue(ctx context.Context, title, body string) error
	EditIssue(ctx context.Context, number int, body string) error
	CloseIssue(ctx context.Context, number int, comment string) error
}

// SyncIssue przenosi wynik nocnych skanów — obrazu i kodu narzędzi — do
// jednego issue zamiast czerwieni na main (decyzja 37): blokujące
// podatności w którymkolwiek — issue otwarte albo zaktualizowane; brak
// blokujących we wszystkich — otwarte issue zamknięte z komentarzem.
//
// missing — skany, które nie dały raportu (zadanie przed nimi nie przeszło).
// Brak raportu to brak wiedzy, a nie brak podatności: issue nie zostaje
// zamknięte, a wynik jest niespełniony, żeby bieg nocny nie wyglądał na
// kompletny.
func SyncIssue(ctx context.Context, issues Issues, reports []vuln.Report, missing []string) error {
	if len(reports) == 0 {
		return cli.Unmet("żaden nocny skan nie dał raportu (%s) — issue bez zmian", strings.Join(missing, ", "))
	}
	blocking := 0
	for _, r := range reports {
		blocking += r.Blocking
	}
	number, err := issues.FindIssue(ctx, vuln.IssueTitle)
	if err != nil {
		return err
	}
	switch body := vuln.IssueBody(reports, missing); {
	case blocking > 0 && number == 0:
		if err := issues.CreateIssue(ctx, vuln.IssueTitle, body); err != nil {
			return err
		}
		cli.Step("issue „%s” otwarte: %d blokujących", vuln.IssueTitle, blocking)
	case blocking > 0:
		if err := issues.EditIssue(ctx, number, body); err != nil {
			return err
		}
		cli.Step("issue #%d zaktualizowane: %d blokujących", number, blocking)
	case len(missing) > 0:
		cli.Warn("brak blokujących w %d raportach, ale skany bez raportu nie pozwalają zamknąć issue", len(reports))
	case number != 0:
		if err := issues.CloseIssue(ctx, number, fmt.Sprintf("Skan z %s: brak blokujących podatności.", reports[0].ScannedAt.Format("2006-01-02"))); err != nil {
			return err
		}
		cli.Step("issue #%d zamknięte — brak blokujących podatności", number)
	default:
		cli.Step("brak blokujących podatności, brak otwartego issue")
	}
	if len(missing) > 0 {
		return cli.Unmet("bez raportu: %s — skan się nie wykonał", strings.Join(missing, ", "))
	}
	return nil
}
