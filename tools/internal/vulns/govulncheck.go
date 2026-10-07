package vulns

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// ParseGovulncheck zamienia strumień `govulncheck -format json` na
// podatności w kodzie osiągalnym: tylko ustalenia na poziomie symbolu
// (pierwsza ramka śladu z funkcją). Ustalenia o module albo pakiecie znaczą
// „zależność ma podatność", a nie „kod ją wywołuje" — tego govulncheck
// w trybie tekstowym też nie zgłasza.
//
// Baza Go nie podaje oceny CVSS, więc waga jest nieznana — a nieznana
// blokuje jak HIGH, gdy jest poprawka.
func ParseGovulncheck(r io.Reader) ([]Finding, error) {
	type frame struct {
		Module   string `json:"module"`
		Version  string `json:"version"`
		Function string `json:"function"`
	}
	type message struct {
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
	var findings []Finding
	dec := json.NewDecoder(r)
	for {
		var m message
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("wynik govulncheck: %w", err)
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
		finding := Finding{ID: f.OSV, Package: top.Module, Version: top.Version, Ecosystem: "Go", Severity: SeverityUnknown}
		if f.FixedVersion != "" {
			finding.Fixed = []string{f.FixedVersion}
		}
		findings = append(findings, finding)
	}
	for i := range findings {
		findings[i].Aliases = slices.Clone(aliases[findings[i].ID])
	}
	slices.SortFunc(findings, func(a, b Finding) int { return strings.Compare(a.key(), b.key()) })
	return findings, nil
}
