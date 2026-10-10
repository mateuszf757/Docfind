package imagetest

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/artifact"
	"github.com/mateuszf757/Docfind/tools/internal/kit/cli"
)

func TestVerdict(t *testing.T) {
	exit1 := errors.New("docker buildx build zakończył się kodem 1")
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
		{"przerwany", "#12 1 failed did not complete successfully", context.Canceled, cli.ExitFailure, ""},
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

type builder struct{ req artifact.BuildRequest }

func (b *builder) Build(_ context.Context, req artifact.BuildRequest) (string, error) {
	b.req = req
	_, err := io.WriteString(req.Out, "#12 1.234 57 passed in 0.38s\n")
	return "", err
}

// TestRun: etap test z kontekstem konfiguracji i tym samym
// SOURCE_DATE_EPOCH co obraz, wynik tylko w pamięci podręcznej.
func TestRun(t *testing.T) {
	oldOut := cli.Out
	cli.Out = io.Discard
	defer func() { cli.Out = oldOut }()
	b := &builder{}
	if err := Run(context.Background(), b, "../../../..", "api", 1790856000, io.Discard); err != nil {
		t.Fatal(err)
	}
	r := b.req
	if r.Target != "test" || !r.CacheOnly || len(r.Contexts) != 1 || r.Contexts[0].Name != "config" || !strings.HasSuffix(r.Contexts[0].Dir, "deploy/config") || r.Args[0].Value != "1790856000" {
		t.Errorf("żądanie builda %+v", r)
	}
	if err := Run(context.Background(), b, "../../../..", "brak", 0, io.Discard); cli.ExitCode(err) != cli.ExitFailure {
		t.Errorf("nieznana usługa: %v", err)
	}
}
