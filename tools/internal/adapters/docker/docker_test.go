package docker

import "testing"

func TestParseBuildxInspect(t *testing.T) {
	out := `Name:          rootless
Driver:        docker
Last Activity: 2026-10-06 21:00:00 +0000 UTC

Nodes:
Name:             rootless-node
Endpoint:         rootless
Status:           running
BuildKit version: v0.33.0
Platforms:        linux/amd64
`
	got := ParseBuildxInspect(out)
	want := Builder{Name: "rootless", Driver: "docker", BuildKitVersion: "v0.33.0"}
	if got != want {
		t.Errorf("ParseBuildxInspect = %+v, oczekiwano %+v", got, want)
	}
	if b := ParseBuildxInspect("nie to wyjście"); b.Driver != "" {
		t.Errorf("sterownik z nieczytelnego wyjścia: %q", b.Driver)
	}
}
