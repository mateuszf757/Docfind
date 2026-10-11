package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/adapters/docker"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/git"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/github"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/gotool"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/helm"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/k3d"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/kube"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/registry"
	"github.com/mateuszf757/Docfind/tools/internal/adapters/uv"
	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/core/environment"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
	"github.com/mateuszf757/Docfind/tools/internal/stages/admission"
	"github.com/mateuszf757/Docfind/tools/internal/stages/build"
)

// workspace — to, czego potrzebuje każde polecenie: korzeń repozytorium
// i sposób uruchamiania programów. Z niego powstają adaptery, które cmd
// podstawia etapom.
type workspace struct {
	root   string
	runner proc.Runner
}

func (w *workspace) git() git.Repo         { return git.Repo{Dir: w.root, Runner: w.runner} }
func (w *workspace) docker() docker.Client { return docker.Client{Runner: w.runner} }
func (w *workspace) gh() github.CLI        { return github.CLI{Runner: w.runner} }
func (w *workspace) k3d() k3d.K3d          { return k3d.K3d{Runner: w.runner} }

func (w *workspace) helm() helm.Helm {
	return helm.Helm{Runner: w.runner, Out: os.Stdout, Err: os.Stderr}
}

// charts — przypięte charty platformy w .cache/downloads repozytorium.
func (w *workspace) charts() helm.Charts { return helm.Charts{Helm: w.helm(), Root: w.root} }

// gotool — toolchain Go na module tools/.
func (w *workspace) gotool() gotool.Go {
	return gotool.Go{Runner: w.runner, ModuleDir: filepath.Join(w.root, "tools"), Out: os.Stdout, Err: os.Stderr}
}

// python — projekt Pythona aplikacji.
func (w *workspace) python() uv.Python {
	return uv.Python{Runner: w.runner, Dir: filepath.Join(w.root, "services", "api"), Out: os.Stdout, Err: os.Stderr}
}

// builder — etap build z gitem, Dockerem i rejestrem.
func (w *workspace) builder() build.Builder {
	return build.Builder{Versions: w.git(), Images: w.docker(), Registry: registry.Client{}}
}

// attestations — weryfikacja atestacji GitHuba obrazów z workflowu ci
// repozytorium, z którego pochodzi kod (GITHUB_REPOSITORY w CI).
func (w *workspace) attestations() github.Attestations {
	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		repo = artifact.DefaultOwner + "/Docfind"
	}
	return github.Attestations{CLI: w.gh(), Repo: repo, Workflow: ".github/workflows/ci.yml"}
}

// admission — wpuszczanie obrazów z rejestru (stages/admission).
func (w *workspace) admission() admission.Gate {
	return admission.Gate{Registry: registry.Client{}, Attestations: w.attestations()}
}

// environmentOf wczytuje definicję wybranego środowiska (DOCFIND_ENV,
// domyślnie dev).
func (w *workspace) environmentOf() (environment.Definition, error) {
	def, err := environment.Load(w.root, environment.Selected())
	if err != nil {
		return environment.Definition{}, err
	}
	if def.Cluster.Provider == environment.ProviderNone {
		cli.Step("środowisko %s (%s), bez klastra", def.Name, def.Stage)
	} else {
		cli.Step("środowisko %s (%s), kontekst %s", def.Name, def.Stage, def.Cluster.Context)
	}
	return def, nil
}

// connect łączy się z klastrem środowiska przez strażnika: klaster musi być
// w definicji, a kubeconfig wskazywać jego kontekst i serwer.
func (w *workspace) connect(def environment.Definition) (*kube.Cluster, error) {
	if err := def.RequireCluster(); err != nil {
		return nil, err
	}
	return kube.Connect(def.Target(w.root), w.runner)
}

// imageName zwraca nazwę obrazu usługi; DF_IMAGE_REGISTRY zastępuje GHCR
// (test publikacji na rejestrze tymczasowym).
func imageName(service string) string {
	return artifact.ImageName(os.Getenv("DF_IMAGE_REGISTRY"), os.Getenv("GITHUB_REPOSITORY_OWNER"), service)
}

// envFlag czyta zmienną środowiskową w postaci 0/1 — ten sam interfejs co
// dawny ci/build.sh (RELEASE=1, PUSH=1, NO_CACHE=1), z którego korzysta
// workflow.
func envFlag(name string) (bool, error) {
	switch os.Getenv(name) {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, cli.Usage("%s=%q — dozwolone 0 albo 1", name, os.Getenv(name))
	}
}

// requireTools sprawdza wymagania na starcie i zgłasza wszystkie braki
// naraz. Brak narzędzia w połowie biegu wyglądałby jak błąd w kodzie,
// a nie w środowisku.
func requireTools(names ...string) error {
	var missing []string
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("brak w PATH: %s — uruchom przez ./bin/mise run …, które instaluje narzędzia z mise.lock", strings.Join(missing, ", "))
	}
	return nil
}

// clientGoVersion zwraca wersję k8s.io/client-go wkompilowaną w program.
func clientGoVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path == "k8s.io/client-go" {
			return dep.Version
		}
	}
	return ""
}

// reportDir — katalog raportów bramek: DF_REPORT_DIR albo .cache/reports.
// Dowód zostaje po biegu, a w CI trafia do artefaktów.
func (w *workspace) reportDir() string {
	if dir := os.Getenv("DF_REPORT_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(w.root, ".cache", "reports")
}

// writeReport zapisuje wynik bramki w JSON do katalogu raportów.
func (w *workspace) writeReport(name string, v any) error {
	dir := w.reportDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name+".json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return err
	}
	cli.Step("raport: %s", path)
	return nil
}

// withReport zapisuje raport bramki także przy wyniku negatywnym i łączy
// błąd zapisu z wynikiem.
func (w *workspace) withReport(name string, v any, err error) error {
	if reportErr := w.writeReport(name, v); reportErr != nil {
		return errors.Join(err, reportErr)
	}
	return err
}

// appendFile dopisuje do pliku, jak `>>` w powłoce. Błąd zamknięcia też
// jest błędem: przy zapisie buforowanym dopiero Close mówi, czy dane
// trafiły na dysk.
func appendFile(path, content string) (err error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	_, err = f.WriteString(content)
	return err
}

// appendSummary dopisuje Markdown do podsumowania zadania w GitHub Actions;
// poza CI nic nie robi.
func appendSummary(markdown string) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	return appendFile(path, markdown)
}

// githubOutput dopisuje wyjścia kroku do GITHUB_OUTPUT; poza CI nic nie robi.
func githubOutput(lines string) error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" || lines == "" {
		return nil
	}
	if err := appendFile(path, lines); err != nil {
		return fmt.Errorf("GITHUB_OUTPUT: %w", err)
	}
	return nil
}

// runURL — link do biegu workflowu w CI; pusty poza nim.
func runURL() string {
	server, repo, run := os.Getenv("GITHUB_SERVER_URL"), os.Getenv("GITHUB_REPOSITORY"), os.Getenv("GITHUB_RUN_ID")
	if run == "" {
		return ""
	}
	return server + "/" + repo + "/actions/runs/" + run
}
