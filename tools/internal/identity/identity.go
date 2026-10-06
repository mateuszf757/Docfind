// Package identity wyznacza tożsamość artefaktu z gita: wersję, commit,
// czas źródeł i tag obrazu (decyzje 21 i 30).
//
// To jedyne miejsce, które odpowiada na pytanie „co to za wersja i z jakiego
// commita". /version, etykiety OCI i tagi w rejestrze biorą odpowiedź stąd —
// gdyby każde pytało gita po swojemu, /version zacząłby kłamać, a cała
// diagnostyka stoi na tym endpoincie.
package identity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// ShortCommitLength — długość skróconego commita w wersji (metadane builda
// SemVer, „+abc1234").
//
// Stała, a nie `git rev-parse --short`: tamto wydłuża skrót, gdy jest
// niejednoznaczny w danym repozytorium, a domyślną długość bierze
// z core.abbrev w konfiguracji użytkownika. Wersja trafia do version.json,
// czyli do warstwy obrazu — ten sam commit zbudowany lokalnie (więcej
// obiektów, inna konfiguracja) i w CI dawałby inny obraz (decyzja 21).
const ShortCommitLength = 7

// DockerTagMaxLength — limit długości tagu obrazu w rejestrze.
const DockerTagMaxLength = 128

// releaseTag to tag wydania vX.Y.Z (decyzja 30). Liczby jak w SemVer: bez
// zer wiodących — tag v01.2.3 nie wyznacza wersji, tak jak v0.4.0-rc.1.
var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Repo to repozytorium gita, z którego bierze się tożsamość.
type Repo struct {
	// Dir — dowolny katalog wewnątrz repozytorium.
	Dir    string
	Runner proc.Runner
	// Now — zegar dla brudnego drzewa; nil oznacza time.Now.
	Now func() time.Time
}

// Root zwraca korzeń repozytorium zawierającego katalog dir.
func Root(ctx context.Context, r proc.Runner, dir string) (string, error) {
	root, err := proc.Output(ctx, r, proc.Cmd{Name: "git", Args: []string{"-C", dir, "rev-parse", "--show-toplevel"}})
	if err != nil {
		return "", fmt.Errorf("katalog %s nie jest w repozytorium gita: %w", dir, err)
	}
	return root, nil
}

func (r Repo) git(ctx context.Context, args ...string) (string, error) {
	return proc.Output(ctx, r.Runner, proc.Cmd{Name: "git", Args: append([]string{"-C", r.Dir}, args...)})
}

// Status zwraca zmiany w drzewie roboczym w postaci `git status --porcelain`;
// pusty napis oznacza czyste drzewo.
//
// Pliki nieśledzone liczą się jak zmiany: obraz zbudowany z pliku, którego
// nie ma w commicie, nie da się odtworzyć z commita. `git describe --dirty`
// ich nie widzi — tak powstał błąd poprawiony w decyzji 30. Tryb plików
// nieśledzonych jest podany jawnie, bo status.showUntrackedFiles=no
// w konfiguracji użytkownika ukryłby je także tutaj.
func (r Repo) Status(ctx context.Context) (string, error) {
	res, err := r.Runner.Run(ctx, proc.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "status", "--porcelain", "--untracked-files=normal"},
	})
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	return strings.TrimRight(string(res.Stdout), "\n"), nil
}

// Commit zwraca pełny identyfikator HEAD.
func (r Repo) Commit(ctx context.Context) (string, error) {
	commit, err := r.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	if len(commit) < ShortCommitLength {
		return "", fmt.Errorf("nieoczekiwany identyfikator commita %q", commit)
	}
	return commit, nil
}

// Version zwraca wersję artefaktu z najbliższego tagu wydania (decyzja 30).
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
//
// Tagi spoza postaci vX.Y.Z (v0.4.0-rc.1, literówka) są pomijane, a nie
// zatrzymują buildu: tagów v* nie da się usunąć (ruleset), więc jeden zły
// tag zatrzymałby każdy kolejny build na zawsze.
func (r Repo) Version(ctx context.Context) (string, error) {
	status, err := r.Status(ctx)
	if err != nil {
		return "", err
	}
	dirty := ""
	if status != "" {
		dirty = "-dirty"
	}
	commit, err := r.Commit(ctx)
	if err != nil {
		return "", err
	}
	sha := commit[:ShortCommitLength]

	tags, err := r.releaseTags(ctx)
	if err != nil {
		return "", err
	}
	if len(tags) == 0 {
		count, err := r.git(ctx, "rev-list", "--count", "HEAD")
		if err != nil {
			return "", fmt.Errorf("git rev-list --count HEAD: %w", err)
		}
		return fmt.Sprintf("0.0.0-dev.%s+%s%s", count, sha, dirty), nil
	}

	tag, err := r.nearestTag(ctx, tags)
	if err != nil {
		return "", err
	}
	m := releaseTag.FindStringSubmatch(tag)
	if m == nil {
		// Nie powinno się zdarzyć: describe wybiera tylko spośród podanych.
		return "", fmt.Errorf("git describe zwrócił %q, a to nie jest tag wydania", tag)
	}
	distance, err := r.git(ctx, "rev-list", "--count", tag+"..HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-list --count %s..HEAD: %w", tag, err)
	}
	if distance == "0" && dirty == "" {
		return fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3]), nil
	}
	patch, err := strconv.Atoi(m[3])
	if err != nil {
		return "", fmt.Errorf("numer łatki w %s: %w", tag, err)
	}
	return fmt.Sprintf("%s.%s.%d-dev.%s+%s%s", m[1], m[2], patch+1, distance, sha, dirty), nil
}

// releaseTags zwraca tagi wydań osiągalne z HEAD.
func (r Repo) releaseTags(ctx context.Context) ([]string, error) {
	out, err := r.git(ctx, "tag", "--list", "v*", "--merged", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("git tag --list: %w", err)
	}
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		if tag := strings.TrimSpace(line); releaseTag.MatchString(tag) {
			tags = append(tags, tag)
		}
	}
	return tags, nil
}

// nearestTag wybiera spośród tagów wydań ten, który git describe uznaje za
// najbliższy HEAD. Jedno wywołanie z dokładnymi nazwami (--match) zamiast
// pętli z --exclude: nazwy wydań nie mają znaków wzorców glob, więc pasują
// tylko same do siebie, a odrzucone tagi w ogóle nie są kandydatami.
func (r Repo) nearestTag(ctx context.Context, tags []string) (string, error) {
	args := []string{"describe", "--tags", "--abbrev=0"}
	for _, tag := range tags {
		args = append(args, "--match", tag)
	}
	args = append(args, "HEAD")
	tag, err := r.git(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("git describe: %w", err)
	}
	return tag, nil
}

// SourceDateEpoch zwraca znacznik czasu builda według konwencji
// reproducible-builds.org: czas ostatniego commita, a nie chwila budowania.
// Ten sam commit zbudowany dziś i za miesiąc daje wtedy ten sam obraz,
// a przebudowa nie wywołuje rolloutu, w którym nic się nie zmieniło.
//
// Brudne drzewo dostaje bieżący czas: jego zawartość nie odpowiada żadnemu
// commitowi, więc nie ma czego odtwarzać, a wersja i tak niesie -dirty.
func (r Repo) SourceDateEpoch(ctx context.Context) (int64, error) {
	status, err := r.Status(ctx)
	if err != nil {
		return 0, err
	}
	if status != "" {
		now := time.Now
		if r.Now != nil {
			now = r.Now
		}
		return now().Unix(), nil
	}
	out, err := r.git(ctx, "log", "-1", "--format=%ct")
	if err != nil {
		return 0, fmt.Errorf("git log: %w", err)
	}
	epoch, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("czas commita %q: %w", out, err)
	}
	return epoch, nil
}

// SourceDate zamienia znacznik czasu na zapis ISO 8601 w UTC, jak w
// version.json i etykiecie org.opencontainers.image.created.
func SourceDate(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// ErrDirtyTree — drzewo robocze ma niezacommitowane zmiany.
var ErrDirtyTree = errors.New("drzewo robocze jest brudne")

// RequireClean odrzuca brudne drzewo. Obraz zbudowany z niezacommitowanych
// zmian nie da się odtworzyć z commita, który podaje /version — czyli
// /version kłamie. Wynik negatywny (kod 1), z listą zmian.
func (r Repo) RequireClean(ctx context.Context) error {
	status, err := r.Status(ctx)
	if err != nil {
		return err
	}
	if status != "" {
		return cli.Unmet("%v — build wydania odrzucony:\n%s", ErrDirtyTree, status)
	}
	return nil
}

// DockerTag zamienia wersję na tag obrazu. SemVer dopuszcza „+" w metadanych
// builda, tag w rejestrze nie — każdy znak spoza [A-Za-z0-9._-] staje się
// myślnikiem. Tożsamość w version.json zostaje pełna; to tylko etykieta.
func DockerTag(version string) string {
	var b strings.Builder
	for _, c := range version {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	tag := b.String()
	if len(tag) > DockerTagMaxLength {
		tag = tag[:DockerTagMaxLength]
	}
	return tag
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
