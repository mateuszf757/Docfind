// Package git zbiera z repozytorium gita fakty o commicie — status, HEAD,
// tagi wydań, odległość od tagu, czas commita — z których core/artifact
// wylicza tożsamość artefaktu (decyzje 21, 30 i 40), i zakłada tagi wydań.
//
// To jedyne miejsce, które pyta gita „co to za wersja i z jakiego commita".
// /version, etykiety OCI i tagi w rejestrze biorą odpowiedź stąd — gdyby
// każde pytało gita po swojemu, /version zacząłby kłamać, a cała diagnostyka
// stoi na tym endpoincie.
package git

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

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
	if len(commit) < artifact.ShortCommitLength {
		return "", fmt.Errorf("nieoczekiwany identyfikator commita %q", commit)
	}
	return commit, nil
}

// Version zwraca wersję drzewa roboczego: fakty z gita, reguła z
// core/artifact (decyzja 30). Brudne drzewo dostaje przyrostek -dirty.
//
// Tagi spoza postaci vX.Y.Z (v0.4.0-rc.1, literówka) są pomijane, a nie
// zatrzymują buildu: tagów v* nie da się usunąć (ruleset), więc jeden zły
// tag zatrzymałby każdy kolejny build na zawsze.
func (r Repo) Version(ctx context.Context) (string, error) {
	status, err := r.Status(ctx)
	if err != nil {
		return "", err
	}
	commit, err := r.Commit(ctx)
	if err != nil {
		return "", err
	}
	return r.version(ctx, artifact.Source{Commit: commit, Dirty: status != ""}, "HEAD")
}

// VersionOf zwraca wersję commita — tę, którą miałoby czyste drzewo na nim.
// Obraz nie niesie wersji (decyzja 40); wdrożenie obrazu z rejestru podaje
// ją aplikacji samo, wyliczoną z historii tego repozytorium.
func (r Repo) VersionOf(ctx context.Context, commit string) (string, error) {
	full, err := r.git(ctx, "rev-parse", "--verify", "--quiet", commit+"^{commit}")
	if err != nil {
		return "", cli.Unmet("commitu %s nie ma w lokalnym repozytorium — git fetch origin", artifact.ShortCommit(commit))
	}
	return r.version(ctx, artifact.Source{Commit: full}, full)
}

func (r Repo) version(ctx context.Context, src artifact.Source, rev string) (string, error) {
	tags, err := r.releaseTags(ctx, rev)
	if err != nil {
		return "", err
	}
	if len(tags) == 0 {
		if src.Commits, err = r.count(ctx, rev); err != nil {
			return "", err
		}
		return artifact.Version(src)
	}
	if src.Tag, err = r.nearestTag(ctx, tags, rev); err != nil {
		return "", err
	}
	if !artifact.IsReleaseTag(src.Tag) {
		// Nie powinno się zdarzyć: describe wybiera tylko spośród podanych.
		return "", fmt.Errorf("git describe zwrócił %q, a to nie jest tag wydania", src.Tag)
	}
	if src.Distance, err = r.count(ctx, src.Tag+".."+rev); err != nil {
		return "", err
	}
	return artifact.Version(src)
}

// LatestRelease zwraca numer najbliższego wydania osiągalnego z commita;
// pusty — commit nie ma wydań w historii.
func (r Repo) LatestRelease(ctx context.Context, commit string) (string, error) {
	tags, err := r.releaseTags(ctx, commit)
	if err != nil || len(tags) == 0 {
		return "", err
	}
	tag, err := r.nearestTag(ctx, tags, commit)
	if err != nil {
		return "", err
	}
	version, _ := artifact.ReleaseOfTag(tag)
	return version, nil
}

// TagExists mówi, czy tag istnieje w lokalnym repozytorium.
func (r Repo) TagExists(ctx context.Context, name string) (bool, error) {
	_, err := r.git(ctx, "rev-parse", "--verify", "--quiet", "refs/tags/"+name)
	switch {
	case err == nil:
		return true, nil
	case proc.ExitCode(err) > 0:
		return false, nil
	default:
		return false, err
	}
}

// CreateTag tworzy tag z adnotacją na commicie. Treść idzie przez stdin,
// nie argumenty — wieloliniowa i bez cytowania dla powłoki.
func (r Repo) CreateTag(ctx context.Context, name, commit, message string) error {
	_, err := r.Runner.Run(ctx, proc.Cmd{
		Name:  "git",
		Args:  []string{"-C", r.Dir, "tag", "--annotate", "--file=-", "--cleanup=verbatim", name, commit},
		Stdin: strings.NewReader(message),
	})
	if err != nil {
		return fmt.Errorf("git tag %s: %w", name, err)
	}
	return nil
}

// count zwraca liczbę commitów w zakresie (`git rev-list --count`).
func (r Repo) count(ctx context.Context, rng string) (int, error) {
	out, err := r.git(ctx, "rev-list", "--count", rng)
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count %s: %w", rng, err)
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("git rev-list --count %s: %q nie jest liczbą", rng, out)
	}
	return n, nil
}

// releaseTags zwraca tagi wydań osiągalne z rev.
func (r Repo) releaseTags(ctx context.Context, rev string) ([]string, error) {
	out, err := r.git(ctx, "tag", "--list", "v*", "--merged", rev)
	if err != nil {
		return nil, fmt.Errorf("git tag --list: %w", err)
	}
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		if tag := strings.TrimSpace(line); artifact.IsReleaseTag(tag) {
			tags = append(tags, tag)
		}
	}
	return tags, nil
}

// nearestTag wybiera spośród tagów wydań ten, który git describe uznaje za
// najbliższy rev. Jedno wywołanie z dokładnymi nazwami (--match) zamiast
// pętli z --exclude: nazwy wydań nie mają znaków wzorców glob, więc pasują
// tylko same do siebie, a odrzucone tagi w ogóle nie są kandydatami.
func (r Repo) nearestTag(ctx context.Context, tags []string, rev string) (string, error) {
	args := []string{"describe", "--tags", "--abbrev=0"}
	for _, tag := range tags {
		args = append(args, "--match", tag)
	}
	args = append(args, rev)
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
// commitowi, więc nie ma czego odtwarzać, a tag obrazu i wersja niosą
// -dirty.
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

// ErrDirtyTree — drzewo robocze ma niezacommitowane zmiany.
var ErrDirtyTree = errors.New("drzewo robocze jest brudne")

// Dirty mówi, czy drzewo robocze ma zmiany.
func (r Repo) Dirty(ctx context.Context) (bool, error) {
	status, err := r.Status(ctx)
	return status != "", err
}

// RequireClean odrzuca brudne drzewo. Obraz zbudowany z niezacommitowanych
// zmian nie da się odtworzyć z commita, który podaje /version — czyli
// /version kłamie. Wynik negatywny (kod 1), z listą zmian.
func (r Repo) RequireClean(ctx context.Context) error {
	status, err := r.Status(ctx)
	if err != nil {
		return err
	}
	if status != "" {
		return cli.Unmet("%v:\n%s", ErrDirtyTree, status)
	}
	return nil
}
