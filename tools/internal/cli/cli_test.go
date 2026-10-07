package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"brak błędu", nil, ExitOK},
		{"warunek niespełniony", Unmet("rozmiar"), ExitUnmet},
		{"opakowany warunek niespełniony", fmt.Errorf("bramka: %w", Unmet("rozmiar")), ExitUnmet},
		{"awaria narzędzia", errors.New("brak dockera"), ExitFailure},
		{"złe wywołanie", Usage("nieznane polecenie"), ExitFailure},
	}
	for _, tt := range tests {
		if got := ExitCode(tt.err); got != tt.want {
			t.Errorf("%s: ExitCode = %d, oczekiwano %d", tt.name, got, tt.want)
		}
	}
}

func TestReport(t *testing.T) {
	var buf bytes.Buffer
	old := Err
	Err = &buf
	defer func() { Err = old }()

	Report(Unmet("obraz za duży"))
	Report(errors.New("brak dockera"))
	want := "NIESPEŁNIONE: obraz za duży\nBŁĄD: brak dockera\n"
	if buf.String() != want {
		t.Errorf("Report wypisał %q, oczekiwano %q", buf.String(), want)
	}
}

func TestVerdict(t *testing.T) {
	var buf bytes.Buffer
	old := Err
	Err = &buf
	defer func() { Err = old }()

	v := &Verdict{}
	if err := v.Err("bramka"); err != nil {
		t.Fatalf("pusty werdykt dał błąd: %v", err)
	}
	v.Fail("pierwszy %d", 1)
	v.Fail("drugi")
	err := v.Err("bramka")
	if ExitCode(err) != ExitUnmet {
		t.Fatalf("kod %d, oczekiwano %d", ExitCode(err), ExitUnmet)
	}
	if !strings.Contains(err.Error(), "2 niespełnionych") {
		t.Errorf("podsumowanie %q", err)
	}
	if got := buf.String(); got != "NIESPEŁNIONE: pierwszy 1\nNIESPEŁNIONE: drugi\n" {
		t.Errorf("Fail wypisał %q", got)
	}
}
