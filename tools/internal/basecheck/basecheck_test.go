package basecheck

import (
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
)

func TestParseAPKInstalled(t *testing.T) {
	db := "C:Q1abc=\nP:musl\nV:1.2.6-r2\nA:x86_64\n\nC:Q1def=\nP:libssl3\nV:3.5.8-r0\n\nP:busybox\nV:1.37.0-r30\n"
	got := ParseAPKInstalled([]byte(db))
	want := map[string]string{"musl": "1.2.6-r2", "libssl3": "3.5.8-r0", "busybox": "1.37.0-r30"}
	for name, version := range want {
		if got[name] != version {
			t.Errorf("%s = %q, oczekiwano %q", name, got[name], version)
		}
	}
}

func TestCheck(t *testing.T) {
	base := Facts{Alpine: "3.24.2", Python: "3.14.7", Musl: "1.2.6-r2", OpenSSL: "3.5.8-r0"}
	tests := []struct {
		name     string
		facts    Facts
		wantCode int
		wantMsg  string
	}{
		{"zgodna", base, cli.ExitOK, ""},
		// Nowe wydanie Alpine pod tym samym tagiem bazy — odświeżenie digestu
		// od Dependabota, które nie jest łatką.
		{"nowe wydanie Alpine", Facts{Alpine: "3.25.0", Python: "3.14.7"}, cli.ExitUnmet, "Alpine 3.25.0"},
		{"inny Python niż testy", Facts{Alpine: "3.24.2", Python: "3.15.0"}, cli.ExitUnmet, "Pythona 3.15.0"},
	}
	for _, tt := range tests {
		var out strings.Builder
		old := cli.Err
		cli.Err = &out
		err := Check(tt.facts, "3.24", "3.14")
		cli.Err = old
		if code := cli.ExitCode(err); code != tt.wantCode {
			t.Errorf("%s: kod %d, oczekiwano %d", tt.name, code, tt.wantCode)
		}
		if tt.wantMsg != "" && !strings.Contains(out.String(), tt.wantMsg) {
			t.Errorf("%s: komunikat %q nie zawiera %q", tt.name, out.String(), tt.wantMsg)
		}
	}
}
