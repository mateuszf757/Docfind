package cluster

import "testing"

func TestPSAVersion(t *testing.T) {
	if v, err := PSAVersion("1.36.4"); err != nil || v != "v1.36" {
		t.Errorf("PSAVersion = %q, %v", v, err)
	}
	if _, err := PSAVersion("1.36"); err == nil {
		t.Error("wersja spoza X.Y.Z przyjęta")
	}
}

func TestRolloutComplete(t *testing.T) {
	done := Rollout{Wanted: 2, Updated: 2, Available: 2, Total: 2, Observed: true}
	if !done.Complete() {
		t.Error("pełny rollout niezakończony")
	}
	for name, r := range map[string]Rollout{
		"stara generacja":     {Wanted: 2, Updated: 2, Available: 2, Total: 2},
		"stary pod jeszcze":   {Wanted: 2, Updated: 2, Available: 2, Total: 3, Observed: true},
		"niedostępna replika": {Wanted: 2, Updated: 2, Available: 1, Total: 2, Observed: true},
	} {
		if r.Complete() {
			t.Errorf("%s: uznany za zakończony", name)
		}
	}
}
