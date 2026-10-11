// Package ci to słownik biegów CI (GitHub Actions), którym etap okresu
// próbnego rozmawia z adapterem GitHuba.
package ci

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
