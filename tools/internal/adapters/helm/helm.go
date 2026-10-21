// Package helm to adapter Helma: render i lint chartów, instalacja
// release'u na klastrze środowiska i przypięte charty platformy w pamięci
// podręcznej, sprawdzane sumą przy każdym użyciu.
package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	coreChart "github.com/mateuszf757/Docfind/tools/internal/core/chart"
	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
	"github.com/mateuszf757/Docfind/tools/internal/core/pins"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
	"github.com/mateuszf757/Docfind/tools/internal/kit/fetch"
	"github.com/mateuszf757/Docfind/tools/internal/kit/proc"
)

// Helm — program helm.
type Helm struct {
	Runner proc.Runner
	// Out i Err — wyjście lint i instalacji na bieżąco.
	Out, Err io.Writer
}

func valueArgs(r cluster.Release) []string {
	var args []string
	if r.Version != "" {
		args = append(args, "--version", r.Version)
	}
	if r.Namespace != "" {
		args = append(args, "--namespace", r.Namespace)
	}
	if r.CreateNamespace {
		args = append(args, "--create-namespace")
	}
	for _, f := range r.ValuesFiles {
		args = append(args, "--values", f)
	}
	for _, v := range r.Set {
		args = append(args, "--set", v)
	}
	for _, v := range r.SetString {
		args = append(args, "--set-string", v)
	}
	return args
}

// Install instaluje albo aktualizuje release na klastrze t (`helm upgrade
// --install --wait`): kończy się, gdy zasoby są gotowe, a nie gdy API je
// przyjęło. Z HooksOnly — gdy przeszły hooki; gotowości pilnuje wtedy
// wywołujący.
func (h Helm) Install(ctx context.Context, t cluster.Target, r cluster.Release) error {
	wait := "--wait=watcher"
	if r.HooksOnly {
		wait = "--wait=hookOnly"
	}
	args := append([]string{"upgrade", "--install", r.Name, r.Chart}, valueArgs(r)...)
	args = append(args, wait, "--timeout", r.Timeout.String(), "--kubeconfig", t.Kubeconfig, "--kube-context", t.Context)
	if _, err := h.Runner.Run(ctx, proc.Cmd{Name: "helm", Args: args, Stdout: h.Out, Stderr: h.Err}); err != nil {
		return fmt.Errorf("helm upgrade --install %s: %w", r.Name, err)
	}
	return nil
}

// Revision — numer bieżącej rewizji release (`helm status -o json`).
func (h Helm) Revision(ctx context.Context, t cluster.Target, namespace, release string) (int, error) {
	out, err := proc.Output(ctx, h.Runner, proc.Cmd{Name: "helm", Args: []string{"status", release, "--namespace", namespace, "--output", "json", "--kubeconfig", t.Kubeconfig, "--kube-context", t.Context}})
	if err != nil {
		return 0, fmt.Errorf("helm status %s: %w", release, err)
	}
	var status struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil || status.Version < 1 {
		return 0, fmt.Errorf("helm status %s: nieczytelna rewizja (%v)", release, err)
	}
	return status.Version, nil
}

// Rollback cofa release do rewizji revision i czeka na gotowość zasobów,
// jak Install. Rollback to nowa rewizja z manifestem tamtej — na klastrze te
// same operacje co upgrade do niej.
func (h Helm) Rollback(ctx context.Context, t cluster.Target, namespace, release string, revision int, timeout time.Duration) error {
	args := []string{"rollback", release, strconv.Itoa(revision), "--namespace", namespace, "--wait=watcher", "--timeout", timeout.String(), "--kubeconfig", t.Kubeconfig, "--kube-context", t.Context}
	if _, err := h.Runner.Run(ctx, proc.Cmd{Name: "helm", Args: args, Stdout: h.Out, Stderr: h.Err}); err != nil {
		return fmt.Errorf("helm rollback %s %d: %w", release, revision, err)
	}
	return nil
}

// Template renderuje release do w. Odmowa Helma (schemat wartości,
// szablon) to warunek niespełniony z powodem z Helma; niemożność
// uruchomienia — awaria.
func (h Helm) Template(ctx context.Context, r cluster.Release, includeCRDs bool, w io.Writer) error {
	args := append([]string{"template", r.Name, r.Chart}, valueArgs(r)...)
	if includeCRDs {
		args = append(args, "--include-crds")
	}
	var stderr bytes.Buffer
	_, err := h.Runner.Run(ctx, proc.Cmd{Name: "helm", Args: args, Stdout: w, Stderr: &stderr})
	if proc.ExitCode(err) > 0 {
		return cli.Unmet("helm template %s: %v", r.Name, err)
	}
	return err
}

// Lint sprawdza chart z podanymi wartościami.
func (h Helm) Lint(ctx context.Context, chart string, set ...string) error {
	args := []string{"lint", chart}
	for _, v := range set {
		args = append(args, "--set", v)
	}
	_, err := h.Runner.Run(ctx, proc.Cmd{Name: "helm", Args: args, Stdout: h.Out, Stderr: h.Err})
	if code := proc.ExitCode(err); code > 0 {
		return cli.Unmet("helm lint %s: kod %d", filepath.Base(chart), code)
	}
	return err
}

// Charts — przypięte charty platformy w pamięci podręcznej repozytorium
// (.cache/downloads).
type Charts struct {
	Helm Helm
	Root string
	// Registry — rejestr OCI (adapters/registry) dla chartów publikowanych
	// tylko jako artefakt OCI.
	Registry Registry
}

// Registry — odczyt manifestu i warstwy z rejestru OCI.
type Registry interface {
	Manifest(ctx context.Context, ref string) ([]byte, string, error)
	Blob(ctx context.Context, repository, digest string) ([]byte, error)
}

// Fetch zwraca ścieżkę archiwum przypiętego charta: z pamięci podręcznej
// albo pobranego (HTTPS albo warstwa charta z manifestu w rejestrze OCI),
// zawsze po sprawdzeniu sumy z ci/pins.env.
//
// Chart z OCI to warstwa manifestu pod wersją — te same bajty, które
// zapisałby `helm pull`, i to ich sumą jest przypięcie. Bez `helm pull`:
// zadanie CI z prawem zapisu do rejestru (lustro, decyzja 80) nie uruchamia
// binarki Helma, która czyta poświadczenia z konfiguracji Dockera.
func (c Charts) Fetch(ctx context.Context, chart pins.Chart) (string, error) {
	if !chart.OCI() {
		return fetch.URL(ctx, c.Root, chart.Source, chart.SHA256)
	}
	repository := strings.TrimPrefix(chart.Source, "oci://")
	file := filepath.Join(c.Root, fetch.Dir, fmt.Sprintf("%s-%s.tgz", path.Base(repository), chart.Version))
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		if err := c.pull(ctx, repository, chart.Version, file); err != nil {
			return "", err
		}
	}
	return file, fetch.Verify(file, chart.SHA256)
}

// pull zapisuje do file warstwę charta Helma z manifestu repository:version.
func (c Charts) pull(ctx context.Context, repository, version, file string) error {
	if c.Registry == nil {
		return fmt.Errorf("chart %s z rejestru OCI: brak dostępu do rejestru", repository)
	}
	ref := repository + ":" + version
	raw, _, err := c.Registry.Manifest(ctx, ref)
	if err != nil {
		return err
	}
	var m struct {
		Layers []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("manifest %s: %w", ref, err)
	}
	var layer string
	for _, l := range m.Layers {
		if l.MediaType == coreChart.LayerMediaType {
			layer = l.Digest
		}
	}
	if layer == "" {
		return cli.Unmet("%s: manifest bez warstwy charta Helma (%s)", ref, coreChart.LayerMediaType)
	}
	data, err := c.Registry.Blob(ctx, repository, layer)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	// Plik tymczasowy w tym samym katalogu: rename jest wtedy atomowy, a dwa
	// równoległe pobrania nie piszą do jednego pliku.
	tmp, err := os.CreateTemp(filepath.Dir(file), "pull-*.tgz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}
