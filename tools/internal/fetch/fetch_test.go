package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

func TestURL(t *testing.T) {
	content := "chart"
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()
	sum := sha256.Sum256([]byte(content))
	want := hex.EncodeToString(sum[:])
	root := t.TempDir()

	path, err := URL(context.Background(), root, server.URL+"/chart-1.0.0.tgz", want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := URL(context.Background(), root, server.URL+"/chart-1.0.0.tgz", want); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("pobrań %d — drugi raz plik ma przyjść z pamięci podręcznej", hits)
	}

	// Plik podmieniony w pamięci podręcznej: odrzucony i usunięty.
	if err := os.WriteFile(path, []byte("podmiana"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = URL(context.Background(), root, server.URL+"/chart-1.0.0.tgz", want)
	if cli.ExitCode(err) != cli.ExitUnmet || !strings.Contains(err.Error(), "nie zgadza się") {
		t.Fatalf("podmieniony plik przyjęty: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("plik z niezgodną sumą został w pamięci podręcznej")
	}
}
