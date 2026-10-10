package layers

import (
	"strings"
	"testing"
)

const module = "example.com/narzedzia"

func TestOf(t *testing.T) {
	tests := []struct {
		dir  string
		want Layer
		ok   bool
	}{
		{"internal/kit/cli", Kit, true},
		{"internal/core/artifact", Core, true},
		{"internal/adapters/kube", Adapters, true},
		{"internal/stages/drain", Stages, true},
		{"cmd/dft", Cmd, true},
		{"internal/drain", "", false},
		{"internal/cmd/x", "", false},
		{"internal/stages", "", false},
		{"tools", "", false},
	}
	for _, tt := range tests {
		if got, ok := Of(tt.dir); got != tt.want || ok != tt.ok {
			t.Errorf("Of(%q) = %q, %v; oczekiwano %q, %v", tt.dir, got, ok, tt.want, tt.ok)
		}
	}
}

// TestCheck: każda reguła ma wariant, który przechodzi, i taki, który jest
// odrzucany — reguła, której nie da się złamać, niczego nie pilnuje.
func TestCheck(t *testing.T) {
	m := func(rel string) string { return module + "/" + rel }
	tests := []struct {
		name    string
		pkg     Package
		wantMsg string
	}{
		{"cmd składa wszystko", Package{"cmd/dft", []string{m("internal/stages/drain"), m("internal/adapters/kube"), "k8s.io/client-go/kubernetes", "os/exec"}}, ""},
		{"etap na portach", Package{"internal/stages/drain", []string{m("internal/core/cluster"), m("internal/kit/cli"), m("internal/stages/identity"), "k8s.io/api/core/v1", "context", "net/url"}}, ""},
		{"adapter na kit i core", Package{"internal/adapters/kube", []string{m("internal/core/cluster"), m("internal/kit/proc"), "k8s.io/client-go/kubernetes", "net/http"}}, ""},
		{"etap importuje adapter", Package{"internal/stages/drain", []string{m("internal/adapters/kube")}}, "etapy nie importują adapterów"},
		{"etap uruchamia programy", Package{"internal/stages/deploy", []string{m("internal/kit/proc")}}, "mechanizm wejścia-wyjścia"},
		{"etap pobiera pliki", Package{"internal/stages/charts", []string{m("internal/kit/fetch")}}, "mechanizm wejścia-wyjścia"},
		{"etap z os/exec", Package{"internal/stages/x", []string{"os/exec"}}, "należy do adaptera"},
		{"etap z siecią", Package{"internal/stages/tls", []string{"net/http/httptrace"}}, "należy do adaptera"},
		{"etap z client-go", Package{"internal/stages/x", []string{"k8s.io/client-go/kubernetes"}}, "spoza listy dozwolonych"},
		{"rdzeń z rejestrem", Package{"internal/core/x", []string{"github.com/google/go-containerregistry/pkg/v1/remote"}}, "spoza listy dozwolonych"},
		{"rdzeń importuje etap", Package{"internal/core/x", []string{m("internal/stages/build")}}, "warstwa core importuje tylko"},
		{"kit importuje rdzeń", Package{"internal/kit/x", []string{m("internal/core/artifact")}}, "warstwa kit importuje tylko"},
		{"adapter importuje adapter", Package{"internal/adapters/k3d", []string{m("internal/adapters/docker")}}, "jeden system zewnętrzny"},
		{"adapter importuje etap", Package{"internal/adapters/x", []string{m("internal/stages/build")}}, "warstwa adapters importuje tylko"},
		{"import z cmd", Package{"internal/stages/x", []string{m("cmd/dft")}}, "nie jest biblioteką"},
		{"pakiet poza warstwami", Package{"internal/drain", nil}, "pakiet poza warstwami"},
		{"import pakietu poza warstwami", Package{"cmd/dft", []string{m("internal/drain")}}, "pakiet poza warstwami"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := Check(module, []Package{tt.pkg})
			switch {
			case tt.wantMsg == "" && len(problems) > 0:
				t.Errorf("odrzucone: %v", problems)
			case tt.wantMsg != "" && (len(problems) != 1 || !strings.Contains(problems[0], tt.wantMsg)):
				t.Errorf("oczekiwano jednego naruszenia z %q, jest %v", tt.wantMsg, problems)
			}
		})
	}
}
