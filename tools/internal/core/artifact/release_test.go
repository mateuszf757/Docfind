package artifact

import (
	"strings"
	"testing"
)

func release() Release {
	return Release{
		Version:    "0.4.0",
		Title:      "Etap 4: indeksowanie",
		Image:      repo + "@" + digest,
		Commit:     strings.Repeat("c", 40),
		AcceptedAt: "2026-10-12T10:00:00Z",
		Staging:    "tożsamość na 2 podach; drain 3 węzłów, 1765 żądań, 0 nieudanych",
	}
}

// TestReleaseTagRoundTrip: to, co `dft release` zapisuje w tagu, CI odczytuje
// bez strat — także z podpisem dopisanym na końcu treści.
func TestReleaseTagRoundTrip(t *testing.T) {
	want := release()
	got, err := ParseReleaseTag("v0.4.0", want.TagMessage()+"-----BEGIN PGP SIGNATURE-----\nabc\n-----END PGP SIGNATURE-----\n")
	if err != nil || got != want {
		t.Fatalf("odczyt %+v, %v; oczekiwano %+v", got, err, want)
	}
	if !strings.HasPrefix(want.TagMessage(), "Etap 4: indeksowanie\n\nAkceptacja na stagingu: ") {
		t.Errorf("treść tagu:\n%s", want.TagMessage())
	}
	untitled := want
	untitled.Title = ""
	if got, _ := ParseReleaseTag("v0.4.0", untitled.TagMessage()); got.Title != "Wydanie 0.4.0" {
		t.Errorf("tytuł domyślny: %q", got.Title)
	}
}

func TestParseReleaseTagRejects(t *testing.T) {
	msg := release().TagMessage()
	tests := []struct{ name, tag, message, want string }{
		{"tag spoza vX.Y.Z", "v0.4.0-rc.1", msg, "spoza vX.Y.Z"},
		{"tag ręczny bez przyczep", "v0.4.0", "Etap 4: indeksowanie\n", "bez digestu"},
		{"obraz po tagu", "v0.4.0", strings.Replace(msg, "@"+digest, ":0.4.0", 1), "bez digestu"},
		{"skrócony commit", "v0.4.0", strings.Replace(msg, strings.Repeat("c", 40), "ccccccc", 1), "pełnym SHA"},
		{"bez wyników akceptacji", "v0.4.0", strings.Replace(msg, "Akceptacja na stagingu: ", "Uwagi: ", 1), "brak wyników akceptacji"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseReleaseTag(tt.tag, tt.message); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("oczekiwano odrzucenia z %q, jest %v", tt.want, err)
			}
		})
	}
}
