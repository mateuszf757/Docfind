// Package ci to słownik biegów CI (GitHub Actions), którym etap okresu
// próbnego rozmawia z adapterem GitHuba.
package ci

import (
	"maps"
	"slices"
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
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
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
