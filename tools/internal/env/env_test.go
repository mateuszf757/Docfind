package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const repoRoot = "../../.."

func TestLoadRepositoryEnvironments(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot, Dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("brak definicji środowisk: %v", err)
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		if _, err := Load(repoRoot, name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOverridesOnlyWhenAllowed(t *testing.T) {
	t.Setenv("DOCFIND_HOSTNAME", "local.example.com")
	t.Setenv("DOCFIND_TLS_ISSUER", "letsencrypt-staging")
	dev, err := Load(repoRoot, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if dev.Gateway.Hostname != "local.example.com" || dev.Gateway.Issuer != "letsencrypt-staging" {
		t.Errorf("dev bez nadpisań z mise.local.toml: %+v", dev.Gateway)
	}
	e := dev
	e.Gateway.OverridesFromEnvironment = false
	e.Gateway.Hostname = "docfind.internal"
	e.applyOverrides(os.Getenv)
	if e.Gateway.Hostname != "docfind.internal" {
		t.Errorf("nadpisanie mimo overridesFromEnvironment: false: %q", e.Gateway.Hostname)
	}
}

func write(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, Dir, "test.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLoadRejects(t *testing.T) {
	// Zmienne z mise.local.toml autora nadpisałyby błędne wartości w definicji
	// testowej, która — jak dev — dopuszcza nadpisania.
	for _, key := range []string{"DOCFIND_HOSTNAME", "DOCFIND_TLS_ISSUER", "DOCFIND_ACME_EMAIL", "DOCFIND_ACME_ZONE"} {
		t.Setenv(key, "")
	}
	devRaw, err := os.ReadFile(filepath.Join(repoRoot, Dir, "dev.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	base := strings.Replace(string(devRaw), "name: dev", "name: test", 1)
	tests := []struct{ name, from, to, want string }{
		{"literówka w polu", "  release: docfind", "  relase: docfind", "unknown field"},
		{"port uprzywilejowany", "    https: 8443", "    https: 443", "cluster.ports.https=443"},
		{"serwer API inny niż port", "apiServer: https://127.0.0.1:6550", "apiServer: https://127.0.0.1:7000", "cluster.apiServer"},
		{"serwer API poza loopbackiem", "apiServer: https://127.0.0.1:6550", "apiServer: https://0.0.0.0:6550", "cluster.apiServer"},
		{"nieznany wydawca", "  issuer: docfind-internal-ca", "  issuer: selfsigned", "gateway.issuer"},
		{"nieznana dopuszczalność", "  destructive: allowed", "  destructive: sometimes", "operations.destructive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := strings.Replace(base, tt.from, tt.to, 1)
			if content == base {
				t.Fatalf("podmiana %q nie zadziałała", tt.from)
			}
			_, err := Load(write(t, content), "test")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("oczekiwano błędu z %q, jest %v", tt.want, err)
			}
		})
	}
}
