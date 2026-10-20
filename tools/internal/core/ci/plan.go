package ci

import (
	"path"
	"slices"
	"strings"
)

// Profile — profil integracji zmiany (decyzja 75): ile z warstwy klastra PR
// ma przejść, zanim wejdzie na main.
type Profile string

// Profile w kolejności od najlżejszego.
const (
	// ProfileFast — sam Fast: zmiana nie dotyka niczego, co sprawdza klaster.
	ProfileFast Profile = "fast"
	// ProfileMedium — klaster z bramkami dymnymi: zmiana aplikacji albo jej
	// zależności.
	ProfileMedium Profile = "medium"
	// ProfileHeavy — klaster ze wszystkimi bramkami: platforma, charty,
	// środowiska, CI, przypięcia albo kod bramek klastrowych.
	ProfileHeavy Profile = "heavy"
)

func (p Profile) rank() int {
	return slices.Index([]Profile{ProfileFast, ProfileMedium, ProfileHeavy}, p)
}

// Plan — profil zmiany i pliki, które wyznaczyły każdy profil.
type Plan struct {
	Profile Profile              `json:"profile"`
	Files   map[Profile][]string `json:"files"`
}

// ClusterRootFiles — pliki tools/cmd/dft, których importy są korzeniami
// grafu pakietów klastrowych: polecenia uruchamiane przez zadanie cluster
// i budowa adapterów (wiring.go) — polecenia sięgają po adaptery (k3d, Helm,
// Docker) metodami workspace, a nie własnymi importami. Graf jest przez to
// szerszy niż konieczne, ale nie węższy.
var ClusterRootFiles = []string{"cluster.go", "gitops.go", "secrets.go", "wiring.go"}

// clusterCommandFiles — pliki tools/cmd/dft, których zmiana to heavy:
// korzenie grafu i dyspozytory poleceń i bramek (main.go, quality.go).
var clusterCommandFiles = append(slices.Clone(ClusterRootFiles), "main.go", "quality.go")

var (
	fastPrefixes   = []string{"docs/", ".github/rulesets/", ".github/environments/", "tests/corpus/"}
	fastFiles      = []string{".github/dependabot.yml", "ci/contract-exceptions.yaml", "ci/vuln-exceptions.yaml", "ci/gitleaks.toml", "ci/gitleaks-ignore", ".gitignore", "LICENSE"}
	mediumPrefixes = []string{"services/api/", "deploy/config/"}
	heavyPrefixes  = []string{"deploy/charts/", "deploy/platform/", "deploy/environments/", "deploy/k3d/", ".github/workflows/", ".github/actions/", "bin/", "tools/cmd/drain-probe/"}
	heavyFiles     = []string{"ci/pins.env", "ci/dft", "mise.lock", "tools/go.mod", "tools/go.sum"}
)

// Classify — profil jednego pliku (ścieżka od korzenia repozytorium).
// clusterDirs — katalogi pakietów tools/ w grafie zależności poleceń
// klastrowych (stages/plan). Nieznana ścieżka to heavy: reguła ma
// przepuszczać tylko to, co zna.
func Classify(file string, clusterDirs map[string]bool) Profile {
	base := path.Base(file)
	switch {
	case slices.Contains(fastFiles, file), hasPrefix(file, fastPrefixes), strings.HasSuffix(file, ".md"):
		return ProfileFast
	case hasPrefix(file, mediumPrefixes):
		return ProfileMedium
	case slices.Contains(heavyFiles, file), hasPrefix(file, heavyPrefixes),
		strings.HasPrefix(file, "mise.") && strings.HasSuffix(file, ".toml"):
		return ProfileHeavy
	case strings.HasPrefix(file, "tools/") && (strings.HasSuffix(file, "_test.go") || strings.Contains(file, "/testdata/")):
		// Testy biegną w Fast i nie zmieniają zachowania bramek.
		return ProfileFast
	case path.Dir(file) == "tools/cmd/dft":
		if slices.Contains(clusterCommandFiles, base) {
			return ProfileHeavy
		}
		return ProfileFast
	case strings.HasPrefix(file, "tools/internal/"):
		if parts := strings.SplitN(file, "/", 5); len(parts) == 5 && !clusterDirs[strings.Join(parts[:4], "/")] {
			return ProfileFast
		}
		return ProfileHeavy
	}
	return ProfileHeavy
}

// PlanFor — profil zmiany: najcięższy z profili jej plików. Pusta zmiana to
// fast.
func PlanFor(changed []string, clusterDirs map[string]bool) Plan {
	p := Plan{Profile: ProfileFast, Files: map[Profile][]string{}}
	for _, f := range changed {
		profile := Classify(f, clusterDirs)
		p.Files[profile] = append(p.Files[profile], f)
		if profile.rank() > p.Profile.rank() {
			p.Profile = profile
		}
	}
	return p
}

func hasPrefix(file string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(file, p) })
}
