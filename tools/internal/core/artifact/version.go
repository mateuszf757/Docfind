package artifact

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ShortCommitLength — długość skróconego commita w wersji (metadane builda
// SemVer, „+abc1234").
//
// Stała, a nie `git rev-parse --short`: tamto wydłuża skrót, gdy jest
// niejednoznaczny w danym repozytorium, a domyślną długość bierze
// z core.abbrev w konfiguracji użytkownika — ten sam commit miałby inną
// wersję lokalnie i w CI.
const ShortCommitLength = 7

// releaseTag to tag wydania vX.Y.Z (decyzja 30). Liczby jak w SemVer: bez
// zer wiodących — tag v01.2.3 nie wyznacza wersji, tak jak v0.4.0-rc.1.
var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// IsReleaseTag mówi, czy tag to tag wydania vX.Y.Z.
func IsReleaseTag(tag string) bool { return releaseTag.MatchString(tag) }

// IsRelease mówi, czy wersja to wersja wydania X.Y.Z — bez części
// przedwydaniowej i metadanych, które ma każdy build spoza tagu (decyzja 30).
func IsRelease(version string) bool { return releaseVersion.MatchString(version) }

// ReleaseOfTag zwraca numer wydania z tagu vX.Y.Z.
func ReleaseOfTag(tag string) (string, bool) {
	if !IsReleaseTag(tag) {
		return "", false
	}
	return strings.TrimPrefix(tag, "v"), true
}

// CompareReleases porównuje numery wydań X.Y.Z liczbowo: -1, 0 albo 1.
// Wywołujący podaje numery, które przeszły IsRelease.
func CompareReleases(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range 3 {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// Source — fakty o commicie z systemu kontroli wersji, z których wynika
// wersja artefaktu. Zbiera je adapter gita; reguła jest tutaj.
type Source struct {
	// Commit — pełny identyfikator HEAD.
	Commit string
	// Dirty — drzewo robocze ma zmiany, także pliki nieśledzone.
	Dirty bool
	// Tag — najbliższy tag wydania vX.Y.Z osiągalny z HEAD; pusty — brak.
	Tag string
	// Distance — commity od Tag do HEAD.
	Distance int
	// Commits — liczba commitów do HEAD (gdy nie ma tagu wydania).
	Commits int
}

// Version zwraca wersję z gita (decyzja 30) — wersję wdrożenia, którą chart
// podaje aplikacji w DOCFIND_VERSION; obraz jej nie niesie (decyzja 40):
//
//   - na tagu vX.Y.Z, przy czystym drzewie: X.Y.Z;
//   - za tagiem albo z brudnym drzewem: X.Y.(Z+1)-dev.<commity od tagu>+<sha>,
//     czyli wersja przedpremierowa następnej łatki — w porządku SemVer
//     powyżej wydania, z którego wyrosła. Surowe `git describe`
//     (0.3.0-5-gabc1234) SemVer czyta jako wersję starszą od 0.3.0, więc
//     zakresy wersji (chart OCI, Argo CD) posortowałyby je przed wydaniem;
//   - bez tagu: 0.0.0-dev.<liczba commitów>+<sha>;
//   - brudne drzewo dostaje przyrostek -dirty, także na tagu: obraz z
//     niezacommitowanych zmian nie może podać się za wydanie.
func Version(s Source) (string, error) {
	if len(s.Commit) < ShortCommitLength {
		return "", fmt.Errorf("nieoczekiwany identyfikator commita %q", s.Commit)
	}
	sha := s.Commit[:ShortCommitLength]
	dirty := ""
	if s.Dirty {
		dirty = "-dirty"
	}
	if s.Tag == "" {
		return fmt.Sprintf("0.0.0-dev.%d+%s%s", s.Commits, sha, dirty), nil
	}
	m := releaseTag.FindStringSubmatch(s.Tag)
	if m == nil {
		return "", fmt.Errorf("%q to nie jest tag wydania vX.Y.Z", s.Tag)
	}
	if s.Distance == 0 && !s.Dirty {
		return fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3]), nil
	}
	patch, err := strconv.Atoi(m[3])
	if err != nil {
		return "", fmt.Errorf("numer łatki w %s: %w", s.Tag, err)
	}
	return fmt.Sprintf("%s.%s.%d-dev.%d+%s%s", m[1], m[2], patch+1, s.Distance, sha, dirty), nil
}

// SourceDate zamienia znacznik czasu na zapis ISO 8601 w UTC, jak w
// version.json i etykiecie org.opencontainers.image.created.
func SourceDate(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// DefaultOwner — właściciel repozytorium, gdy GITHUB_REPOSITORY_OWNER jest pusty.
const DefaultOwner = "mateuszf757"

// ImageName zwraca nazwę obrazu usługi: <rejestr>/docfind-<usługa>.
//
// Rejestr domyślnie to GHCR konta właściciela; ścieżka obrazu w rejestrze jest
// małymi literami, a nazwa konta GitHuba nie musi być. registry (DF_IMAGE_REGISTRY)
// zastępuje go w całości — publikację sprawdza się wtedy na tymczasowym
// rejestrze na loopbacku tym samym kodem, zamiast go łatać.
func ImageName(registry, owner, service string) string {
	if registry == "" {
		if owner == "" {
			owner = DefaultOwner
		}
		registry = "ghcr.io/" + strings.ToLower(owner)
	}
	return fmt.Sprintf("%s/docfind-%s", strings.TrimSuffix(registry, "/"), service)
}
