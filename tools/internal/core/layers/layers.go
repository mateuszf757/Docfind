// Package layers to reguły architektury kodu narzędzi (decyzja 39): każdy
// pakiet należy do jednej warstwy, a warstwa importuje tylko to, na co jej
// pozwala kierunek zależności.
//
//	cmd      → stages, adapters, core, kit   składa program z części
//	stages   → stages, core, kit             etapy i bramki: decyzje na portach
//	adapters → core, kit                     systemy zewnętrzne za portami
//	core     → core, kit                     typy i reguły domeny, bez wejścia-wyjścia
//	kit      → kit                           narzędzia bez wiedzy o domenie
//
// Etapy nigdy nie importują adapterów: o świecie zewnętrznym wiedzą tylko
// tyle, ile mówią ich porty (interfejsy w pakiecie etapu, na typach z core),
// a adapter podstawia cmd. Rdzeń i etapy nie sięgają też po mechanizmy
// wejścia-wyjścia, którymi adapter rozmawia ze światem — uruchamianie
// programów, sieć, client-go, rejestr obrazów: etap, który uruchamia
// kubectl, robi pracę adaptera, nawet jeśli nie importuje żadnego.
//
// Reguły obejmują kod produkcyjny; testy mogą składać etap z prawdziwymi
// adapterami na atrapach.
package layers

import (
	"fmt"
	"slices"
	"strings"
)

// Layer — warstwa kodu.
type Layer string

// Warstwy.
const (
	Kit      Layer = "kit"
	Core     Layer = "core"
	Adapters Layer = "adapters"
	Stages   Layer = "stages"
	Cmd      Layer = "cmd"
)

// may — warstwy wewnętrzne, które warstwa może importować. Adaptery nie
// importują siebie nawzajem: każdy to jeden system zewnętrzny, a składa je
// cmd.
var may = map[Layer][]Layer{
	Kit:      {Kit},
	Core:     {Kit, Core},
	Adapters: {Kit, Core},
	Stages:   {Kit, Core, Stages},
	Cmd:      {Kit, Core, Adapters, Stages},
}

// ioKit — pakiety kit, które są mechanizmem wejścia-wyjścia adapterów.
var ioKit = []string{"internal/kit/proc", "internal/kit/fetch"}

// ioStd — biblioteka standardowa wejścia-wyjścia poza plikami repozytorium:
// procesy, sieć, wywołania systemowe.
var ioStd = []string{"os/exec", "net", "net/http", "syscall"}

// pureExternal — biblioteki spoza modułu, których mogą używać rdzeń
// i etapy: typy Kubernetesa i nazw obrazów, YAML. Klienci (client-go,
// zdalny rejestr) to adaptery. Dwa YAML-e: sigs.k8s.io/yaml do odczytu
// przez typy Go, go.yaml.in/yaml/v3 do edycji drzewa z zachowaniem
// komentarzy (pakowanie charta, decyzja 45) — tej sigs.k8s.io/yaml nie
// umie, a sam korzysta z go.yaml.in/yaml pod spodem.
var pureExternal = []string{
	"k8s.io/api/",
	"k8s.io/apimachinery/",
	"sigs.k8s.io/yaml",
	"go.yaml.in/yaml/v3",
	"github.com/google/go-containerregistry/pkg/name",
}

// Package — pakiet modułu: ścieżka względem modułu i importy z plików
// produkcyjnych (bez _test.go).
type Package struct {
	Dir     string
	Imports []string
}

// Of zwraca warstwę pakietu o ścieżce względem modułu: internal/<warstwa>/…
// albo cmd/….
func Of(dir string) (Layer, bool) {
	parts := strings.Split(dir, "/")
	switch {
	case len(parts) >= 2 && parts[0] == "cmd":
		return Cmd, true
	case len(parts) >= 3 && parts[0] == "internal":
		l := Layer(parts[1])
		if _, ok := may[l]; ok && l != Cmd {
			return l, true
		}
	}
	return "", false
}

// Check zwraca naruszenia reguł w pakietach modułu module, posortowane.
func Check(module string, pkgs []Package) []string {
	var problems []string
	for _, p := range pkgs {
		layer, ok := Of(p.Dir)
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: pakiet poza warstwami — internal/{kit,core,adapters,stages}/<nazwa> albo cmd/<nazwa>", p.Dir))
			continue
		}
		for _, imp := range p.Imports {
			if msg := check(module, layer, imp); msg != "" {
				problems = append(problems, fmt.Sprintf("%s (%s) importuje %s — %s", p.Dir, layer, imp, msg))
			}
		}
	}
	slices.Sort(problems)
	return problems
}

func check(module string, layer Layer, imp string) string {
	if rel, ok := strings.CutPrefix(imp, module+"/"); ok {
		target, ok := Of(rel)
		if !ok {
			return "pakiet poza warstwami"
		}
		if !slices.Contains(may[layer], target) {
			return reason(layer, target)
		}
		if (layer == Core || layer == Stages) && slices.Contains(ioKit, rel) {
			return "mechanizm wejścia-wyjścia adapterów; potrzebę wyraża port"
		}
		return ""
	}
	if layer != Core && layer != Stages {
		return ""
	}
	first, _, _ := strings.Cut(imp, "/")
	if !strings.Contains(first, ".") {
		// Biblioteka standardowa.
		if slices.Contains(ioStd, imp) || strings.HasPrefix(imp, "net/http/") {
			return "wejście-wyjście poza plikami repozytorium należy do adaptera"
		}
		return ""
	}
	for _, allowed := range pureExternal {
		if imp == strings.TrimSuffix(allowed, "/") || strings.HasPrefix(imp, allowed) {
			return ""
		}
	}
	return "biblioteka spoza listy dozwolonych dla rdzenia i etapów (typy Kubernetesa, nazwy obrazów, YAML)"
}

func reason(from, to Layer) string {
	switch {
	case from == Stages && to == Adapters:
		return "etapy nie importują adapterów: potrzebę wyraża port w etapie, adapter podstawia cmd"
	case from == Adapters && to == Adapters:
		return "adapter to jeden system zewnętrzny; adaptery składa cmd"
	case to == Cmd:
		return "cmd składa program i nie jest biblioteką"
	default:
		return fmt.Sprintf("warstwa %s importuje tylko %v", from, may[from])
	}
}
