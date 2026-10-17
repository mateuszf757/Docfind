package artifact

import (
	"fmt"
	"slices"

	"github.com/mateuszf757/Docfind/tools/internal/core/ci"
)

// EvidencePredicate — typ predykatu pakietu dowodów wydania (decyzja 53).
const EvidencePredicate = "https://github.com/mateuszf757/Docfind/attestations/evidence/v1"

// Evidence — pakiet dowodów wydania: to, co trzeba umieć wykazać o wydaniu,
// gdy biegi, checki i artefakty Actions już znikną (repozytorium publiczne:
// najwyżej 90 dni, od 2026-10-01 także biegi i checki). Predykat atestacji
// Sigstore przy digeście wydania — podpisany, w Rekorze i obok obrazu
// w GHCR — żyje tyle co obraz.
type Evidence struct {
	// Release — zapis wydania z tagu: digest, commit, akceptacja na stagingu
	// i commit kodu bramek.
	Release Release `json:"release"`
	// Main — biegi workflowu ci po push na main dla commita wydania, od
	// najnowszego: wyniki zadań i artefakty z digestami.
	Main  []EvidenceRun `json:"main_runs"`
	Tools EvidenceTools `json:"tools"`
}

// EvidenceRun — bieg z wynikami zadań i artefaktami.
type EvidenceRun struct {
	ci.Run
	Jobs      []ci.Job           `json:"jobs"`
	Artifacts []EvidenceArtifact `json:"artifacts"`
}

// EvidenceArtifact — artefakt biegu: skrót SHA-256 archiwum z API GitHuba.
// Treść znika po retencji; skrót zostaje i potwierdza kopię, jeśli ktoś ją
// zachował.
type EvidenceArtifact struct {
	Name      string `json:"name"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size_in_bytes"`
	Expired   bool   `json:"expired"`
	ExpiresAt string `json:"expires_at"`
}

// EvidenceTools — wersje narzędzi, którymi zebrano pakiet: skrót mise.lock
// z commita wydania, commit kodu dft i wersja Go.
type EvidenceTools struct {
	MiseLockSHA256 string `json:"mise_lock_sha256"`
	DftRevision    string `json:"dft_revision"`
	GoVersion      string `json:"go_version"`
}

// MainJobs — zadania biegu main, które muszą być zielone w biegu commita
// wydania: testy i obraz, publikacja z atestacją, klaster na digeście
// z GHCR i chart, który trafił na staging.
var MainJobs = []string{"test", "build-publish", "attest", "cluster", "chart"}

// Check sprawdza pakiet: wydanie poprawne, a ostatni bieg push na main dla
// commita wydania zakończony sukcesem, z zadaniami MainJobs zielonymi —
// bieg zielony z pominiętym klastrem nie jest dowodem, że digest przeszedł
// klaster (decyzja 47).
func (e Evidence) Check() []string {
	var found []string
	if err := e.Release.Validate(); err != nil {
		found = append(found, err.Error())
	}
	if len(e.Main) == 0 {
		return append(found, fmt.Sprintf("brak biegu ci po push na main dla commita %s", ShortCommit(e.Release.Commit)))
	}
	run := e.Main[0]
	if run.HeadSHA != e.Release.Commit {
		found = append(found, fmt.Sprintf("bieg %d dotyczy commita %s, a wydanie %s", run.ID, ShortCommit(run.HeadSHA), ShortCommit(e.Release.Commit)))
	}
	if run.Conclusion != "success" {
		found = append(found, fmt.Sprintf("bieg main %s: %s", run.URL, run.Conclusion))
	}
	for _, name := range MainJobs {
		i := slices.IndexFunc(run.Jobs, func(j ci.Job) bool { return j.Name == name })
		switch {
		case i < 0:
			found = append(found, fmt.Sprintf("bieg main %s bez zadania %s", run.URL, name))
		case run.Jobs[i].Conclusion != "success":
			found = append(found, fmt.Sprintf("bieg main %s: zadanie %s — %s", run.URL, name, run.Jobs[i].Conclusion))
		}
	}
	return found
}
