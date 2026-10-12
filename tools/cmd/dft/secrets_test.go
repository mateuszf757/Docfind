package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/kit/sensitive"
)

// TestRecoveryKeys: katalog 0700, plik 0600, klucz z poprzedniego klastra
// o tej samej nazwie odsunięty, a nie nadpisany; stan poza repozytorium
// (XDG_STATE_HOME).
func TestRecoveryKeys(t *testing.T) {
	quiet(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := stateDir("docfind")
	if !strings.HasSuffix(dir, filepath.Join("docfind", "docfind")) || !strings.HasPrefix(dir, os.Getenv("XDG_STATE_HOME")) {
		t.Fatalf("katalog stanu: %s", dir)
	}
	keys := recoveryKeys{dir: dir, cluster: "docfind"}
	path, err := keys.SaveRecovery([]sensitive.Value{sensitive.New("a2V5LTE=")})
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: %v, %v; oczekiwano %v", p, info.Mode().Perm(), err, want)
		}
	}
	if _, err := keys.SaveRecovery([]sensitive.Value{sensitive.New("a2V5LTI=")}); err != nil {
		t.Fatal(err)
	}
	old, _ := filepath.Glob(filepath.Join(dir, recoveryFile+".*.old"))
	if len(old) != 1 {
		t.Fatalf("poprzedni klucz nie odsunięty: %v", old)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cluster string   `json:"cluster"`
		Keys    []string `json:"recovery_keys_base64"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Cluster != "docfind" || len(doc.Keys) != 1 || doc.Keys[0] != "a2V5LTI=" {
		t.Errorf("plik klucza: %s, %v", raw, err)
	}
}
