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
