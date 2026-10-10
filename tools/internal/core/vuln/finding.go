package vuln

import (
	"slices"
	"strconv"
	"strings"
	"time"
)

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

// Key — pakiet i identyfikator: porządek raportu i porównanie z obrazem
// bazowym.
func (f Finding) Key() string { return f.Package + " " + f.ID }

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

// SeverityFromCVSS zamienia ocenę CVSS (napis z bazy OSV) na wagę; brak
// albo nieczytelna ocena to waga nieznana.
func SeverityFromCVSS(score string) string {
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
		baseline[f.Key()] = true
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
		case p.Baseline != nil && baseline[f.Key()]:
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
