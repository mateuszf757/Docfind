// Package chart pakuje chart aplikacji jako artefakt do rejestru OCI
// (decyzja 45): wersja z tożsamości commita, a w wartościach obraz przypięty
// digestem i wersja wdrożenia — chart z rejestru wdraża dokładnie ten obraz,
// który zbudowano i zaatestowano z tego commita.
//
// Archiwum powstaje powtarzalnie, jak obraz (decyzja 21): wpisy posortowane,
// czas każdego pliku to czas commita, właściciel 0, gzip bez nazwy i czasu
// w nagłówku. `helm package` zapisuje czasy modyfikacji plików z dysku —
// w CI czas checkoutu — więc ten sam commit dawałby różne digesty.
package chart

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
	sigsyaml "sigs.k8s.io/yaml"
)

// Build — co chart z rejestru ma wdrożyć.
type Build struct {
	// Version — wersja charta, semver bez metadanych budowania. OCI nie
	// dopuszcza „+" w tagu: Helm zapisuje „_", a z powrotem zamienia je tylko
	// umowa Helma i Argo CD — rejestr, gh attestation i reszta narzędzi widzą
	// „_". Porządku SemVer metadane i tak nie zmieniają, a commit jest
	// w appVersion i adnotacji revision.
	Version string
	// AppVersion — commit obrazu (tag), dla ludzi i `helm list`.
	AppVersion string
	// Tag i Digest — obraz aplikacji; DeployVersion — wersja wdrożenia
	// (api.version → DOCFIND_VERSION, decyzja 40).
	Tag, Digest, DeployVersion string
}

var (
	semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)
	digest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Validate odrzuca build, którego nie da się opublikować.
func (b Build) Validate() error {
	switch {
	case !semver.MatchString(b.Version):
		return fmt.Errorf("wersja charta %q: oczekiwano semver bez metadanych budowania (X.Y.Z albo X.Y.Z-pre)", b.Version)
	case !digest.MatchString(b.Digest):
		return fmt.Errorf("digest obrazu %q: oczekiwano sha256:<64 hex>", b.Digest)
	case b.Tag == "" || b.AppVersion == "" || b.DeployVersion == "":
		return errors.New("tag obrazu, appVersion i wersja wdrożenia są wymagane")
	case strings.Contains(b.DeployVersion, "dirty"):
		return fmt.Errorf("wersja %s z brudnego drzewa — publikowany chart musi odpowiadać commitowi", b.DeployVersion)
	}
	return nil
}

// ChartVersion — wersja charta z wersji artefaktu: bez metadanych budowania.
func ChartVersion(artifactVersion string) string {
	v, _, _ := strings.Cut(artifactVersion, "+")
	return v
}

// Typy mediów charta Helma w rejestrze OCI (helm.sh/docs/topics/registries).
const (
	ManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	ConfigMediaType   = "application/vnd.cncf.helm.config.v1+json"
	LayerMediaType    = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
)

// OCI — chart jako artefakt OCI: konfiguracja (metadane z Chart.yaml
// w JSON), warstwa (archiwum) i manifest. Bez czasu publikacji — `helm push`
// dopisuje do manifestu bieżący czas, więc ten sam chart dawałby przy
// każdym biegu inny digest; tu jest czas commita.
type OCI struct {
	Name, Version string
	Config        []byte
	Archive       []byte
	Manifest      []byte
}

// Artifact pakuje chart i składa jego manifest OCI. source — repozytorium
// kodu (adnotacja org.opencontainers.image.source wiąże pakiet w GHCR
// z repozytorium).
func Artifact(name string, files map[string][]byte, b Build, epoch time.Time, source string) (OCI, error) {
	archive, err := Package(name, files, b, epoch)
	if err != nil {
		return OCI{}, err
	}
	meta, err := set(files["Chart.yaml"], map[string]string{"version": b.Version, "appVersion": b.AppVersion})
	if err != nil {
		return OCI{}, err
	}
	config, err := sigsyaml.YAMLToJSON(meta)
	if err != nil {
		return OCI{}, fmt.Errorf("metadane charta jako JSON: %w", err)
	}
	type descriptor struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int    `json:"size"`
	}
	manifest, err := json.Marshal(struct {
		SchemaVersion int               `json:"schemaVersion"`
		MediaType     string            `json:"mediaType"`
		Config        descriptor        `json:"config"`
		Layers        []descriptor      `json:"layers"`
		Annotations   map[string]string `json:"annotations"`
	}{
		SchemaVersion: 2, MediaType: ManifestMediaType,
		Config: descriptor{ConfigMediaType, sha(config), len(config)},
		Layers: []descriptor{{LayerMediaType, sha(archive), len(archive)}},
		Annotations: map[string]string{
			"org.opencontainers.image.title":    name,
			"org.opencontainers.image.version":  b.Version,
			"org.opencontainers.image.revision": b.AppVersion,
			"org.opencontainers.image.created":  epoch.UTC().Format(time.RFC3339),
			"org.opencontainers.image.source":   source,
		},
	})
	if err != nil {
		return OCI{}, err
	}
	return OCI{Name: name, Version: b.Version, Config: config, Archive: archive, Manifest: manifest}, nil
}

// Digest — digest manifestu, czyli tożsamość artefaktu w rejestrze.
func (o OCI) Digest() string { return sha(o.Manifest) }

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Package zwraca archiwum charta name z plików files (ścieżki względem
// katalogu charta) z wersją i obrazem z b, powtarzalne dla tego samego
// wejścia i epoch.
func Package(name string, files map[string][]byte, b Build, epoch time.Time) ([]byte, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	out := maps.Clone(files)
	var err error
	if out["Chart.yaml"], err = set(files["Chart.yaml"], map[string]string{"version": b.Version, "appVersion": b.AppVersion}); err != nil {
		return nil, fmt.Errorf("plik Chart.yaml: %w", err)
	}
	if out["values.yaml"], err = set(files["values.yaml"], map[string]string{
		"api.image.tag": b.Tag, "api.image.digest": b.Digest, "api.version": b.DeployVersion,
	}); err != nil {
		return nil, fmt.Errorf("plik values.yaml: %w", err)
	}

	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	// Nagłówek gzip bez nazwy pliku i bez czasu — zerowe wartości.
	tw := tar.NewWriter(gz)
	for _, p := range slices.Sorted(maps.Keys(out)) {
		data := out[p]
		hdr := &tar.Header{
			Name: path.Join(name, p), Mode: 0o644, Size: int64(len(data)),
			ModTime: epoch.UTC(), Typeflag: tar.TypeReg, Format: tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// set ustawia wartości tekstowe pod ścieżkami „a.b.c" w dokumencie YAML,
// zachowując komentarze i kolejność kluczy. Ścieżka musi istnieć —
// literówka w nazwie klucza to błąd, a nie nowy klucz obok właściwego.
func set(doc []byte, values map[string]string) ([]byte, error) {
	if len(doc) == 0 {
		return nil, errors.New("brak pliku")
	}
	var root yaml.Node
	if err := yaml.Unmarshal(doc, &root); err != nil {
		return nil, err
	}
	if len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("oczekiwano mapy na najwyższym poziomie")
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		node, err := lookup(root.Content[0], strings.Split(key, "."))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		node.Kind, node.Tag, node.Value, node.Style = yaml.ScalarNode, "!!str", values[key], yaml.DoubleQuotedStyle
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func lookup(m *yaml.Node, keys []string) (*yaml.Node, error) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != keys[0] {
			continue
		}
		v := m.Content[i+1]
		if len(keys) == 1 {
			if v.Kind != yaml.ScalarNode {
				return nil, errors.New("nie jest wartością prostą")
			}
			return v, nil
		}
		if v.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s nie jest mapą", keys[0])
		}
		return lookup(v, keys[1:])
	}
	return nil, fmt.Errorf("brak klucza %s", keys[0])
}

// Read odczytuje z archiwum charta to, co Package w nim zapisał: wersję
// i obraz. Weryfikacja po publikacji porównuje to z tym, co miało trafić
// do rejestru — z bajtów pobranych z rejestru, a nie z pamięci.
func Read(archive []byte) (Build, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return Build{}, err
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Build{}, err
		}
		// Tylko pliki charta, nie podchartów: <nazwa>/Chart.yaml, <nazwa>/values.yaml.
		_, file, _ := strings.Cut(h.Name, "/")
		if file == "Chart.yaml" || file == "values.yaml" {
			data, err := io.ReadAll(tr)
			if err != nil {
				return Build{}, err
			}
			files[file] = data
		}
	}
	var meta struct {
		Version    string `json:"version"`
		AppVersion string `json:"appVersion"`
	}
	var values struct {
		API struct {
			Version string `json:"version"`
			Image   struct {
				Tag    string `json:"tag"`
				Digest string `json:"digest"`
			} `json:"image"`
		} `json:"api"`
	}
	if err := sigsyaml.Unmarshal(files["Chart.yaml"], &meta); err != nil || meta.Version == "" {
		return Build{}, fmt.Errorf("archiwum bez Chart.yaml z wersją: %v", err)
	}
	if err := sigsyaml.Unmarshal(files["values.yaml"], &values); err != nil {
		return Build{}, fmt.Errorf("values.yaml: %w", err)
	}
	return Build{
		Version: meta.Version, AppVersion: meta.AppVersion,
		Tag: values.API.Image.Tag, Digest: values.API.Image.Digest, DeployVersion: values.API.Version,
	}, nil
}

// Files — pliki charta z katalogu dir, ścieżki względem niego.
func Files(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)], err = os.ReadFile(p)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("chart %s: %w", dir, err)
	}
	return files, nil
}
