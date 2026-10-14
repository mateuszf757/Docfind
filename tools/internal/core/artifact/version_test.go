package artifact

import (
	"strings"
	"testing"
)

// TestVersion: reguła z decyzji 30 na samych faktach o commicie — bez gita.
// Zbieranie faktów z prawdziwego repozytorium sprawdza adapters/git.
func TestVersion(t *testing.T) {
	commit := "abc1234" + strings.Repeat("0", 33)
	tests := []struct {
		name string
		src  Source
		want string
	}{
		{"bez tagu", Source{Commit: commit, Commits: 12}, "0.0.0-dev.12+abc1234"},
		{"bez tagu, brudne", Source{Commit: commit, Commits: 12, Dirty: true}, "0.0.0-dev.12+abc1234-dirty"},
		{"na tagu", Source{Commit: commit, Tag: "v0.3.0"}, "0.3.0"},
		{"na tagu, brudne — nie wydanie", Source{Commit: commit, Tag: "v0.3.0", Dirty: true}, "0.3.1-dev.0+abc1234-dirty"},
		{"za tagiem", Source{Commit: commit, Tag: "v0.3.0", Distance: 5}, "0.3.1-dev.5+abc1234"},
		{"łatka 9 → 10", Source{Commit: commit, Tag: "v1.2.9", Distance: 1}, "1.2.10-dev.1+abc1234"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Version(tt.src)
			if err != nil || got != tt.want {
				t.Errorf("Version = %q, %v; oczekiwano %q", got, err, tt.want)
			}
		})
	}
	for _, bad := range []Source{{Commit: "abc"}, {Commit: commit, Tag: "v0.4.0-rc.1"}} {
		if _, err := Version(bad); err == nil {
			t.Errorf("Version(%+v) przyjęte", bad)
		}
	}
}

func TestIsReleaseTag(t *testing.T) {
	for tag, want := range map[string]bool{"v0.3.0": true, "v10.20.30": true, "v01.2.3": false, "v0.4.0-rc.1": false, "0.3.0": false} {
		if got := IsReleaseTag(tag); got != want {
			t.Errorf("IsReleaseTag(%q) = %v", tag, got)
		}
	}
}

func TestReleaseNumbers(t *testing.T) {
	if v, ok := ReleaseOfTag("v0.4.0"); !ok || v != "0.4.0" {
		t.Errorf("ReleaseOfTag: %q, %v", v, ok)
	}
	if _, ok := ReleaseOfTag("v0.4.0-rc.1"); ok {
		t.Error("tag przedwydaniowy jako wydanie")
	}
	tests := []struct {
		a, b string
		want int
	}{
		{"0.4.0", "0.3.9", 1},
		{"0.3.10", "0.3.9", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.3.1", "0.3.1", 0},
		{"0.3.1", "0.4.0", -1},
	}
	for _, tt := range tests {
		if got := CompareReleases(tt.a, tt.b); got != tt.want {
			t.Errorf("CompareReleases(%s, %s) = %d, oczekiwano %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestImageName(t *testing.T) {
	tests := []struct{ registry, owner, want string }{
		{"", "MateuszF757", "ghcr.io/mateuszf757/docfind-api"},
		{"", "", "ghcr.io/mateuszf757/docfind-api"},
		{"127.0.0.1:5000/docfind/", "MateuszF757", "127.0.0.1:5000/docfind/docfind-api"},
	}
	for _, tt := range tests {
		if got := ImageName(tt.registry, tt.owner, "api"); got != tt.want {
			t.Errorf("ImageName(%q, %q) = %q, oczekiwano %q", tt.registry, tt.owner, got, tt.want)
		}
		// Chart w tym samym rejestrze co obraz (decyzja 45).
		if got, want := ChartName(tt.registry, tt.owner), strings.TrimSuffix(tt.want, "/docfind-api")+"/charts/docfind"; got != want {
			t.Errorf("ChartName(%q, %q) = %q, oczekiwano %q", tt.registry, tt.owner, got, want)
		}
	}
}

func TestIsRelease(t *testing.T) {
	for version, want := range map[string]bool{
		"0.3.0": true, "10.20.30": true,
		"0.3.1-dev.4+abc1234": false, "0.3.0-dirty": false, "01.2.3": false, "0.3": false, "v0.3.0": false,
	} {
		if got := IsRelease(version); got != want {
			t.Errorf("IsRelease(%q) = %v, oczekiwano %v", version, got, want)
		}
	}
}
