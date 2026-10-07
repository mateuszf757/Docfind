package pins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot — testy biegną w katalogu pakietu, trzy poziomy pod korzeniem.
const repoRoot = "../../.."

// TestRepositoryPins: prawdziwy ci/pins.env przechodzi walidację. Literówka
// w kluczu albo digest bez sha256 wychodzi w zadaniu test, a nie w połowie
// tworzenia klastra.
func TestRepositoryPins(t *testing.T) {
	p, err := Load(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Get("DF_BUILDKIT_IMAGE"); !strings.HasPrefix(got, "moby/buildkit:") {
		t.Errorf("DF_BUILDKIT_IMAGE = %q", got)
	}
}

func validPins() string {
	data, err := os.ReadFile(filepath.Join(repoRoot, File))
	if err != nil {
		panic(err)
	}
	return string(data)
}

func TestParseRejects(t *testing.T) {
	base := validPins()
	tests := []struct {
		name   string
		mutate func(string) string
		want   string
	}{
		{
			name:   "nieznany klucz",
			mutate: func(s string) string { return s + "DF_NOWY=1\n" },
			want:   "nieznane przypięcie DF_NOWY",
		},
		{
			name:   "brak klucza",
			mutate: func(s string) string { return strings.Replace(s, "DF_BASE_ALPINE=3.24\n", "", 1) },
			want:   "brak przypięcia DF_BASE_ALPINE",
		},
		{
			name: "obraz bez digestu",
			mutate: func(s string) string {
				return strings.Replace(s, "DF_BUILDKIT_IMAGE=moby/buildkit:v0.33.0@sha256:6c2fa84a6b61ccd72899dde4239f8d5717f05f9a8ca6f3cad185fb1a95a94de3", "DF_BUILDKIT_IMAGE=moby/buildkit:v0.33.0", 1)
			},
			want: "DF_BUILDKIT_IMAGE=moby/buildkit:v0.33.0 — oczekiwano: obraz z digestem",
		},
		{
			name: "URL charta bez wersji",
			mutate: func(s string) string {
				return strings.Replace(s, "DF_COREDNS_CHART_VERSION=1.47.1", "DF_COREDNS_CHART_VERSION=1.48.0", 1)
			},
			want: "DF_COREDNS_CHART_URL nie zawiera wersji",
		},
		{
			name:   "cudzysłowy",
			mutate: func(s string) string { return strings.Replace(s, "DF_BASE_ALPINE=3.24", `DF_BASE_ALPINE="3.24"`, 1) },
			want:   "Bash zinterpretowałby inaczej",
		},
		{
			name: "podstawienie polecenia",
			mutate: func(s string) string {
				return strings.Replace(s, "DF_BASE_ALPINE=3.24", "DF_BASE_ALPINE=$(id)", 1)
			},
			want: "Bash zinterpretowałby inaczej",
		},
		{
			name:   "drugi raz ten sam klucz",
			mutate: func(s string) string { return s + "DF_BASE_ALPINE=3.24\n" },
			want:   "zdefiniowane drugi raz",
		},
		{
			name:   "linia bez znaku równości",
			mutate: func(s string) string { return s + "DF_BASE_ALPINE\n" },
			want:   "oczekiwano KEY=VALUE",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.mutate(base)))
			if err == nil {
				t.Fatal("oczekiwano odrzucenia")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("błąd %q nie zawiera %q", err, tt.want)
			}
		})
	}
}

func TestImageTag(t *testing.T) {
	tests := map[string]string{
		"moby/buildkit:v0.33.0@sha256:6c2f":    "v0.33.0",
		"localhost:5000/obraz:1.0@sha256:abcd": "1.0",
		"localhost:5000/obraz@sha256:abcd":     "",
		"obraz":                                "",
	}
	for image, want := range tests {
		if got := ImageTag(image); got != want {
			t.Errorf("ImageTag(%q) = %q, oczekiwano %q", image, got, want)
		}
	}
}

func TestRepositoryVersionSources(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join(repoRoot, Dockerfile))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := UVVersion(dockerfile); err != nil || v == "" {
		t.Errorf("UVVersion: %q, %v", v, err)
	}
	if minors := PythonMinors(dockerfile); len(minors) != 1 {
		t.Errorf("PythonMinors: %v — etapy Dockerfile mają mieć jedną bazę Pythona", minors)
	}
	gomod, err := os.ReadFile(filepath.Join(repoRoot, GoMod))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := GoDirective(gomod); err != nil || v == "" {
		t.Errorf("GoDirective: %q, %v", v, err)
	}
}

func TestVersionSources(t *testing.T) {
	dockerfile := []byte("FROM ghcr.io/astral-sh/uv:0.12.21@sha256:" + strings.Repeat("a", 64) + " AS uv\n" +
		"FROM python:3.14-alpine@sha256:" + strings.Repeat("b", 64) + " AS builder\n" +
		"FROM python:3.14.7-alpine AS test\n" +
		"FROM python:3.13 AS old\n")
	if v, err := UVVersion(dockerfile); err != nil || v != "0.12.21" {
		t.Errorf("UVVersion = %q, %v", v, err)
	}
	if got := strings.Join(PythonMinors(dockerfile), ","); got != "3.13,3.14" {
		t.Errorf("PythonMinors = %q", got)
	}
	if _, err := UVVersion([]byte("FROM ghcr.io/astral-sh/uv:0.12.21 AS uv\n")); err == nil {
		t.Error("uv bez digestu przyjęty")
	}
	if v, err := PythonVersion([]byte("3.14\n")); err != nil || v != "3.14" {
		t.Errorf("PythonVersion = %q, %v", v, err)
	}
	if _, err := PythonVersion([]byte("3.14.7\n")); err == nil {
		t.Error(".python-version z łatką przyjęty — ma być major.minor")
	}
	if v, err := GoDirective([]byte("module x\n\ngo 1.27.1\n")); err != nil || v != "1.27.1" {
		t.Errorf("GoDirective = %q, %v", v, err)
	}
}
