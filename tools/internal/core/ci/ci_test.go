package ci

import (
	"slices"
	"testing"
)

func TestUnmet(t *testing.T) {
	for _, tt := range []struct {
		name  string
		needs map[string]Need
		want  []string
	}{
		{"wszystkie zielone", map[string]Need{"test": {"success"}, "build": {"success"}}, nil},
		{"pominięte to nie zaliczone", map[string]Need{"test": {"success"}, "build": {"skipped"}}, []string{"build: skipped"}},
		{"porażka i anulowanie, w kolejności nazw", map[string]Need{"test": {"failure"}, "build": {"cancelled"}, "tools": {"success"}}, []string{"build: cancelled", "test: failure"}},
		{"brak wyniku", map[string]Need{"test": {}}, []string{"test: "}},
	} {
		if got := Unmet(tt.needs); !slices.Equal(got, tt.want) {
			t.Errorf("%s: Unmet = %q; oczekiwano %q", tt.name, got, tt.want)
		}
	}
}

func TestSetupStep(t *testing.T) {
	for name, want := range map[string]bool{
		"Set up job":            true,
		"Initialize containers": true,
		"Run actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1": true,
		"narzędzia (mise)":             true,
		"Go i gh (mise)":               true,
		"dft":                          true,
		"pamięć podręczna Go (odczyt)": true,
		"przypięte wersje":             false,
		"wynik warstwy W1":             false,
		"Complete job":                 false,
		"Run actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9": false,
	} {
		if got := SetupStep(name); got != want {
			t.Errorf("SetupStep(%q) = %v, oczekiwano %v", name, got, want)
		}
	}
}
