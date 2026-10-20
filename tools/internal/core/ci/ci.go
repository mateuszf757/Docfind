// Package ci to słownik biegów CI (GitHub Actions), którym etap okresu
// próbnego rozmawia z adapterem GitHuba.
package ci

import (
	"maps"
	"slices"
	"strings"
)

// Run — bieg workflowu.
type Run struct {
	ID         int64  `json:"id"`
	Event      string `json:"event"`
	Attempt    int    `json:"run_attempt"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HeadSHA    string `json:"head_sha"`
	CreatedAt  string `json:"created_at"`
	URL        string `json:"html_url"`
}

// Job — zadanie w biegu.
type Job struct {
	Name        string `json:"name"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// Step — krok zadania z API zadań biegu.
type Step struct {
	Name        string `json:"name"`
	Conclusion  string `json:"conclusion"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// JobSteps — zadanie z krokami. Osobny typ, bo Job trafia do pakietu
// dowodów wydania (core/artifact), a tam kroki byłyby szumem w predykacie
// atestacji.
type JobSteps struct {
	Job
	Steps []Step `json:"steps"`
}

// SetupStep — krok podatku startowego: przygotowanie zadania przed
// pierwszą bramką — przydział maszyny, checkout, narzędzia z mise, build
// dft (decyzja 69). Rozpoznawany po nazwie, z konwencji workflowu ci:
// narzędzia w krokach „… (mise)”, build dft w kroku „dft”.
func SetupStep(name string) bool {
	switch {
	case name == "Set up job", name == "Initialize containers", name == "dft":
		return true
	case strings.HasPrefix(name, "Run actions/checkout@"), strings.HasSuffix(name, "(mise)"):
		return true
	}
	return false
}

// Layer — warstwa CI (§3.1 kontraktu).
type Layer string

// Warstwy CI.
const (
	W1 Layer = "W1" // szybka: PR, push, noc
	W2 Layer = "W2" // integracja przed scaleniem
	W3 Layer = "W3" // po scaleniu
	W4 Layer = "W4" // noc
	W5 Layer = "W5" // wydanie
)

// Layers — warstwa każdego zadania workflowu ci (decyzja 57). Zadanie
// bez warstwy to błąd `dft check workflows` (§3.1, §8.6 kontraktu): nowa
// funkcja CI zaczyna od miejsca w warstwach i budżetu. Zadanie, które biegnie
// w kilku zdarzeniach, ma warstwę zdarzenia, dla którego powstało.
var Layers = map[string]Layer{
	"test": W1, "build": W1, "build-publish": W1, "tools": W1, "negatives": W1, "fast-feedback": W1,
	"cluster": W2, "vulns": W2, "integration": W2,
	"attest": W3, "chart": W3, "attest-chart": W3,
	"vulns-issue": W4, "settings-drift": W4, "main-health": W4, "budget": W4,
	"release": W5, "attest-release": W5, "promote": W5,
}

// PullRequest — scalony PR: ostatni commit (ten, na którym CI dało wynik
// przed scaleniem) i chwila scalenia.
type PullRequest struct {
	Number   int    `json:"number"`
	HeadSHA  string `json:"head_sha"`
	MergedAt string `json:"merged_at"`
	URL      string `json:"html_url"`
}

// Need — wynik zadania z needs zadania zbierającego (toJSON(needs)
// w workflowie).
type Need struct {
	Result string `json:"result"`
}

// Unmet — zadania z needs, które nie skończyły się sukcesem, jako
// „zadanie: wynik”, w kolejności nazw. Pominięte to nie zaliczone (decyzja
// 47): zadanie zbierające warstwę przechodzi tylko wtedy, gdy każde zadanie
// warstwy dało success — wymagany status, który przepuszcza pominięte,
// przepuszcza też każdą bramkę, którą ktoś wyłączył warunkiem.
func Unmet(needs map[string]Need) []string {
	var unmet []string
	for _, job := range slices.Sorted(maps.Keys(needs)) {
		if r := needs[job].Result; r != "success" {
			unmet = append(unmet, job+": "+r)
		}
	}
	return unmet
}
