package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// testRepo to prawdziwe repozytorium gita w katalogu tymczasowym. Wersja
// z gita trafia do /version, etykiet OCI i tagów w rejestrze, a błąd w jej
// logice nie psuje żadnego testu aplikacji — wychodzi dopiero jako obraz
// z cudzą wersją. Dlatego bez atrap gita.
type testRepo struct {
	t    *testing.T
	dir  string
	tick int
}

// newTestRepo zakłada repozytorium odcięte od konfiguracji gita autora:
// globalne tag.gpgSign, commit.gpgSign, core.abbrev albo
// status.showUntrackedFiles zmieniłyby przebieg testu.
func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("brak git w PATH: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.invalid")
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git("init", "--quiet", "--initial-branch=main")
	return r
}

func (r *testRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit tworzy pusty commit z kolejnym, przewidywalnym czasem — czas
// commita to SOURCE_DATE_EPOCH obrazu.
func (r *testRepo) commit(msg string) {
	r.t.Helper()
	r.tick++
	date := time.Date(2026, 10, 1, 12, 0, r.tick, 0, time.UTC).Format(time.RFC3339)
	r.t.Setenv("GIT_AUTHOR_DATE", date)
	r.t.Setenv("GIT_COMMITTER_DATE", date)
	r.git("commit", "--quiet", "--allow-empty", "-m", msg)
}

func (r *testRepo) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) remove(name string) {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.dir, name)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *testRepo) sha() string {
	r.t.Helper()
	return r.git("rev-parse", "HEAD")[:artifact.ShortCommitLength]
}

func (r *testRepo) repo() Repo {
	return Repo{Dir: r.dir, Runner: proc.Exec{}}
}

// TestVersion przechodzi przez historię repozytorium krok po kroku, jak
// rośnie prawdziwe: commity, tag wydania, praca za tagiem, tagi w złej
// postaci. Każdy krok sprawdza wersję po zmianie.
func TestVersion(t *testing.T) {
	r := newTestRepo(t)
	steps := []struct {
		name    string
		prepare func()
		want    func() string
	}{
		{
			name:    "bez tagu",
			prepare: func() { r.commit("pierwszy"); r.commit("drugi") },
			want:    func() string { return "0.0.0-dev.2+" + r.sha() },
		},
		{
			name:    "bez tagu, plik nieśledzony",
			prepare: func() { r.write("zmiana", "x") },
			want:    func() string { return "0.0.0-dev.2+" + r.sha() + "-dirty" },
		},
		{
			name: "na tagu wydania",
			prepare: func() {
				r.remove("zmiana")
				r.git("tag", "--annotate", "v0.3.0", "-m", "Etap 3")
			},
			want: func() string { return "0.3.0" },
		},
		{
			// Błąd poprzedniej wersji df_version (decyzja 30): git describe
			// --dirty nie widzi plików nieśledzonych i dawał „czyste wydanie".
			name:    "na tagu, plik nieśledzony",
			prepare: func() { r.write("zmiana", "x") },
			want:    func() string { return "0.3.1-dev.0+" + r.sha() + "-dirty" },
		},
		{
			name: "na tagu, zmiana w indeksie",
			prepare: func() {
				r.git("add", "zmiana")
			},
			want: func() string { return "0.3.1-dev.0+" + r.sha() + "-dirty" },
		},
		{
			// Za tagiem wersja przedpremierowa następnej łatki: w porządku
			// SemVer powyżej 0.3.0, a surowe git describe (0.3.0-2-g…)
			// stałoby poniżej.
			name: "dwa commity za tagiem",
			prepare: func() {
				r.git("commit", "--quiet", "-m", "trzeci")
				r.commit("czwarty")
			},
			want: func() string { return "0.3.1-dev.2+" + r.sha() },
		},
		{
			name:    "zmiana w śledzonym pliku",
			prepare: func() { r.write("zmiana", "y") },
			want:    func() string { return "0.3.1-dev.2+" + r.sha() + "-dirty" },
		},
		{
			// Tagi bliżej niż wydanie, ale w innej postaci: pominięte, a nie
			// błąd — tagów v* nie da się usunąć, więc błąd zatrzymałby
			// buildy na zawsze. v01.2.3 ma zero wiodące, którego SemVer nie
			// dopuszcza.
			name: "tagi spoza postaci vX.Y.Z pominięte",
			prepare: func() {
				r.git("checkout", "--quiet", "--", "zmiana")
				for _, tag := range []string{"v0.4.0-rc.1", "v1.2.3.4", "v01.2.3", "vfoo", "0.5.0"} {
					r.git("tag", tag)
				}
				r.commit("piąty")
			},
			want: func() string { return "0.3.1-dev.3+" + r.sha() },
		},
		{
			name:    "tag lekki też wyznacza wydanie",
			prepare: func() { r.git("tag", "v0.4.0") },
			want:    func() string { return "0.4.0" },
		},
		{
			name: "plik ignorowany nie brudzi drzewa",
			prepare: func() {
				r.write(".gitignore", "*.log\n")
				r.git("add", ".gitignore")
				r.commit("ignorowane")
				r.write("build.log", "x")
			},
			want: func() string { return "0.4.1-dev.1+" + r.sha() },
		},
	}
	for _, step := range steps {
		step.prepare()
		got, err := r.repo().Version(context.Background())
		if err != nil {
			t.Fatalf("%s: Version: %v", step.name, err)
		}
		if want := step.want(); got != want {
			t.Errorf("%s: wersja %q, oczekiwano %q", step.name, got, want)
		}
	}
}

// TestVersionMergedReleaseBranch: tag wydania na gałęzi scalonej merge
// commitem jest osiągalny z HEAD, a odległość liczy wszystkie commity spoza
// historii tagu — z obu rodziców.
func TestVersionMergedReleaseBranch(t *testing.T) {
	r := newTestRepo(t)
	r.commit("start")
	r.git("switch", "--quiet", "-c", "wydanie")
	r.commit("poprawka")
	r.git("tag", "--annotate", "v1.0.0", "-m", "wydanie")
	r.git("switch", "--quiet", "main")
	r.commit("praca na main")
	r.git("merge", "--quiet", "--no-ff", "-m", "scalenie", "wydanie")

	got, err := r.repo().Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Za tagiem: „praca na main" i commit scalający.
	if want := "1.0.1-dev.2+" + r.sha(); got != want {
		t.Errorf("wersja %q, oczekiwano %q", got, want)
	}
}

// TestVersionNearestOfTwoReleases: z dwóch wydań w historii wygrywa
// bliższe, nie wyższe numerem.
func TestVersionNearestOfTwoReleases(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")
	r.git("tag", "v0.9.0")
	r.commit("b")
	r.git("tag", "v0.2.0")
	r.commit("c")

	got, err := r.repo().Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "0.2.1-dev.1+" + r.sha(); got != want {
		t.Errorf("wersja %q, oczekiwano %q", got, want)
	}
}

// TestVersionIgnoresUserUntrackedConfig: konfiguracja autora nie może ukryć
// plików nieśledzonych przed oceną brudu.
func TestVersionIgnoresUserUntrackedConfig(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")
	r.git("tag", "v0.3.0")
	r.git("config", "status.showUntrackedFiles", "no")
	r.git("config", "core.abbrev", "12")
	r.write("nowy", "x")

	got, err := r.repo().Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "0.3.1-dev.0+" + r.sha() + "-dirty"; got != want {
		t.Errorf("wersja %q, oczekiwano %q", got, want)
	}
}

func TestVersionOutsideRepository(t *testing.T) {
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	repo := Repo{Dir: t.TempDir(), Runner: proc.Exec{}}
	_, err := repo.Version(context.Background())
	if err == nil {
		t.Fatal("oczekiwano błędu poza repozytorium")
	}
	// Brak repozytorium to awaria narzędzia (kod 2), nie wynik negatywny.
	if code := cli.ExitCode(err); code != cli.ExitFailure {
		t.Errorf("kod wyjścia %d, oczekiwano %d (%v)", code, cli.ExitFailure, err)
	}
}

func TestSourceDateEpoch(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")
	repo := r.repo()
	fixedNow := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	repo.Now = func() time.Time { return fixedNow }

	got, err := repo.SourceDateEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	commitTime := time.Date(2026, 10, 1, 12, 0, 1, 0, time.UTC)
	if got != commitTime.Unix() {
		t.Errorf("czyste drzewo: %d, oczekiwano czasu commita %d", got, commitTime.Unix())
	}
	if date := artifact.SourceDate(got); date != "2026-10-01T12:00:01Z" {
		t.Errorf("SourceDate = %q", date)
	}

	r.write("zmiana", "x")
	got, err = repo.SourceDateEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != fixedNow.Unix() {
		t.Errorf("brudne drzewo: %d, oczekiwano bieżącego czasu %d", got, fixedNow.Unix())
	}
}

func TestRequireClean(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")
	repo := r.repo()
	if err := repo.RequireClean(context.Background()); err != nil {
		t.Fatalf("czyste drzewo odrzucone: %v", err)
	}

	r.write("zmiana", "x")
	err := repo.RequireClean(context.Background())
	// Brudne drzewo to wynik negatywny (kod 1), nie awaria narzędzia.
	if code := cli.ExitCode(err); code != cli.ExitUnmet {
		t.Errorf("kod wyjścia %d, oczekiwano %d (%v)", code, cli.ExitUnmet, err)
	}
	if err == nil || !strings.Contains(err.Error(), "?? zmiana") {
		t.Errorf("oczekiwano listy zmian w błędzie, jest %v", err)
	}
}

// TestVersionOfAndReleases: wersja dowolnego commita z historii — tak
// wdrożenie podaje wersję obrazowi z rejestru, który jej nie niesie
// (decyzja 40) — i najbliższe wydanie osiągalne z commita.
func TestVersionOfAndReleases(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")
	repo := r.repo()
	ctx := context.Background()
	if got, err := repo.LatestRelease(ctx, "HEAD"); err != nil || got != "" {
		t.Errorf("bez wydań: %q, %v", got, err)
	}
	r.git("tag", "--annotate", "v0.3.0", "-m", "Etap 3")
	r.commit("b")
	built := r.git("rev-parse", "HEAD")
	r.commit("c")
	r.write("brud", "x")

	// Brud drzewa nie dotyczy commita z historii.
	if got, err := repo.VersionOf(ctx, built); err != nil || got != "0.3.1-dev.1+"+built[:artifact.ShortCommitLength] {
		t.Errorf("wersja commita: %q, %v", got, err)
	}
	if got, err := repo.LatestRelease(ctx, built); err != nil || got != "0.3.0" {
		t.Errorf("ostatnie wydanie: %q, %v", got, err)
	}
	if _, err := repo.VersionOf(ctx, strings.Repeat("0", 40)); cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "git fetch") {
		t.Errorf("nieznany commit: %v", err)
	}
	if dirty, err := repo.Dirty(ctx); err != nil || !dirty {
		t.Errorf("brudne drzewo: %v, %v", dirty, err)
	}
}

// TestCreateTag: tag z adnotacją na wskazanym commicie, z treścią zapisaną
// dosłownie — CI czyta z niej digest wydania.
func TestCreateTag(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")
	target := r.git("rev-parse", "HEAD")
	r.commit("b")
	repo := r.repo()
	ctx := context.Background()
	if ok, err := repo.TagExists(ctx, "v0.4.0"); err != nil || ok {
		t.Fatalf("tag przed utworzeniem: %v, %v", ok, err)
	}
	message := "Etap 4\n\nAkceptacja na stagingu: ok\n\nImage: x@sha256:abc\n"
	if err := repo.CreateTag(ctx, "v0.4.0", target, message); err != nil {
		t.Fatal(err)
	}
	if ok, err := repo.TagExists(ctx, "v0.4.0"); err != nil || !ok {
		t.Errorf("tag po utworzeniu: %v, %v", ok, err)
	}
	if typ := r.git("cat-file", "-t", "v0.4.0"); typ != "tag" {
		t.Errorf("tag %s zamiast z adnotacją", typ)
	}
	if got := r.git("rev-parse", "v0.4.0^{commit}"); got != target {
		t.Errorf("tag na %s, oczekiwano %s", got, target)
	}
	if got := r.git("tag", "--list", "--format=%(contents)", "v0.4.0"); got != strings.TrimSpace(message) {
		t.Errorf("treść tagu:\n%q\noczekiwano:\n%q", got, message)
	}
	if err := repo.CreateTag(ctx, "v0.4.0", target, message); err == nil {
		t.Error("drugi tag o tej samej nazwie przyjęty")
	}
}

// TestExport: kod katalogu z commita, a nie z drzewa roboczego, tylko ten
// katalog i ze ścieżkami względem niego; rev, który wygląda jak opcja,
// nie trafia do git archive; dowiązanie symboliczne zatrzymuje wypakowanie.
func TestExport(t *testing.T) {
	r := newTestRepo(t)
	put := func(name, content string) {
		t.Helper()
		path := filepath.Join(r.dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("README.md", "poza modułem")
	put("tools/go.mod", "module example.com/tools\n")
	put("tools/cmd/dft/main.go", "package main // v1\n")
	r.git("add", ".")
	r.commit("baza")
	base := r.git("rev-parse", "HEAD")
	put("tools/cmd/dft/main.go", "package main // v2\n")
	put("tools/nowy.go", "package tools\n")
	r.git("add", ".")
	r.commit("zmiana")
	put("tools/go.mod", "module example.com/brudne\n")

	tools := Repo{Dir: filepath.Join(r.dir, "tools"), Runner: proc.Exec{}}
	ctx := context.Background()
	dir := t.TempDir()
	if err := tools.Export(ctx, base, dir); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"go.mod": "module example.com/tools\n", "cmd/dft/main.go": "package main // v1\n"} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Errorf("%s: %q, %v; oczekiwano %q", name, got, err, want)
		}
	}
	for _, absent := range []string{"nowy.go", "README.md", "tools"} {
		if _, err := os.Stat(filepath.Join(dir, absent)); err == nil {
			t.Errorf("%s w kodzie z commita bazowego", absent)
		}
	}

	for _, rev := range []string{strings.Repeat("0", 40), "--output=" + filepath.Join(dir, "x.tar")} {
		if err := tools.Export(ctx, rev, t.TempDir()); !errors.Is(err, artifact.ErrUnknownCommit) {
			t.Errorf("rev %q: %v, oczekiwano ErrUnknownCommit", rev, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "x.tar")); err == nil {
		t.Error("rev w postaci opcji trafił do git archive")
	}

	if err := os.Symlink("../README.md", filepath.Join(r.dir, "tools", "link")); err != nil {
		t.Fatal(err)
	}
	r.git("add", "tools/link")
	r.commit("dowiązanie")
	if err := tools.Export(ctx, "HEAD", t.TempDir()); err == nil || !strings.Contains(err.Error(), "tylko pliki i katalogi") {
		t.Errorf("dowiązanie symboliczne wypakowane: %v", err)
	}
}

// TestShallow: płytki klon (checkout z fetch-depth) rozpoznany — skan
// historii widziałby w nim tylko jej część.
func TestShallow(t *testing.T) {
	r := newTestRepo(t)
	r.commit("pierwszy")
	r.commit("drugi")
	ctx := context.Background()
	if shallow, err := r.repo().Shallow(ctx); err != nil || shallow {
		t.Fatalf("pełne repozytorium: płytkie=%v, %v", shallow, err)
	}
	clone := filepath.Join(t.TempDir(), "klon")
	if out, err := exec.Command("git", "clone", "--quiet", "--depth", "1", "file://"+r.dir, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone --depth 1: %v\n%s", err, out)
	}
	if shallow, err := (Repo{Dir: clone, Runner: proc.Exec{}}).Shallow(ctx); err != nil || !shallow {
		t.Errorf("płytki klon: płytkie=%v, %v", shallow, err)
	}
}
