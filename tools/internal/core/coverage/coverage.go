// Package coverage to pokrycie kodu narzędzi z profilu `go test
// -coverprofile` (§6.1 kontraktu, decyzja 62): odsetek instrukcji
// wykonanych przez jakikolwiek test, per pakiet i dla całego modułu.
package coverage

import (
	"bufio"
	"bytes"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Stat — instrukcje pakietu: wszystkie i wykonane.
type Stat struct {
	Statements int `json:"statements"`
	Covered    int `json:"covered"`
}

// Percent — odsetek wykonanych instrukcji; pakiet bez instrukcji — 100.
func (s Stat) Percent() float64 {
	if s.Statements == 0 {
		return 100
	}
	return 100 * float64(s.Covered) / float64(s.Statements)
}

// Parse czyta profil i zwraca pokrycie każdego pakietu (ścieżka względem
// modułu, np. internal/stages/drain) i całego modułu. Z -coverpkg każdy
// binarny test zapisuje te same bloki — blok jest wykonany, jeśli wykonał
// go którykolwiek.
func Parse(profile []byte, module string) (map[string]Stat, Stat, error) {
	type block struct {
		statements int
		covered    bool
	}
	blocks := map[string]block{}
	scanner := bufio.NewScanner(bytes.NewReader(profile))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, Stat{}, fmt.Errorf("profil pokrycia: nieczytelny wiersz %q", line)
		}
		n, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil {
			return nil, Stat{}, fmt.Errorf("profil pokrycia: nieczytelny wiersz %q", line)
		}
		b := blocks[fields[0]]
		b.statements = n
		b.covered = b.covered || count > 0
		blocks[fields[0]] = b
	}
	if err := scanner.Err(); err != nil {
		return nil, Stat{}, err
	}
	packages := map[string]Stat{}
	var total Stat
	for key, b := range blocks {
		file, _, _ := strings.Cut(key, ":")
		pkg := strings.TrimPrefix(path.Dir(file), module+"/")
		s := packages[pkg]
		s.Statements += b.statements
		total.Statements += b.statements
		if b.covered {
			s.Covered += b.statements
			total.Covered += b.statements
		}
		packages[pkg] = s
	}
	return packages, total, nil
}

// Layer — warstwa pakietu (kit, core, adapters, stages) albo pusta dla cmd.
func Layer(pkg string) string {
	parts := strings.Split(pkg, "/")
	if len(parts) >= 2 && parts[0] == "internal" {
		return parts[1]
	}
	return ""
}

// Below — pakiety z pokryciem poniżej minimum swojej warstwy, w kolejności
// nazw. Warstwa bez minimum (cmd) — poza regułą.
func Below(packages map[string]Stat, minimum map[string]float64) []string {
	var found []string
	for _, pkg := range slices.Sorted(maps.Keys(packages)) {
		min, ok := minimum[Layer(pkg)]
		if !ok {
			continue
		}
		if p := packages[pkg].Percent(); p < min {
			found = append(found, fmt.Sprintf("%s: pokrycie %.1f%% poniżej %.0f%% dla warstwy %s", pkg, p, min, Layer(pkg)))
		}
	}
	return found
}
