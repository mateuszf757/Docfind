// Package osv to adapter OSV-Scannera: skan archiwum obrazu na bazach
// pobranych raz na bieg do pamięci podręcznej repozytorium i opis tych baz
// (suma, liczba wpisów, najnowszy wpis) — z jakiej migawki jest wynik.
package osv

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
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

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
func Parse(raw []byte) ([]vuln.Finding, error) {
	var out osvOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("wynik osv-scanner: %w", err)
	}
	var findings []vuln.Finding
	for _, r := range out.Results {
		for _, p := range r.Packages {
			for _, g := range p.Groups {
				f := vuln.Finding{
					Package: p.Package.Name, Version: p.Package.Version, Ecosystem: p.Package.Ecosystem,
					Score: g.MaxSeverity, Severity: vuln.SeverityFromCVSS(g.MaxSeverity),
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
	slices.SortFunc(findings, func(a, b vuln.Finding) int { return strings.Compare(a.Key(), b.Key()) })
	return findings, nil
}

// Databases opisuje bazy w katalogu OSV-Scannera (osv-scalibr/<ekosystem>/all.zip).
func Databases(dir string) ([]vuln.Database, error) {
	zips, err := filepath.Glob(filepath.Join(dir, "osv-scalibr", "*", "all.zip"))
	if err != nil {
		return nil, err
	}
	var dbs []vuln.Database
	for _, path := range zips {
		db, err := describe(path)
		if err != nil {
			return nil, err
		}
		dbs = append(dbs, db)
	}
	return dbs, nil
}

func describe(path string) (vuln.Database, error) {
	db := vuln.Database{Ecosystem: filepath.Base(filepath.Dir(path))}
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

// Databases opisuje bazy, którymi skanuje Scanner.
func (s Scanner) Databases() ([]vuln.Database, error) { return Databases(s.DBDir) }

// Scan skanuje archiwum obrazu. download — pobrać bazy (pierwszy skan
// biegu); bez tego skan używa baz pobranych wcześniej, więc dwa skany
// porównywane ze sobą widzą tę samą migawkę.
func (s Scanner) Scan(ctx context.Context, archive string, download bool) (_ []vuln.Finding, err error) {
	tmp, err := os.MkdirTemp("", "dft-osv-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	output := filepath.Join(tmp, "osv.json")
	args := []string{"scan", "image", "--archive", archive, "--offline-vulnerabilities", "--format", "json", "--output-file", output}
	if download {
		args = append(args, "--download-offline-databases")
	}
	_, err = s.Runner.Run(ctx, proc.Cmd{Name: "osv-scanner", Args: args, Env: []string{"OSV_SCANNER_LOCAL_DB_CACHE_DIRECTORY=" + s.DBDir}})
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
