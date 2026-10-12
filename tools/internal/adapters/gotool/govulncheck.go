package gotool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/vuln"
)

// ParseGovulncheck zamienia strumień `govulncheck -format json` na
// podatności w kodzie osiągalnym: tylko ustalenia na poziomie symbolu
// (pierwsza ramka śladu z funkcją). Ustalenia o module albo pakiecie znaczą
// „zależność ma podatność", a nie „kod ją wywołuje" — tego govulncheck
// w trybie tekstowym też nie zgłasza.
//
// Baza Go nie podaje oceny CVSS, więc waga jest nieznana — a nieznana
// blokuje jak HIGH, gdy jest poprawka.
//
// Zwraca też migawkę bazy z komunikatu config (db_last_modified): po niej
// porównanie z punktem odniesienia sprawdza, że oba skany widziały tę samą
// bazę. Strumień bez niej to wynik, którego nie da się z niczym porównać.
func ParseGovulncheck(r io.Reader) ([]vuln.Finding, vuln.Database, error) {
	type frame struct {
		Module   string `json:"module"`
		Version  string `json:"version"`
		Function string `json:"function"`
	}
	type message struct {
		Config *struct {
			DB             string `json:"db"`
			DBLastModified string `json:"db_last_modified"`
		} `json:"config"`
		OSV *struct {
			ID      string   `json:"id"`
			Aliases []string `json:"aliases"`
		} `json:"osv"`
		Finding *struct {
			OSV          string  `json:"osv"`
			FixedVersion string  `json:"fixed_version"`
			Trace        []frame `json:"trace"`
		} `json:"finding"`
	}
	aliases := map[string][]string{}
	seen := map[string]bool{}
	var findings []vuln.Finding
	db := vuln.Database{Ecosystem: "Go"}
	dec := json.NewDecoder(r)
	for {
		var m message
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, db, fmt.Errorf("wynik govulncheck: %w", err)
		}
		if m.Config != nil {
			db.Ecosystem = "Go (" + m.Config.DB + ")"
			db.Newest = m.Config.DBLastModified
		}
		if m.OSV != nil {
			aliases[m.OSV.ID] = m.OSV.Aliases
		}
		f := m.Finding
		if f == nil || len(f.Trace) == 0 || f.Trace[0].Function == "" {
			continue
		}
		top := f.Trace[0]
		if seen[f.OSV+" "+top.Module] {
			continue
		}
		seen[f.OSV+" "+top.Module] = true
		finding := vuln.Finding{ID: f.OSV, Package: top.Module, Version: top.Version, Ecosystem: "Go", Severity: vuln.SeverityUnknown}
		if f.FixedVersion != "" {
			finding.Fixed = []string{f.FixedVersion}
		}
		findings = append(findings, finding)
	}
	for i := range findings {
		findings[i].Aliases = slices.Clone(aliases[findings[i].ID])
	}
	slices.SortFunc(findings, func(a, b vuln.Finding) int { return strings.Compare(a.Key(), b.Key()) })
	if db.Newest == "" {
		return nil, db, errors.New("wynik govulncheck bez daty bazy (config.db_last_modified)")
	}
	return findings, db, nil
}
