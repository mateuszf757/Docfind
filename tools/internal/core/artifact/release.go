package artifact

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// fullCommit — pełny identyfikator commita SHA-1.
var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ReleasePredicate — typ predykatu atestacji wydania (decyzja 40): digest
// obrazu zbudowanego na main jest wydaniem X.Y.Z, przyjętym na stagingu.
const ReleasePredicate = "https://github.com/mateuszf757/Docfind/attestations/release/v1"

// Release — wydanie: obraz commita z main pod numerem X.Y.Z. Ten sam zapis
// niesie tag z adnotacją vX.Y.Z (pisze go `dft release` po akceptacji na
// stagingu) i atestacja wydania (wystawia ją CI na tagu) — wydanie nie
// buduje obrazu, tylko nazywa digest, który już przeszedł bramki.
type Release struct {
	Version string `json:"version"`
	// Title — pierwsza linia tagu: co zawiera wydanie.
	Title string `json:"title"`
	// Image — repozytorium@sha256:… indeksu w rejestrze.
	Image string `json:"image"`
	// Commit — commit, z którego zbudowano obraz; tag wskazuje na ten commit.
	Commit string `json:"commit"`
	// AcceptedAt — chwila akceptacji na stagingu (RFC 3339, UTC).
	AcceptedAt string `json:"accepted_at"`
	// Staging — wyniki bramek akceptacji na stagingu.
	Staging string `json:"staging"`
	// GatesCommit — commit kodu bramek, którym biegła akceptacja (dft
	// zbudowany z czystego drzewa, vcs.revision): wynik akceptacji bez wersji
	// kodu, który go wydał, nie da się sprawdzić (decyzja 48).
	GatesCommit string `json:"gates_commit"`
}

// Tag — nazwa tagu wydania.
func (r Release) Tag() string { return "v" + r.Version }

// Przyczepy (git trailers) tagu wydania — część czytana przez program.
const (
	trailerImage      = "Image"
	trailerCommit     = "Commit"
	trailerAcceptedAt = "Accepted-At"
	trailerGates      = "Gates-Commit"
	stagingPrefix     = "Akceptacja na stagingu: "
)

// Validate sprawdza wydanie przed zapisem w tagu i po odczycie z niego.
func (r Release) Validate() error {
	var problems []string
	if !IsRelease(r.Version) {
		problems = append(problems, fmt.Sprintf("numer %q spoza X.Y.Z", r.Version))
	}
	if _, _, err := ParseRef(r.Image); err != nil {
		problems = append(problems, fmt.Sprintf("obraz %q bez digestu", r.Image))
	}
	if len(r.Commit) != 40 {
		problems = append(problems, fmt.Sprintf("commit %q nie jest pełnym SHA", r.Commit))
	}
	if _, err := time.Parse(time.RFC3339, r.AcceptedAt); err != nil {
		problems = append(problems, fmt.Sprintf("czas akceptacji %q spoza RFC 3339", r.AcceptedAt))
	}
	if strings.TrimSpace(r.Staging) == "" {
		problems = append(problems, "brak wyników akceptacji na stagingu")
	}
	if !fullCommit.MatchString(r.GatesCommit) {
		problems = append(problems, fmt.Sprintf("commit kodu bramek %q nie jest pełnym SHA", r.GatesCommit))
	}
	if len(problems) > 0 {
		return fmt.Errorf("wydanie %s: %s", r.Tag(), strings.Join(problems, "; "))
	}
	return nil
}

// TagMessage — treść tagu z adnotacją: tytuł i wyniki akceptacji dla
// człowieka, przyczepy dla programu (CI czyta z nich digest).
func (r Release) TagMessage() string {
	title := r.Title
	if title == "" {
		title = "Wydanie " + r.Version
	}
	return fmt.Sprintf("%s\n\n%s%s\n\n%s: %s\n%s: %s\n%s: %s\n%s: %s\n",
		title, stagingPrefix, r.Staging,
		trailerImage, r.Image, trailerCommit, r.Commit, trailerAcceptedAt, r.AcceptedAt, trailerGates, r.GatesCommit)
}

// ParseReleaseTag czyta wydanie z tagu vX.Y.Z i jego treści. Tag bez
// przyczep to tag spoza `dft release` — wydaniem nie jest.
func ParseReleaseTag(tag, message string) (Release, error) {
	version, ok := ReleaseOfTag(tag)
	if !ok {
		return Release{}, fmt.Errorf("tag %q spoza vX.Y.Z", tag)
	}
	r := Release{Version: version}
	scanner := bufio.NewScanner(strings.NewReader(message))
	first := true
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " ")
		if first && line != "" {
			r.Title, first = line, false
			continue
		}
		if staging, ok := strings.CutPrefix(line, stagingPrefix); ok {
			r.Staging = staging
			continue
		}
		key, value, ok := strings.Cut(line, ": ")
		switch {
		case !ok:
		case key == trailerImage:
			r.Image = value
		case key == trailerCommit:
			r.Commit = value
		case key == trailerAcceptedAt:
			r.AcceptedAt = value
		case key == trailerGates:
			r.GatesCommit = value
		}
	}
	if err := r.Validate(); err != nil {
		return Release{}, fmt.Errorf("%w — tag wydania powstaje przez ./bin/mise run release", err)
	}
	return r, nil
}
