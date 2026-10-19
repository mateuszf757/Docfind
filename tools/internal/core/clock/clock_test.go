package clock

import (
	"strings"
	"testing"
	"time"
)

func TestProblem(t *testing.T) {
	for skew, want := range map[time.Duration]string{
		1600 * time.Millisecond: "",
		-59 * time.Second:       "",
		5 * time.Minute:         "spieszy się o 5m0s",
		-2 * time.Minute:        "spóźnia się o 2m0s",
	} {
		got := Problem(skew)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("Problem(%v) = %q; oczekiwano %q", skew, got, want)
		}
	}
}
