// Package vulns to bramka podatności obrazu: OSV-Scanner na archiwum obrazu
// (pakiety Alpine i Pythona z uv.lock, które w nim są), polityka progów
// i wyjątki z datą wygaśnięcia (decyzja 37).
//
// Baza podatności zmienia się codziennie, więc ten sam commit może jutro
// być czerwony. Dlatego:
//   - blokuje tylko to, co da się naprawić (jest wersja z poprawką),
//     o wadze HIGH albo CRITICAL — albo nieznanej, bo nieznana to nie
//     bezpieczna;
//   - na PR-ze blokuje tylko to, czego nie ma w obrazie bazowym (main):
//     podatność opublikowana wczoraj w pakiecie, którego PR nie dotyka, nie
//     jest winą PR-a, a jej naprawa to osobna zmiana;
//   - świadomie przyjęte podatności leżą w ci/vuln-exceptions.yaml
//     z uzasadnieniem i datą — wyjątek po dacie znów blokuje;
//   - raport zapisuje, z jakiej bazy powstał: suma i najnowszy wpis każdej
//     bazy ekosystemu, pobranej raz na bieg i użytej do obu skanów.
package vulns

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// ExceptionsFile — wyjątki względem korzenia repozytorium.
const ExceptionsFile = "ci/vuln-exceptions.yaml"

// MaxExceptionDays — najdalsza data wygaśnięcia wyjątku od dziś. Wyjątek
// „na zawsze" to podatność, o której przestaje się pamiętać.
const MaxExceptionDays = 90

// Exception — świadomie przyjęta podatność w jednym pakiecie.
type Exception struct {
	// ID — CVE, GHSA, GO-… albo identyfikator OSV (np. ALPINE-CVE-…);
	// pasuje do identyfikatora i aliasów podatności.
	ID      string `json:"id"`
	Package string `json:"package"`
	// Expires — RRRR-MM-DD; od tego dnia wyjątek nie działa.
	Expires string `json:"expires"`
	Reason  string `json:"reason"`
	// Scope — image (pakiety obrazu, domyślnie) albo go (moduły narzędzi
	// z tools/, govulncheck).
	Scope string `json:"scope,omitempty"`
}

// Zakresy wyjątków.
const (
	ScopeImage = "image"
	ScopeGo    = "go"
)

// InScope zwraca wyjątki jednego zakresu.
func InScope(exceptions []Exception, scope string) []Exception {
	var out []Exception
	for _, e := range exceptions {
		s := e.Scope
		if s == "" {
			s = ScopeImage
		}
		if s == scope {
			out = append(out, e)
		}
	}
	return out
}

func (e Exception) expiry() (time.Time, error) {
	return time.Parse("2006-01-02", e.Expires)
}

// LoadExceptions czyta wyjątki ściśle (nieznane pole to błąd) i sprawdza
// każdy: identyfikator, pakiet, uzasadnienie i datę nie dalszą niż
// MaxExceptionDays od dziś. Wyjątki po dacie nie są błędem pliku —
// odrzuca je ocena, gdy pasują do znalezionej podatności.
func LoadExceptions(path string, today time.Time) ([]Exception, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		Exceptions []Exception `json:"exceptions"`
	}
	if err := yaml.UnmarshalStrict(raw, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var problems []string
	for i, e := range file.Exceptions {
		where := fmt.Sprintf("wyjątek %d (%s)", i+1, e.ID)
		if e.ID == "" || e.Package == "" {
			problems = append(problems, where+": id i package wymagane")
		}
		if len(strings.TrimSpace(e.Reason)) < 20 {
			problems = append(problems, where+": reason — uzasadnienie, nie etykieta")
		}
		if e.Scope != "" && e.Scope != ScopeImage && e.Scope != ScopeGo {
			problems = append(problems, fmt.Sprintf("%s: scope=%q — image albo go", where, e.Scope))
		}
		exp, err := e.expiry()
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: expires=%q — RRRR-MM-DD", where, e.Expires))
			continue
		}
		if exp.After(today.AddDate(0, 0, MaxExceptionDays)) {
			problems = append(problems, fmt.Sprintf("%s: expires %s — dalej niż %d dni od dziś", where, e.Expires, MaxExceptionDays))
		}
	}
	if len(problems) > 0 {
		return nil, cli.Unmet("%s: %s", path, strings.Join(problems, "; "))
	}
	return file.Exceptions, nil
}

// Finding — podatność w pakiecie obrazu.
type Finding struct {
	// ID — CVE, jeśli podatność go ma, inaczej identyfikator OSV.
	ID        string   `json:"id"`
	Aliases   []string `json:"aliases"`
	Package   string   `json:"package"`
	Version   string   `json:"version"`
	Ecosystem string   `json:"ecosystem"`
	// Score — najwyższa ocena CVSS z grupy OSV; pusta, gdy baza jej nie ma.
	Score    string   `json:"cvss_score"`
	Severity string   `json:"severity"`
	Fixed    []string `json:"fixed_versions"`
	Blocking bool     `json:"blocking"`
	// Note — dlaczego nie blokuje albo dlaczego blokuje mimo wyjątku.
	Note string `json:"note,omitempty"`
}

func (f Finding) key() string { return f.Package + " " + f.ID }

func (f Finding) matches(e Exception) bool {
	if !strings.EqualFold(e.Package, f.Package) {
		return false
	}
	return slices.ContainsFunc(append([]string{f.ID}, f.Aliases...), func(id string) bool { return strings.EqualFold(id, e.ID) })
}

// Wagi według CVSS (pierwsza wersja specyfikacji, ta sama skala w v3 i v4).
const (
	SeverityCritical = "CRITICAL"
	SeverityHigh     = "HIGH"
	SeverityMedium   = "MEDIUM"
	SeverityLow      = "LOW"
	SeverityUnknown  = "UNKNOWN"
)

func severity(score string) string {
	v, err := strconv.ParseFloat(score, 64)
	switch {
	case err != nil || v <= 0:
		return SeverityUnknown
	case v >= 9.0:
		return SeverityCritical
	case v >= 7.0:
		return SeverityHigh
	case v >= 4.0:
		return SeverityMedium
	default:
		return SeverityLow
	}
}

// osvOutput — to, czego bramka potrzebuje z `osv-scanner --format json`.
type osvOutput struct {
	Results []struct {
		Packages []struct {
			Package struct {
				Name      string `json:"name"`
				Version   string `json:"version"`
				Ecosystem string `json:"ecosystem"`
			} `json:"package"`
			Vulnerabilities []struct {
				ID       string   `json:"id"`
				Aliases  []string `json:"aliases"`
				Affected []struct {
					Package struct {
						Name      string `json:"name"`
						Ecosystem string `json:"ecosystem"`
					} `json:"package"`
					Ranges []struct {
						Events []map[string]string `json:"events"`
					} `json:"ranges"`
				} `json:"affected"`
			} `json:"vulnerabilities"`
			Groups []struct {
				IDs         []string `json:"ids"`
				Aliases     []string `json:"aliases"`
				MaxSeverity string   `json:"max_severity"`
			} `json:"groups"`
		} `json:"packages"`
	} `json:"results"`
}

// Parse zamienia wynik OSV-Scannera na podatności: jedna na grupę
// (identyfikatory i aliasy tej samej podatności) w pakiecie.
func Parse(raw []byte) ([]Finding, error) {
	var out osvOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("wynik osv-scanner: %w", err)
	}
	var findings []Finding
	for _, r := range out.Results {
		for _, p := range r.Packages {
			for _, g := range p.Groups {
				f := Finding{
					Package: p.Package.Name, Version: p.Package.Version, Ecosystem: p.Package.Ecosystem,
					Score: g.MaxSeverity, Severity: severity(g.MaxSeverity),
				}
				ids := append(slices.Clone(g.IDs), g.Aliases...)
				for _, v := range p.Vulnerabilities {
					if !slices.Contains(g.IDs, v.ID) {
						continue
					}
					ids = append(ids, v.Aliases...)
					for _, a := range v.Affected {
						if a.Package.Name != p.Package.Name || a.Package.Ecosystem != p.Package.Ecosystem {
							continue
						}
						for _, rg := range a.Ranges {
							for _, ev := range rg.Events {
								if fixed := ev["fixed"]; fixed != "" && !slices.Contains(f.Fixed, fixed) {
									f.Fixed = append(f.Fixed, fixed)
								}
							}
						}
					}
				}
				slices.Sort(ids)
				ids = slices.Compact(ids)
				f.ID = ids[0]
				for _, id := range ids {
					if strings.HasPrefix(id, "CVE-") {
						f.ID = id
						break
					}
				}
				for _, id := range ids {
					if id != f.ID {
						f.Aliases = append(f.Aliases, id)
					}
				}
				findings = append(findings, f)
			}
		}
	}
	slices.SortFunc(findings, func(a, b Finding) int { return strings.Compare(a.key(), b.key()) })
	return findings, nil
}

// Policy — jak oceniać podatności.
type Policy struct {
	Exceptions []Exception
	Today      time.Time
	// Baseline — podatności obrazu bazowego (main); nil — bez porównania.
	Baseline []Finding
}

// Evaluate oznacza podatności blokujące i zwraca wyjątki, które do niczego
// nie pasują (do usunięcia).
func (p Policy) Evaluate(findings []Finding) ([]Finding, []string) {
	used := map[int]bool{}
	baseline := map[string]bool{}
	for _, f := range p.Baseline {
		baseline[f.key()] = true
	}
	out := slices.Clone(findings)
	for i := range out {
		f := &out[i]
		switch {
		case len(f.Fixed) == 0:
			f.Note = "bez wersji z poprawką"
			continue
		case f.Severity == SeverityMedium || f.Severity == SeverityLow:
			f.Note = "poniżej progu (HIGH)"
			continue
		}
		excepted := false
		for j, e := range p.Exceptions {
			if !f.matches(e) {
				continue
			}
			used[j] = true
			exp, _ := e.expiry()
			if !p.Today.Before(exp) {
				f.Note = "wyjątek wygasł " + e.Expires
				continue
			}
			excepted = true
			f.Note = "wyjątek do " + e.Expires + ": " + e.Reason
		}
		switch {
		case excepted:
		case p.Baseline != nil && baseline[f.key()]:
			f.Note = "jest też w obrazie bazowym (main) — nie wnosi jej ta zmiana"
		default:
			f.Blocking = true
		}
	}
	var unused []string
	for j, e := range p.Exceptions {
		if !used[j] {
			unused = append(unused, e.ID+" ("+e.Package+")")
		}
	}
	return out, unused
}

// Database — jedna baza ekosystemu pobrana przez OSV-Scanner.
type Database struct {
	Ecosystem string `json:"ecosystem"`
	SHA256    string `json:"sha256"`
	Entries   int    `json:"entries"`
	// Newest — najnowsza zmiana wpisu w bazie: migawka, z której jest wynik.
	Newest string `json:"newest_modified"`
}

// Databases opisuje bazy w katalogu OSV-Scannera (osv-scalibr/<ekosystem>/all.zip).
func Databases(dir string) ([]Database, error) {
	zips, err := filepath.Glob(filepath.Join(dir, "osv-scalibr", "*", "all.zip"))
	if err != nil {
		return nil, err
	}
	var dbs []Database
	for _, path := range zips {
		db, err := describe(path)
		if err != nil {
			return nil, err
		}
		dbs = append(dbs, db)
	}
	return dbs, nil
}

func describe(path string) (Database, error) {
	db := Database{Ecosystem: filepath.Base(filepath.Dir(path))}
	f, err := os.Open(path)
	if err != nil {
		return db, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return db, err
	}
	db.SHA256 = hex.EncodeToString(h.Sum(nil))
	z, err := zip.OpenReader(path)
	if err != nil {
		return db, fmt.Errorf("%s: %w", path, err)
	}
	defer z.Close()
	for _, entry := range z.File {
		rc, err := entry.Open()
		if err != nil {
			return db, err
		}
		var record struct {
			Modified string `json:"modified"`
		}
		err = json.NewDecoder(rc).Decode(&record)
		_ = rc.Close()
		if err != nil {
			return db, fmt.Errorf("%s/%s: %w", path, entry.Name, err)
		}
		db.Entries++
		if record.Modified > db.Newest {
			db.Newest = record.Modified
		}
	}
	return db, nil
}

// Scanner uruchamia OSV-Scanner z bazami w DBDir.
type Scanner struct {
	Runner proc.Runner
	DBDir  string
}

// ScanArchive skanuje archiwum obrazu. download — pobrać bazy (pierwszy
// skan biegu); bez tego skan używa baz pobranych wcześniej, więc dwa skany
// porównywane ze sobą widzą tę samą migawkę.
func (s Scanner) ScanArchive(ctx context.Context, archive, output string, download bool) ([]Finding, error) {
	args := []string{"scan", "image", "--archive", archive, "--offline-vulnerabilities", "--format", "json", "--output-file", output}
	if download {
		args = append(args, "--download-offline-databases")
	}
	_, err := s.Runner.Run(ctx, proc.Cmd{Name: "osv-scanner", Args: args, Env: []string{"OSV_SCANNER_LOCAL_DB_CACHE_DIRECTORY=" + s.DBDir}})
	// 0 — brak podatności, 1 — są (to wynik, nie awaria); 128 — skaner nie
	// znalazł żadnych pakietów, czyli nic nie sprawdził.
	switch code := proc.ExitCode(err); code {
	case 0, 1:
	case 128:
		return nil, fmt.Errorf("osv-scanner nie znalazł pakietów w %s — skan niczego nie sprawdził", archive)
	default:
		return nil, fmt.Errorf("osv-scanner: %w", err)
	}
	raw, err := os.ReadFile(output)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}
