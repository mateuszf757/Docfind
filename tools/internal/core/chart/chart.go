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
	return packaged(name, files, Meta{Version: b.Version, AppVersion: b.AppVersion}, archive, epoch, source)
}

// Meta — wersja charta bez obrazu w wartościach (chart platformy): wersja
// z tożsamości commita i commit w appVersion, jak w charcie aplikacji.
type Meta struct {
	Version, AppVersion string
}

// Validate odrzuca metadane, których nie da się opublikować.
func (m Meta) Validate() error {
	switch {
	case !semver.MatchString(m.Version):
		return fmt.Errorf("wersja charta %q: oczekiwano semver bez metadanych budowania (X.Y.Z albo X.Y.Z-pre)", m.Version)
	case m.AppVersion == "":
		return errors.New("appVersion jest wymagane")
	}
	return nil
}

// Plain pakuje chart bez obrazu w wartościach — tylko wersja i appVersion
// w Chart.yaml — i składa jego manifest OCI, powtarzalnie jak Artifact.
func Plain(name string, files map[string][]byte, m Meta, epoch time.Time, source string) (OCI, error) {
	if err := m.Validate(); err != nil {
		return OCI{}, err
	}
	out := maps.Clone(files)
	var err error
	if out["Chart.yaml"], err = set(files["Chart.yaml"], map[string]string{"version": m.Version, "appVersion": m.AppVersion}); err != nil {
		return OCI{}, fmt.Errorf("plik Chart.yaml: %w", err)
	}
	archive, err := tgz(name, out, epoch)
	if err != nil {
		return OCI{}, err
	}
	return packaged(name, files, m, archive, epoch, source)
}

// Values pakuje chart bez obrazu, w którym dft wpisuje do values.yaml całe
// gałęzie najwyższego poziomu (pakiet dostawy, decyzja 81: komponenty
// i wartości środowisk). Klucze muszą już stać w values.yaml — literówka to
// błąd, nie nowy klucz; komentarze reszty pliku zostają.
func Values(name string, files map[string][]byte, m Meta, values map[string]any, epoch time.Time, source string) (OCI, error) {
	doc, err := replaceTop(files["values.yaml"], values)
	if err != nil {
		return OCI{}, fmt.Errorf("plik values.yaml: %w", err)
	}
	out := maps.Clone(files)
	out["values.yaml"] = doc
	return Plain(name, out, m, epoch, source)
}

// replaceTop zastępuje wartości istniejących kluczy najwyższego poziomu
// dokumentu YAML, zachowując komentarze i kolejność kluczy. Mapy kodowane
// są z kluczami posortowanymi, więc wynik zależy tylko od treści.
func replaceTop(doc []byte, values map[string]any) ([]byte, error) {
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
	m := root.Content[0]
	for _, key := range slices.Sorted(maps.Keys(values)) {
		// Przez JSON: struktury z tagami json (jak w charcie) i mapy
		// z posortowanymi kluczami.
		raw, err := json.Marshal(values[key])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return nil, err
		}
		found := false
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value != key {
				continue
			}
			var n yaml.Node
			if err := n.Encode(generic); err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			n.HeadComment, n.LineComment, n.FootComment = m.Content[i+1].HeadComment, m.Content[i+1].LineComment, m.Content[i+1].FootComment
			m.Content[i+1] = &n
			found = true
		}
		if !found {
			return nil, fmt.Errorf("brak klucza %s", key)
		}
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

// ValuesOf zwraca values.yaml charta z archiwum jako mapę — to, co wdroży
// Argo CD z pakietu pobranego z rejestru.
func ValuesOf(archive []byte) (map[string]any, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("archiwum bez values.yaml")
		}
		if err != nil {
			return nil, err
		}
		if _, file, _ := strings.Cut(h.Name, "/"); file != "values.yaml" {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		var values map[string]any
		if err := sigsyaml.Unmarshal(data, &values); err != nil {
			return nil, fmt.Errorf("values.yaml: %w", err)
		}
		return values, nil
	}
}

// packaged składa konfigurację i manifest OCI dla archiwum charta z plików
// files z metadanymi m.
func packaged(name string, files map[string][]byte, m Meta, archive []byte, epoch time.Time, source string) (OCI, error) {
	meta, err := set(files["Chart.yaml"], map[string]string{"version": m.Version, "appVersion": m.AppVersion})
	if err != nil {
		return OCI{}, err
	}
	config, err := sigsyaml.YAMLToJSON(meta)
	if err != nil {
		return OCI{}, fmt.Errorf("metadane charta jako JSON: %w", err)
	}
	manifest, err := ociManifest(config, archive, map[string]string{
		"org.opencontainers.image.title":    name,
		"org.opencontainers.image.version":  m.Version,
		"org.opencontainers.image.revision": m.AppVersion,
		"org.opencontainers.image.created":  epoch.UTC().Format(time.RFC3339),
		"org.opencontainers.image.source":   source,
	})
	if err != nil {
		return OCI{}, err
	}
	return OCI{Name: name, Version: m.Version, Config: config, Archive: archive, Manifest: manifest}, nil
}

// ociManifest — manifest OCI charta Helma: konfiguracja i jedna warstwa
// z archiwum, w formacie, który czytają Helm i Argo CD.
func ociManifest(config, archive []byte, annotations map[string]string) ([]byte, error) {
	type descriptor struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int    `json:"size"`
	}
	return json.Marshal(struct {
		SchemaVersion int               `json:"schemaVersion"`
		MediaType     string            `json:"mediaType"`
		Config        descriptor        `json:"config"`
		Layers        []descriptor      `json:"layers"`
		Annotations   map[string]string `json:"annotations"`
	}{
		SchemaVersion: 2, MediaType: ManifestMediaType,
		Config:      descriptor{ConfigMediaType, sha(config), len(config)},
		Layers:      []descriptor{{LayerMediaType, sha(archive), len(archive)}},
		Annotations: annotations,
	})
}

// UpstreamAnnotation — adnotacja lustra: skąd pochodzi archiwum (adres
// z ci/pins.env).
const UpstreamAnnotation = "io.docfind.mirror.upstream"

// Mirror składa manifest OCI wokół archiwum charta z zewnątrz, bez zmiany
// jednego bajtu: warstwa to to samo archiwum, którego sumę zapisuje
// ci/pins.env, więc digest warstwy jest tą sumą, a digest manifestu wynika
// z niej deterministycznie — bez czasu publikacji, bez zapytania do
// rejestru (decyzja 80). upstream — adres, z którego pochodzi archiwum;
// source — repozytorium, z którym GHCR wiąże pakiet.
//
// Archiwum musi mieć jeden katalog najwyższego poziomu z Chart.yaml — tak
// rozpakowuje chart Argo CD (util/oci, „expected 1 directory").
func Mirror(archive []byte, upstream, source string) (OCI, error) {
	chartYAML, err := topLevelChart(archive)
	if err != nil {
		return OCI{}, err
	}
	var meta struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := sigsyaml.Unmarshal(chartYAML, &meta); err != nil || meta.Name == "" || meta.Version == "" {
		return OCI{}, fmt.Errorf("metadane charta bez nazwy albo wersji: %v", err)
	}
	config, err := sigsyaml.YAMLToJSON(chartYAML)
	if err != nil {
		return OCI{}, fmt.Errorf("metadane charta jako JSON: %w", err)
	}
	manifest, err := ociManifest(config, archive, map[string]string{
		"org.opencontainers.image.title":   meta.Name,
		"org.opencontainers.image.version": meta.Version,
		"org.opencontainers.image.source":  source,
		UpstreamAnnotation:                 upstream,
	})
	if err != nil {
		return OCI{}, err
	}
	return OCI{Name: meta.Name, Version: meta.Version, Config: config, Archive: archive, Manifest: manifest}, nil
}

// topLevelChart zwraca Chart.yaml charta z archiwum, w którym wszystko leży
// w jednym katalogu najwyższego poziomu.
func topLevelChart(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("archiwum charta: %w", err)
	}
	tr := tar.NewReader(gz)
	tops := map[string]bool{}
	var chartYAML []byte
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("archiwum charta: %w", err)
		}
		// Nagłówek globalny PAX to nie plik — tar go nie rozpakowuje.
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		top, rest, _ := strings.Cut(strings.TrimPrefix(h.Name, "./"), "/")
		tops[top] = true
		if rest == "Chart.yaml" {
			if chartYAML, err = io.ReadAll(tr); err != nil {
				return nil, err
			}
		}
	}
	if len(tops) != 1 || chartYAML == nil {
		return nil, fmt.Errorf("archiwum charta: oczekiwano jednego katalogu z Chart.yaml, jest %d wpisów najwyższego poziomu", len(tops))
	}
	return chartYAML, nil
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

	return tgz(name, out, epoch)
}

// tgz zapisuje pliki files jako archiwum charta name: wpisy posortowane,
// czas każdego to epoch, nagłówek gzip bez nazwy pliku i czasu.
func tgz(name string, files map[string][]byte, epoch time.Time) ([]byte, error) {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	// Nagłówek gzip bez nazwy pliku i bez czasu — zerowe wartości.
	tw := tar.NewWriter(gz)
	for _, p := range slices.Sorted(maps.Keys(files)) {
		data := files[p]
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
