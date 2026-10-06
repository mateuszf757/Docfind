package imagetest

import (
	"errors"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/cli"
	"github.com/mateuszf757/Docfind/tools/internal/proc"
)

func TestVerdict(t *testing.T) {
	exit1 := &proc.ExitError{Cmd: "docker buildx build", Code: 1}
	tests := []struct {
		name     string
		log      string
		err      error
		wantCode int
		want     string
	}{
		{"zielone testy", "#12 1.234 57 passed in 0.38s\n#12 DONE", nil, cli.ExitOK, "57 passed in 0.38s"},
		{"wynik z pamięci podręcznej", "#12 CACHED", nil, cli.ExitOK, "bez zmian od poprzedniego biegu (wynik z pamięci podręcznej)"},
		{
			"czerwony pytest",
			"#12 1.0 1 failed, 56 passed in 0.40s\nERROR: process \"/bin/sh -c /opt/venv/bin/python -m pytest -q -p no:cacheprovider\" did not complete successfully: exit code: 1",
			exit1, cli.ExitUnmet, "",
		},
		{
			// Brak sieci przy uv sync nie może wyglądać jak czerwony test.
			"awaria budowania",
			"ERROR: process \"/bin/sh -c uv sync --frozen\" did not complete successfully: exit code: 2",
			exit1, cli.ExitFailure, "",
		},
		{"docker nie uruchomił się", "", errors.New("brak dockera"), cli.ExitFailure, ""},
	}
	for _, tt := range tests {
		got, err := Verdict(tt.log, tt.err)
		if code := cli.ExitCode(err); code != tt.wantCode {
			t.Errorf("%s: kod %d, oczekiwano %d (%v)", tt.name, code, tt.wantCode, err)
		}
		if got != tt.want {
			t.Errorf("%s: wynik %q, oczekiwano %q", tt.name, got, tt.want)
		}
	}
}
