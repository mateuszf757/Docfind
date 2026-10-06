// Package oci czyta obrazy zapisane przez `docker save` (układ OCI) i porównuje
// je warstwa po warstwie (dawniej ci/compare_oci.py).
package oci

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
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// AttestationKey — klucz manifestów atestacji w porównaniu.
const AttestationKey = "attestation-manifest"

// ConfigDigestFromSave zwraca digest konfiguracji obrazu z archiwum
// `docker save`.
//
// Konfiguracja niesie digesty rozpakowanych warstw (diff_ids), więc ten sam
// digest konfiguracji to ta sama zawartość — niezależnie od kompresji i typu
// manifestu, którymi kopia lokalna różni się od tej w rejestrze. Z archiwum,
// bo `docker image inspect .Id` znaczy co innego w magazynie klasycznym
// (konfiguracja) i w magazynie containerd (manifest), a BuildKit przy
// sterowniku docker nie podaje digestu konfiguracji w --metadata-file.
func ConfigDigestFromSave(archive string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%s: brak manifest.json w archiwum docker save", archive)
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", archive, err)
		}
		if path.Clean(hdr.Name) != "manifest.json" {
			continue
		}
		var manifests []struct {
			Config string `json:"Config"`
		}
		if err := json.NewDecoder(tr).Decode(&manifests); err != nil {
			return "", fmt.Errorf("manifest.json: %w", err)
		}
		if len(manifests) != 1 {
			return "", fmt.Errorf("manifest.json: oczekiwano jednego obrazu, jest %d", len(manifests))
		}
		// blobs/sha256/<hex> (Docker 25+) albo <hex>.json (starszy format).
		name := strings.TrimSuffix(path.Base(manifests[0].Config), ".json")
		if len(name) != 64 {
			return "", fmt.Errorf("manifest.json: nieoczekiwana ścieżka konfiguracji %q", manifests[0].Config)
		}
		return "sha256:" + name, nil
	}
}

// Extract rozpakowuje archiwum `docker save` do katalogu dir. Przyjmuje tylko
// katalogi i zwykłe pliki, a ścieżki wychodzące poza dir odrzuca — archiwum
// pochodzi z demona Dockera, ale rozpakowanie nie może ufać nazwom w tar.
func Extract(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", archive, err)
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("%s: ścieżka %q poza katalogiem docelowym", archive, hdr.Name)
		}
		target := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(target, tr); err != nil {
				return err
			}
		}
	}
}

func writeFile(target string, r io.Reader) (err error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	_, err = io.Copy(out, r)
	return err
}

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform,omitempty"`
}

type manifest struct {
	Config descriptor   `json:"config"`
	Layers []descriptor `json:"layers"`
}

type entry struct {
	digest   string
	manifest manifest
}

// Layout to rozpakowany układ OCI z `docker save`.
type Layout string

func (l Layout) blob(digest string) ([]byte, error) {
	hexPart, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hexPart) != 64 {
		return nil, fmt.Errorf("nieobsługiwany digest %q", digest)
	}
	return os.ReadFile(filepath.Join(string(l), "blobs", "sha256", hexPart))
}

// manifests zwraca manifesty z indeksu, kluczowane platformą (os/arch) albo
// rodzajem atestacji.
func (l Layout) manifests() (map[string]entry, error) {
	raw, err := os.ReadFile(filepath.Join(string(l), "index.json"))
	if err != nil {
		return nil, err
	}
	var top struct {
		Manifests []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("index.json: %w", err)
	}
	if len(top.Manifests) == 0 {
		return nil, errors.New("index.json: brak manifestów")
	}
	nodeRaw, err := l.blob(top.Manifests[0].Digest)
	if err != nil {
		return nil, err
	}
	// Węzeł to indeks (obraz z atestacją albo wieloplatformowy) albo od razu
	// manifest obrazu — bez atestacji docker save zapisuje sam manifest.
	var node map[string]json.RawMessage
	if err := json.Unmarshal(nodeRaw, &node); err != nil {
		return nil, err
	}
	entries := []descriptor{top.Manifests[0]}
	if list, ok := node["manifests"]; ok {
		entries = nil
		if err := json.Unmarshal(list, &entries); err != nil {
			return nil, err
		}
	}

	found := map[string]entry{}
	for _, d := range entries {
		mRaw, err := l.blob(d.Digest)
		if err != nil {
			return nil, err
		}
		var m manifest
		if err := json.Unmarshal(mRaw, &m); err != nil {
			return nil, err
		}
		key := ""
		if d.Annotations["vnd.docker.reference.type"] == AttestationKey {
			key = AttestationKey
		} else {
			goos, arch := "?", "?"
			if d.Platform != nil {
				goos, arch = d.Platform.OS, d.Platform.Architecture
			} else {
				// Bez indeksu platformy nie ma we wpisie — jest w konfiguracji.
				cRaw, err := l.blob(m.Config.Digest)
				if err != nil {
					return nil, err
				}
				var cfg struct {
					OS           string `json:"os"`
					Architecture string `json:"architecture"`
				}
				if err := json.Unmarshal(cRaw, &cfg); err != nil {
					return nil, err
				}
				goos, arch = cfg.OS, cfg.Architecture
			}
			key = goos + "/" + arch
		}
		found[key] = entry{digest: d.Digest, manifest: m}
	}
	return found, nil
}

// layerFiles zwraca ścieżkę pliku w warstwie → sumę jego zawartości (albo cel
// dowiązania, albo typ i prawa dla reszty).
func (l Layout) layerFiles(digest string) (map[string]string, error) {
	raw, err := l.blob(digest)
	if err != nil {
		return nil, err
	}
	var r io.Reader = bytes.NewReader(raw)
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	}
	files := map[string]string{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		switch hdr.Typeflag {
		case tar.TypeReg:
			h := sha256.New()
			if _, err := io.Copy(h, tr); err != nil {
				return nil, err
			}
			files[hdr.Name] = hex.EncodeToString(h.Sum(nil))
		case tar.TypeSymlink, tar.TypeLink:
			files[hdr.Name] = "-> " + hdr.Linkname
		default:
			files[hdr.Name] = fmt.Sprintf("%c %o %d:%d", hdr.Typeflag, hdr.Mode, hdr.Uid, hdr.Gid)
		}
	}
}

func explainLayer(a, b Layout, digestA, digestB string) ([]string, error) {
	filesA, err := a.layerFiles(digestA)
	if err != nil {
		return nil, err
	}
	filesB, err := b.layerFiles(digestB)
	if err != nil {
		return nil, err
	}
	var paths []string
	for p := range filesA {
		paths = append(paths, p)
	}
	for p := range filesB {
		if _, ok := filesA[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	var lines []string
	for _, p := range paths {
		if filesA[p] != filesB[p] {
			lines = append(lines, "      "+p)
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "      (pliki te same — różnią się metadane archiwum: czasy, prawa)")
	}
	return lines, nil
}

func short(digest string) string {
	if len(digest) > 19 {
		return digest[:19]
	}
	return digest
}

// Compare porównuje dwa obrazy: manifesty platform, czyli to, co faktycznie
// trafia do kontenera — konfigurację i warstwy. Manifesty atestacji są
// raportowane osobno i nie wpływają na werdykt: z definicji opisują przebieg
// budowania, a nie zawartość obrazu.
//
// Przy różnicy schodzi do poziomu pliku: które warstwy się różnią i które
// pliki w nich. Tak znaleziony został uv_cache.json z czasem instalacji
// w nanosekundach — jedyny bajt różniący dwa buildy tego samego commita.
//
// Zwraca true, gdy obrazy są identyczne. Błąd oznacza, że porównanie się nie
// wykonało — to awaria narzędzia, a nie wynik.
func Compare(a, b Layout, out io.Writer) (bool, error) {
	ours, err := a.manifests()
	if err != nil {
		return false, fmt.Errorf("%s: %w", a, err)
	}
	theirs, err := b.manifests()
	if err != nil {
		return false, fmt.Errorf("%s: %w", b, err)
	}
	var keys []string
	for k := range ours {
		keys = append(keys, k)
	}
	for k := range theirs {
		if _, ok := ours[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)

	identical := true
	for _, key := range keys {
		left, okLeft := ours[key]
		right, okRight := theirs[key]
		if !okLeft || !okRight {
			// Atestacja obecna tylko po jednej stronie to różnica w sposobie
			// budowania (lokalnie jej nie ma, w rejestrze jest), nie w zawartości.
			if key == AttestationKey {
				fmt.Fprintf(out, "         %s: obecna tylko w jednym obrazie — nie wpływa na werdykt\n", key)
				continue
			}
			fmt.Fprintf(out, "RÓŻNICA  %s: obecny tylko w jednym obrazie\n", key)
			identical = false
			continue
		}
		same := left.digest == right.digest
		if key == AttestationKey {
			note := "identyczna"
			if !same {
				note = "różna — oczekiwane, opisuje przebieg budowania"
			}
			fmt.Fprintf(out, "         %s: %s\n", key, note)
			continue
		}
		if same {
			fmt.Fprintf(out, "OK       %s: %s\n", key, short(left.digest))
			continue
		}
		identical = false
		fmt.Fprintf(out, "RÓŻNICA  %s: %s ≠ %s\n", key, short(left.digest), short(right.digest))
		layersA, layersB := left.manifest.Layers, right.manifest.Layers
		for i := 0; i < min(len(layersA), len(layersB)); i++ {
			if layersA[i].Digest == layersB[i].Digest {
				continue
			}
			fmt.Fprintf(out, "   warstwa %d: różne pliki\n", i)
			lines, err := explainLayer(a, b, layersA[i].Digest, layersB[i].Digest)
			if err != nil {
				return false, err
			}
			fmt.Fprintln(out, strings.Join(lines, "\n"))
		}
		if len(layersA) != len(layersB) {
			fmt.Fprintf(out, "   różna liczba warstw: %d ≠ %d\n", len(layersA), len(layersB))
		}
		if left.manifest.Config.Digest != right.manifest.Config.Digest {
			fmt.Fprintln(out, "   konfiguracja obrazu różna (także wtedy, gdy różni się tylko lista warstw)")
		}
	}
	return identical, nil
}
