// Package fetch pobiera artefakty spoza repozytorium (charty) do pamięci
// podręcznej w .cache/downloads i sprawdza ich sumę przy każdym użyciu,
// a nie tylko przy pobraniu — plik w pamięci podręcznej też może zostać
// podmieniony albo uszkodzony (dawniej df_fetch_verified w ci/lib.sh).
//
// Suma pochodzi z ci/pins.env, a nie z serwera, z którego idzie plik: suma
// ściągnięta z tego samego miejsca chroni tylko przed uszkodzeniem
// w transferze, a nie przed podmianą.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

// Dir — pamięć podręczna względem korzenia repozytorium (poza gitem).
const Dir = ".cache/downloads"

// URL pobiera plik po HTTPS i zwraca ścieżkę w pamięci podręcznej.
func URL(ctx context.Context, root, url, sha string) (string, error) {
	file := filepath.Join(root, Dir, path.Base(url))
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		if err := download(ctx, url, file); err != nil {
			return "", err
		}
	}
	return file, verify(file, sha)
}

// OCIChart pobiera chart z rejestru OCI przez `helm pull` i sprawdza sumę.
// `helm pull` sam nie weryfikuje niczego poza tym, co poda rejestr.
func OCIChart(ctx context.Context, r proc.Runner, root, ref, version, sha string) (string, error) {
	file := filepath.Join(root, Dir, fmt.Sprintf("%s-%s.tgz", path.Base(ref), version))
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return "", err
		}
		// Katalog tymczasowy w tym samym systemie plików: rename jest wtedy
		// atomowy, a dwa równoległe pobrania nie piszą do jednego pliku.
		tmp, err := os.MkdirTemp(filepath.Dir(file), "pull-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmp)
		if _, err := r.Run(ctx, proc.Cmd{Name: "helm", Args: []string{"pull", ref, "--version", version, "--destination", tmp}}); err != nil {
			return "", fmt.Errorf("helm pull %s %s: %w", ref, version, err)
		}
		if err := os.Rename(filepath.Join(tmp, fmt.Sprintf("%s-%s.tgz", path.Base(ref), version)), file); err != nil {
			return "", err
		}
	}
	return file, verify(file, sha)
}

func download(ctx context.Context, url, file string) (err error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".part.*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pobranie %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pobranie %s: HTTP %d", url, resp.StatusCode)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pobranie %s: %w", url, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// verify porównuje sumę pliku z przypiętą; niezgodny plik jest usuwany,
// żeby kolejny bieg pobrał go od nowa, zamiast powtarzać błąd.
func verify(file, want string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	_ = f.Close()
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(file)
		return cli.Unmet("suma %s nie zgadza się z przypiętą w ci/pins.env (otrzymano %s, oczekiwano %s)", filepath.Base(file), got, want)
	}
	return nil
}
