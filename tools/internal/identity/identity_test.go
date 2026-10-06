package identity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
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
	return r.git("rev-parse", "HEAD")[:ShortCommitLength]
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
	if date := SourceDate(got); date != "2026-10-01T12:00:01Z" {
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

func TestDockerTag(t *testing.T) {
	long := strings.Repeat("a", 200)
	tests := []struct {
		version, want string
	}{
		{"0.3.0", "0.3.0"},
		{"0.3.1-dev.2+abc1234", "0.3.1-dev.2-abc1234"},
		{"0.3.1-dev.0+abc1234-dirty", "0.3.1-dev.0-abc1234-dirty"},
		{"1.0.0+żółć", "1.0.0-----"},
		{long, long[:DockerTagMaxLength]},
	}
	for _, tt := range tests {
		if got := DockerTag(tt.version); got != tt.want {
			t.Errorf("DockerTag(%q) = %q, oczekiwano %q", tt.version, got, tt.want)
		}
	}
}

func TestImageName(t *testing.T) {
	if got := ImageName("MateuszF757", "api"); got != "ghcr.io/mateuszf757/docfind-api" {
		t.Errorf("ImageName = %q", got)
	}
	if got := ImageName("", "api"); got != "ghcr.io/mateuszf757/docfind-api" {
		t.Errorf("ImageName bez właściciela = %q", got)
	}
}
