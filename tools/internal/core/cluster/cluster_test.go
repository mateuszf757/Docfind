package cluster

import (
	"strings"
	"testing"
)

func TestPSAVersion(t *testing.T) {
	if v, err := PSAVersion("1.36.4"); err != nil || v != "v1.36" {
		t.Errorf("PSAVersion = %q, %v", v, err)
	}
	if _, err := PSAVersion("1.36"); err == nil {
		t.Error("wersja spoza X.Y.Z przyjęta")
	}
}

func TestRolloutComplete(t *testing.T) {
	done := Rollout{Wanted: 2, Updated: 2, Available: 2, Total: 2, Observed: true}
	if !done.Complete() {
		t.Error("pełny rollout niezakończony")
	}
	for name, r := range map[string]Rollout{
		"stara generacja":     {Wanted: 2, Updated: 2, Available: 2, Total: 2},
		"stary pod jeszcze":   {Wanted: 2, Updated: 2, Available: 2, Total: 3, Observed: true},
		"niedostępna replika": {Wanted: 2, Updated: 2, Available: 1, Total: 2, Observed: true},
	} {
		if r.Complete() {
			t.Errorf("%s: uznany za zakończony", name)
		}
	}
}

func TestSystemImages(t *testing.T) {
	airgap := []string{"docker.io/rancher/klipper-lb:v0.4.17", "docker.io/rancher/mirrored-library-busybox:1.37.0", "docker.io/library/busybox:1.37"}
	pods := []Pod{
		{Namespace: "kube-system", Name: "svclb", Images: map[string]string{"lb": "rancher/klipper-lb:v0.4.17"}},
		{Namespace: "docfind", Name: "api", Images: map[string]string{"api": "ghcr.io/x/docfind-api:c@sha256:abc"}},
		{Namespace: "x", Name: "b", Images: map[string]string{"b": "busybox:1.37"}},
	}
	if found := SystemImages(pods, nil, airgap, "ghcr.io/x/docfind-"); len(found) > 0 {
		t.Fatalf("obrazy z archiwum i z digestem odrzucone: %v", found)
	}
	// Nasz obraz po tagu (lokalny build, archiwum zadania build) — nie ta reguła.
	local := Pod{Namespace: "docfind", Name: "api-local", Images: map[string]string{"api": "ghcr.io/x/docfind-api:local-aaaa"}}
	found := SystemImages(append(pods, local, Pod{Namespace: "kube-system", Name: "metrics", Images: map[string]string{"m": "rancher/mirrored-metrics-server:v0.9.0"}}), nil, airgap, "ghcr.io/x/docfind-")
	if len(found) != 1 || !strings.Contains(found[0], "rancher/mirrored-metrics-server:v0.9.0 bez digestu") {
		t.Errorf("obraz spoza archiwum: %v", found)
	}
	found = SystemImages(pods, []string{"docker.io/rancher/klipper-lb:v0.4.17", "ghcr.io/x/docfind-api@sha256:abc"}, airgap, "ghcr.io/x/docfind-")
	if len(found) != 1 || !strings.Contains(found[0], "pobrany z sieci") {
		t.Errorf("obraz z archiwum pobierany z sieci: %v", found)
	}
}

func TestFullImage(t *testing.T) {
	for ref, want := range map[string]string{
		"busybox:1.37":                         "docker.io/library/busybox:1.37",
		"rancher/klipper-lb:v0.4.17":           "docker.io/rancher/klipper-lb:v0.4.17",
		"docker.io/rancher/klipper-lb:v0.4.17": "docker.io/rancher/klipper-lb:v0.4.17",
		"ghcr.io/x/api:1":                      "ghcr.io/x/api:1",
		"localhost/x:1":                        "localhost/x:1",
		"127.0.0.1:5000/x:1":                   "127.0.0.1:5000/x:1",
	} {
		if got := FullImage(ref); got != want {
			t.Errorf("FullImage(%s) = %s; oczekiwano %s", ref, got, want)
		}
	}
}
