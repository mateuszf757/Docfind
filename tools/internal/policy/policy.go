// Package policy sprawdza manifesty Kubernetesa względem decyzji projektu
// (dawniej ci/check_policy.py).
//
// kubeconform sprawdza, czy manifest jest poprawny względem schematu. Tu
// sprawdzamy, czy jest zgodny z naszymi decyzjami — pole może być poprawne
// i jednocześnie niechciane. Przykład, który dał początek tym regułom: chart
// CoreDNS dokładał domyślny limit CPU, bo Helm scala wartości z domyślnymi,
// a kubeconform nie miał czego zgłosić.
package policy

import (
	"fmt"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/manifest"
)

// Options — reguły zależne od źródła manifestów.
type Options struct {
	// RequireDigest — obrazy przypięte digestem (decyzja 20). Wymagane od
	// komponentów z zewnątrz; nasz obraz w renderze testowym ma tag „lint".
	RequireDigest bool
}

var workloadKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true, "Job": true, "CronJob": true}

// podSpec zwraca spec poda workloadu albo nil dla innych rodzajów.
func podSpec(m map[string]any) map[string]any {
	kind := manifest.String(m, "kind")
	if !workloadKinds[kind] {
		return nil
	}
	spec := manifest.Map(m, "spec")
	if kind == "CronJob" {
		spec = manifest.Map(manifest.Map(spec, "jobTemplate"), "spec")
	}
	// Manifest pochodzi z YAML-a, więc typ trzeba sprawdzić, a nie założyć.
	return manifest.Map(manifest.Map(spec, "template"), "spec")
}

// Check zwraca naruszenia reguł we wszystkich manifestach.
func Check(manifests []map[string]any, opts Options) []string {
	var found []string
	for _, m := range manifests {
		found = append(found, violations(m, opts)...)
	}
	return found
}

func violations(m map[string]any, opts Options) []string {
	spec := podSpec(m)
	if spec == nil {
		return nil
	}
	name := manifest.String(m, "kind") + "/" + manifest.String(manifest.Map(m, "metadata"), "name")
	var found []string
	containers := append(manifest.List(spec, "containers"), manifest.List(spec, "initContainers")...)
	for _, raw := range containers {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		where := fmt.Sprintf("%s kontener %s", name, manifest.String(c, "name"))
		resources := manifest.Map(c, "resources")
		requests := manifest.Map(resources, "requests")
		limits := manifest.Map(resources, "limits")

		// Decyzja 16: request CPU i pamięci zawsze — na nich opiera się
		// scheduler; limit pamięci zawsze — chroni węzeł przed wyciekiem.
		for _, r := range []string{"cpu", "memory"} {
			if _, ok := requests[r]; !ok {
				found = append(found, fmt.Sprintf("%s: brak requests.%s", where, r))
			}
		}
		if _, ok := limits["memory"]; !ok {
			found = append(found, fmt.Sprintf("%s: brak limits.memory", where))
		}
		// Decyzja 16: bez limitu CPU — dławienie przez CFS daje opóźnienia
		// przy bezczynnym węźle.
		if cpu, ok := limits["cpu"]; ok && cpu != nil {
			found = append(found, fmt.Sprintf("%s: limits.cpu=%v — decyzja 16 wyklucza limity CPU", where, cpu))
		}
		// Decyzja 20: obrazy z zewnątrz przypięte digestem. Tag jest
		// przesuwalny, więc sam tag oznacza zgodę na zawartość, której nikt
		// nie sprawdził.
		if image := manifest.String(c, "image"); opts.RequireDigest && !strings.Contains(image, "@sha256:") {
			found = append(found, fmt.Sprintf("%s: obraz %s bez digestu — decyzja 20", where, image))
		}
	}
	return found
}
