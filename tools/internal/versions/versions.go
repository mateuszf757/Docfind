// Package versions sprawdza spójność wersji zapisanych w miejscach, których
// żaden automat nie synchronizuje: mise.toml, ci/pins.env, Dockerfile,
// .python-version i tools/go.mod. Rozjazd ma wyjść w zadaniu test, a nie jako
// obraz na innym Pythonie niż testy albo kubectl innym niż klaster.
package versions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/pins"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// Check porównuje wersje w repozytorium o korzeniu root. goVersion to wersja
// Go, którą zbudowano program (runtime.Version()) — przy GOTOOLCHAIN=local
// to wersja z mise.toml.
func Check(ctx context.Context, root string, r proc.Runner, goVersion string) error {
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
	kubectl, err := kubectlVersion(ctx, r)
	if err != nil {
		return err
	}
	if want := "v" + p.Get("DF_KUBERNETES_VERSION"); kubectl != want {
		v.Fail("kubectl %s (mise.toml), a klaster i schematy to %s (%s)", kubectl, want, pins.File)
	} else {
		cli.Step("kubectl %s zgodny z klastrem", kubectl)
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
	uvOut, err := proc.Output(ctx, r, proc.Cmd{Name: "uv", Args: []string{"--version"}})
	if err != nil {
		return fmt.Errorf("uv --version: %w", err)
	}
	if fields := strings.Fields(uvOut); len(fields) < 2 || fields[1] != uvWant {
		cli.Warn("lokalny %s, a Dockerfile i CI używają uv %s — uv self update %s", uvOut, uvWant, uvWant)
	}

	return v.Err("spójność wersji")
}

func kubectlVersion(ctx context.Context, r proc.Runner) (string, error) {
	out, err := proc.Output(ctx, r, proc.Cmd{Name: "kubectl", Args: []string{"version", "--client", "-o", "json"}})
	if err != nil {
		return "", fmt.Errorf("kubectl version: %w", err)
	}
	var version struct {
		ClientVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"clientVersion"`
	}
	if err := json.Unmarshal([]byte(out), &version); err != nil {
		return "", fmt.Errorf("kubectl version: nieczytelny JSON: %w", err)
	}
	if version.ClientVersion.GitVersion == "" {
		return "", fmt.Errorf("kubectl version: brak clientVersion.gitVersion")
	}
	return version.ClientVersion.GitVersion, nil
}
