// Package fetch pobiera artefakty spoza repozytorium (charty) do pamięci
// podręcznej w .cache/downloads i sprawdza ich sumę przy każdym użyciu,
// a nie tylko przy pobraniu — plik w pamięci podręcznej też może zostać
// podmieniony albo uszkodzony (dawniej df_fetch_verified w ci/lib.sh).
//
// Suma pochodzi z ci/pins.env, a nie z serwera, z którego idzie plik: suma
// ściągnięta z tego samego miejsca chroni tylko przed uszkodzeniem
// w transferze, a nie przed podmianą. Mechanizm adapterów (decyzja 39) —
// etapy dostają pliki przez porty (adapters/helm).
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

	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
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
	return file, Verify(file, sha)
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

// Verify porównuje sumę pliku z przypiętą; niezgodny plik jest usuwany,
// żeby kolejny bieg pobrał go od nowa, zamiast powtarzać błąd. Także dla
// plików pobranych inną drogą (`helm pull` z rejestru OCI), która sama nie
// weryfikuje niczego poza tym, co poda serwer.
func Verify(file, want string) error {
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
