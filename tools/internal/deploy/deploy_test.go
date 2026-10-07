package deploy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/env"
)

const repoRoot = "../../.."

func TestK3dConfig(t *testing.T) {
	e := env.Environment{Cluster: env.Cluster{Name: "docfind-ci", Ports: env.Ports{API: 6551, HTTP: 8081, HTTPS: 8444}}}
	raw, err := K3dConfig(filepath.Join(repoRoot, "deploy", "k3d", "cluster.yaml"), e)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{`"name": "docfind-ci"`, `"hostPort": "6551"`, `"127.0.0.1:8081:80"`, `"127.0.0.1:8444:443"`, `"--disable=traefik"`} {
		if !strings.Contains(out, want) {
			t.Errorf("konfiguracja bez %s:\n%s", want, out)
		}
	}
}

// TestK3dConfigRejectsWildcard: mapowanie na 0.0.0.0 to klaster osiągalny
// z sieci — odrzucone, a nie przepisane po cichu (decyzja 17).
func TestK3dConfigRejectsWildcard(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "k3d", "cluster.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct{ from, to string }{
		{"port: 127.0.0.1:8080:80", "port: 0.0.0.0:8080:80"},
		{"  hostIP: 127.0.0.1", "  hostIP: 0.0.0.0"},
	} {
		path := filepath.Join(t.TempDir(), "cluster.yaml")
		bad := strings.Replace(string(src), mutation.from, mutation.to, 1)
		if bad == string(src) {
			t.Fatalf("podmiana %q nie zadziałała", mutation.from)
		}
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := K3dConfig(path, env.Environment{Cluster: env.Cluster{Name: "x", Ports: env.Ports{API: 6550, HTTP: 8080, HTTPS: 8443}}})
		if cli.ExitCode(err) != cli.ExitUnmet {
			t.Errorf("%s: przyjęte (%v)", mutation.to, err)
		}
	}
}
