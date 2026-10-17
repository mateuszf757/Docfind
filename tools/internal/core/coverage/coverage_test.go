package coverage

import (
	"strings"
	"testing"
)

const module = "github.com/mateuszf757/Docfind/tools"

func TestParse(t *testing.T) {
	profile := "mode: atomic\n" +
		module + "/internal/stages/drain/drain.go:10.2,12.3 4 1\n" +
		module + "/internal/stages/drain/drain.go:14.2,15.3 6 0\n" +
		// Ten sam blok z drugiego binarnego testu (-coverpkg): wykonany tam.
		module + "/internal/stages/drain/drain.go:14.2,15.3 6 3\n" +
		module + "/internal/kit/cli/cli.go:1.1,2.2 10 0\n"
	packages, total, err := Parse([]byte(profile), module)
	if err != nil {
		t.Fatal(err)
	}
	if s := packages["internal/stages/drain"]; s.Statements != 10 || s.Covered != 10 {
		t.Errorf("drain: %+v", s)
	}
	if s := packages["internal/kit/cli"]; s.Percent() != 0 {
		t.Errorf("cli: %+v", s)
	}
	if total.Statements != 20 || total.Covered != 10 {
		t.Errorf("moduł: %+v", total)
	}
	if _, _, err := Parse([]byte("mode: set\nzły wiersz\n"), module); err == nil {
		t.Error("nieczytelny profil przyjęty")
	}
}

func TestBelow(t *testing.T) {
	packages := map[string]Stat{
		"internal/stages/drain":  {Statements: 100, Covered: 69},
		"internal/stages/deploy": {Statements: 100, Covered: 74},
		"internal/kit/cli":       {Statements: 10, Covered: 7},
		"cmd/dft":                {Statements: 100, Covered: 20},
	}
	found := Below(packages, map[string]float64{"stages": 70, "kit": 60})
	if len(found) != 1 || !strings.HasPrefix(found[0], "internal/stages/drain: pokrycie 69.0% poniżej 70% dla warstwy stages") {
		t.Errorf("Below = %q", found)
	}
}
