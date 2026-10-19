// Package versions sprawdza spójność wersji zapisanych w miejscach, których
// żaden automat nie synchronizuje: mise.toml, ci/pins.env, Dockerfile,
// .python-version i tools/go.mod. Rozjazd ma wyjść w zadaniu test, a nie jako
// obraz na innym Pythonie niż testy albo kubectl innym niż klaster.
package versions

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

// Tools — lokalne programy, których wersje się porównuje (adapters/kube,
// adapters/uv).
type Tools struct {
	Kubectl interface {
		ClientVersion(ctx context.Context) (string, error)
	}
	UV interface {
		Version(ctx context.Context) (string, error)
	}
}

// Check porównuje wersje w repozytorium o korzeniu root. goVersion to wersja
// Go, którą zbudowano program (runtime.Version()) — przy GOTOOLCHAIN=local
// to wersja z mise.toml. clientGo to wersja k8s.io/client-go wkompilowana
// w program (z debug.ReadBuildInfo); pusta — sprawdzenie pominięte.
func Check(ctx context.Context, root string, tools Tools, goVersion, clientGo string) error {
	p, err := pins.Load(root)
	if err != nil {
		return err
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, pins.Dockerfile))
	if err != nil {
		return err
	}
	v := &cli.Verdict{}

	// Go z mise ↔ dyrektywa go w go.mod. Przy GOTOOLCHAIN=local starszy
	// toolchain niż w go.mod nie zbuduje programu, więc tu zostaje przypadek
	// odwrotny: mise podbite, go.mod nie.
	gomod, err := os.ReadFile(filepath.Join(root, pins.GoMod))
	if err != nil {
		return err
	}
	directive, err := pins.GoDirective(gomod)
	if err != nil {
		return err
	}
	if goVersion != "go"+directive {
		v.Fail("program zbudowany przez %s, a %s wymaga go %s — podbij oba razem (mise.toml i go mod edit -go)", goVersion, pins.GoMod, directive)
	} else {
		cli.Step("Go %s: mise.toml i %s zgodne", directive, pins.GoMod)
	}

	// kubectl z mise.toml ↔ Kubernetes klastra i schematów kubeconform.
	kubectl, err := tools.Kubectl.ClientVersion(ctx)
	if err != nil {
		return err
	}
	if want := "v" + p.Get(pins.KubernetesVersion); kubectl != want {
		v.Fail("kubectl %s (mise.toml), a klaster i schematy to %s (%s)", kubectl, want, pins.File)
	} else {
		cli.Step("kubectl %s zgodny z klastrem", kubectl)
	}

	// client-go v0.X ↔ Kubernetes 1.X klastra. Biblioteka gwarantuje
	// zgodność z klastrem tej samej wersji minor; aktualizacja k3s bez
	// podbicia client-go (albo odwrotnie) wychodzi tutaj, a nie jako pole,
	// którego bramka nie widzi.
	if clientGo != "" {
		if want := clientGoMinor(p.Get(pins.KubernetesVersion)); !strings.HasPrefix(clientGo, want) {
			v.Fail("k8s.io/client-go %s, a klaster to Kubernetes %s — oczekiwano %sx (tools/go.mod)", clientGo, p.Get(pins.KubernetesVersion), want)
		} else {
			cli.Step("client-go %s zgodny z klastrem", clientGo)
		}
	}

	// Python: .python-version (testy) ↔ tag bazy w Dockerfile (produkcja).
	// Nowa wersja bazy od Dependabota zatrzyma się tutaj, dopóki testy nie
	// przejdą na nią.
	pyFile, err := os.ReadFile(filepath.Join(root, pins.PythonVersionPath))
	if err != nil {
		return err
	}
	python, err := pins.PythonVersion(pyFile)
	if err != nil {
		return err
	}
	if minors := pins.PythonMinors(dockerfile); len(minors) != 1 || minors[0] != python {
		v.Fail("testy biegną na Pythonie %s (%s), a obraz na %s (%s)", python, pins.PythonVersionPath, strings.Join(minors, ", "), pins.Dockerfile)
	} else {
		cli.Step("Python %s: testy i obraz zgodne", python)
	}

	// uv: lokalny ↔ Dockerfile. W CI równe z konstrukcji — workflow instaluje
	// wersję odczytaną z Dockerfile — więc lokalnie wystarczy ostrzeżenie.
	// Twardy błąd zatrzymywałby pracę po każdej łatce uv od Dependabota.
	uvWant, err := pins.UVVersion(dockerfile)
	if err != nil {
		return err
	}
	uv, err := tools.UV.Version(ctx)
	if err != nil {
		return err
	}
	if uv != uvWant {
		cli.Warn("lokalny uv %s, a Dockerfile i CI używają uv %s — uv self update %s", uv, uvWant, uvWant)
	}

	return v.Err("spójność wersji")
}

// clientGoMinor zamienia wersję Kubernetesa 1.X.Y na przedrostek wersji
// client-go v0.X.
func clientGoMinor(kubernetes string) string {
	parts := strings.Split(kubernetes, ".")
	if len(parts) < 2 {
		return "v0." + kubernetes
	}
	return "v0." + parts[1] + "."
}

// Otoczenie, które mise ustawia z mise.toml [env] (decyzja 64). Inna
// wartość w CI to nadpisanie z zewnątrz: GOPROXY z „direct” pobiera moduły
// prosto z repozytoriów kodu, GONOSUMDB i GOINSECURE wyłączają sprawdzanie
// sum, GOFLAGS zmienia każde wywołanie `go`, MISE_VERSION
// i MISE_INSTALL_PATH podmieniają przypięty launcher (bin/mise).
var (
	MiseEnv = map[string]string{
		"GOPROXY":     "https://proxy.golang.org",
		"GOTOOLCHAIN": "local",
	}
	forbiddenEnv = []string{"GOFLAGS", "GONOSUMDB", "GONOSUMCHECK", "GOINSECURE", "GOPRIVATE", "GONOPROXY", "MISE_VERSION"}
	// launcherVersion — przypięta wersja mise w bin/mise.
	launcherVersion = regexp.MustCompile(`MISE_VERSION:-v?([0-9]+\.[0-9]+\.[0-9]+)`)
)

// LauncherVersion — wersja mise przypięta w bin/mise.
func LauncherVersion(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "bin", "mise"))
	if err != nil {
		return "", err
	}
	m := launcherVersion.FindSubmatch(raw)
	if m == nil {
		return "", errors.New("bin/mise bez przypiętej wersji (MISE_VERSION:-X.Y.Z)")
	}
	return string(m[1]), nil
}

// Environment — nadpisania otoczenia narzędzi: zmienne z mise.toml
// o innej wartości, zmienne zakazane, baza sum inna niż sum.golang.org
// i katalogi mise inne niż te, które ustawia bin/mise w wersji launcher
// (pusta — bez porównania katalogów).
func Environment(getenv func(string) string, root, launcher string) []string {
	var found []string
	if launcher != "" {
		dir := filepath.Join(root, ".mise")
		for key, want := range map[string]string{"MISE_DATA_DIR": dir, "MISE_INSTALL_PATH": filepath.Join(dir, "mise-"+launcher)} {
			if got := getenv(key); got != "" && got != want {
				found = append(found, fmt.Sprintf("%s=%q, a bin/mise ustawia %q", key, got, want))
			}
		}
		slices.Sort(found)
	}
	for _, key := range slices.Sorted(maps.Keys(MiseEnv)) {
		if got := getenv(key); got != MiseEnv[key] {
			found = append(found, fmt.Sprintf("%s=%q, a mise.toml ustawia %q", key, got, MiseEnv[key]))
		}
	}
	for _, key := range forbiddenEnv {
		if got := getenv(key); got != "" {
			found = append(found, fmt.Sprintf("%s=%q — nadpisanie z otoczenia, którego mise.toml nie ustawia", key, got))
		}
	}
	if got := getenv("GOSUMDB"); got != "" && got != "sum.golang.org" {
		found = append(found, fmt.Sprintf("GOSUMDB=%q — moduły sprawdzane innej bazie sum niż sum.golang.org", got))
	}
	return found
}
