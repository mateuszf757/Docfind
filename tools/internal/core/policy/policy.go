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
	"slices"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/core/container"
	"github.com/mateuszf757/Docfind/tools/internal/kit/manifest"
)

// Options — reguły zależne od źródła manifestów.
type Options struct {
	// RequireDigest — obrazy przypięte digestem (decyzja 20). Wymagane od
	// komponentów z zewnątrz; nasz obraz w renderze testowym ma tag „lint".
	RequireDigest bool
	// RotatableSecrets — sekrety tylko jako wolumen bez subPath (decyzja 44).
	// subPath nie aktualizuje się nigdy, a zmienna środowiskowa z Secreta to
	// migawka ze startu kontenera: rotacja nie dotarłaby do poda. Wymagane
	// od naszego charta; charty z zewnątrz robią po swojemu.
	RotatableSecrets bool
	// Hardened — pody utwardzone ponad PSA restricted (decyzja 55):
	// readOnlyRootFilesystem, allowPrivilegeEscalation: false, drop ALL,
	// runAsNonRoot, seccomp RuntimeDefault i pod bez tokenu konta usługi.
	// Wymagane od naszego charta aplikacji; PSA w API serverze sprawdza część
	// tego dopiero na klastrze, a RO rootfs i token — wcale.
	Hardened bool
	// ImmutableConfig — ConfigMapy niezmienne (immutable: true), z sumą
	// treści w nazwie (decyzja 63): nowa konfiguracja to nowa ConfigMapa.
	ImmutableConfig bool
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
	if opts.ImmutableConfig && manifest.String(m, "kind") == "ConfigMap" {
		if v, ok := m["immutable"].(bool); !ok || !v {
			return []string{fmt.Sprintf("ConfigMap/%s: brak immutable: true — zmiana treści pod tą samą nazwą dociera do starych podów w trakcie rolloutu (decyzja 63)", manifest.String(manifest.Map(m, "metadata"), "name"))}
		}
		return nil
	}
	spec := podSpec(m)
	if spec == nil {
		return nil
	}
	name := manifest.String(m, "kind") + "/" + manifest.String(manifest.Map(m, "metadata"), "name")
	var found []string
	if opts.Hardened {
		found = append(found, podHardening(name, spec)...)
	}
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
		if opts.RotatableSecrets {
			found = append(found, unrotatable(where, c, secretVolumes(spec))...)
		}
		if opts.Hardened {
			found = append(found, containerHardening(where, c, spec)...)
		}
	}
	return found
}

// secretVolumes — nazwy wolumenów poda z Secreta.
func secretVolumes(spec map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, raw := range manifest.List(spec, "volumes") {
		if v, ok := raw.(map[string]any); ok && manifest.Map(v, "secret") != nil {
			names[manifest.String(v, "name")] = true
		}
	}
	return names
}

// unrotatable — sposoby podania sekretu, do których rotacja nie dociera.
func unrotatable(where string, c map[string]any, secrets map[string]bool) []string {
	var found []string
	for _, raw := range manifest.List(c, "volumeMounts") {
		m, ok := raw.(map[string]any)
		if ok && secrets[manifest.String(m, "name")] && manifest.String(m, "subPath") != "" {
			found = append(found, fmt.Sprintf("%s: Secret %s montowany przez subPath — nie aktualizuje się nigdy (decyzja 44)", where, manifest.String(m, "name")))
		}
	}
	for _, raw := range manifest.List(c, "env") {
		e, ok := raw.(map[string]any)
		if ok && manifest.Map(manifest.Map(e, "valueFrom"), "secretKeyRef") != nil {
			found = append(found, fmt.Sprintf("%s: zmienna %s z Secreta — migawka ze startu kontenera (decyzja 44)", where, manifest.String(e, "name")))
		}
	}
	for _, raw := range manifest.List(c, "envFrom") {
		e, ok := raw.(map[string]any)
		if ok && manifest.Map(e, "secretRef") != nil {
			found = append(found, fmt.Sprintf("%s: envFrom z Secreta — migawka ze startu kontenera (decyzja 44)", where))
		}
	}
	return found
}

// podHardening — pod bez tokenu konta usługi: aplikacja nie rozmawia
// z API serverem, a token w podzie to poświadczenie do kradzieży
// (decyzje 43 i 55).
func podHardening(name string, spec map[string]any) []string {
	if v, ok := spec["automountServiceAccountToken"].(bool); !ok || v {
		return []string{fmt.Sprintf("%s: automountServiceAccountToken musi być false — aplikacja nie rozmawia z API serverem", name)}
	}
	return nil
}

// containerHardening — kontener utwardzony: system plików tylko do odczytu,
// bez eskalacji uprawnień, bez capabilities, nie jako root, z seccomp
// RuntimeDefault. runAsNonRoot i seccomp mogą stać w kontekście poda.
func containerHardening(where string, c, spec map[string]any) []string {
	sc := manifest.Map(c, "securityContext")
	pod := manifest.Map(spec, "securityContext")
	var found []string
	if v, ok := sc["readOnlyRootFilesystem"].(bool); !ok || !v {
		found = append(found, fmt.Sprintf("%s: readOnlyRootFilesystem musi być true — zapis tylko do wolumenów", where))
	}
	if v, ok := sc["allowPrivilegeEscalation"].(bool); !ok || v {
		found = append(found, fmt.Sprintf("%s: allowPrivilegeEscalation musi być false", where))
	}
	if !dropsAll(manifest.Map(sc, "capabilities")) {
		found = append(found, fmt.Sprintf("%s: capabilities.drop musi zawierać ALL", where))
	}
	if !boolIn("runAsNonRoot", sc, pod) {
		found = append(found, fmt.Sprintf("%s: runAsNonRoot musi być true (kontener albo pod)", where))
	}
	for _, ctx := range []map[string]any{sc, pod} {
		if uid, ok := ctx["runAsUser"]; ok && fmt.Sprint(uid) == "0" {
			found = append(found, fmt.Sprintf("%s: runAsUser 0", where))
		}
	}
	if profile(sc) == "" && profile(pod) != "RuntimeDefault" || profile(sc) != "" && profile(sc) != "RuntimeDefault" {
		found = append(found, fmt.Sprintf("%s: seccompProfile.type musi być RuntimeDefault (kontener albo pod)", where))
	}
	return found
}

func dropsAll(caps map[string]any) bool {
	for _, d := range manifest.List(caps, "drop") {
		if fmt.Sprint(d) == "ALL" {
			return true
		}
	}
	return false
}

// boolIn — pole logiczne ustawione na true w kontenerze, a bez niego w podzie.
func boolIn(field string, container, pod map[string]any) bool {
	if v, ok := container[field].(bool); ok {
		return v
	}
	v, ok := pod[field].(bool)
	return ok && v
}

func profile(sc map[string]any) string {
	return manifest.String(manifest.Map(sc, "seccompProfile"), "type")
}

// APIHardening — ustawienia bezpieczeństwa kontenera api z charta
// (deploy/charts/docfind), z którymi dym obrazu w `check:runtime` uruchamia
// kontener (decyzja 72). Build obrazu nie ma Helma, więc ustawienia są
// tutaj, a zgodność z renderem charta sprawdza stages/charts — zmiana
// w charcie bez zmiany tutaj to czerwony `lint`.
var APIHardening = container.Hardening{
	ReadOnlyRoot:    true,
	Tmpfs:           []string{"/tmp"},
	CapDropAll:      true,
	NoNewPrivileges: true,
	User:            "10001:10001",
}

// PodHardening — ustawienia bezpieczeństwa kontenera name z workloadów
// w manifestach: kontekst kontenera i poda (runAsUser, runAsGroup) oraz
// wolumeny emptyDir jako miejsca zapisu. Kontener w kilku workloadach musi
// mieć wszędzie te same.
func PodHardening(manifests []map[string]any, name string) (container.Hardening, error) {
	var found []container.Hardening
	for _, m := range manifests {
		spec := podSpec(m)
		if spec == nil {
			continue
		}
		emptyDirs := map[string]bool{}
		for _, v := range manifest.List(spec, "volumes") {
			if vol, _ := v.(map[string]any); vol != nil && vol["emptyDir"] != nil {
				emptyDirs[manifest.String(vol, "name")] = true
			}
		}
		pod := manifest.Map(spec, "securityContext")
		for _, c := range manifest.List(spec, "containers") {
			ctr, _ := c.(map[string]any)
			if manifest.String(ctr, "name") != name {
				continue
			}
			sc := manifest.Map(ctr, "securityContext")
			h := container.Hardening{CapDropAll: dropsAll(manifest.Map(sc, "capabilities"))}
			if v, ok := sc["readOnlyRootFilesystem"].(bool); ok && v {
				h.ReadOnlyRoot = true
			}
			if v, ok := sc["allowPrivilegeEscalation"].(bool); ok && !v {
				h.NoNewPrivileges = true
			}
			if uid := securityField("runAsUser", sc, pod); uid != "" {
				h.User = uid
				if gid := securityField("runAsGroup", sc, pod); gid != "" {
					h.User += ":" + gid
				}
			}
			for _, vm := range manifest.List(ctr, "volumeMounts") {
				if mount, _ := vm.(map[string]any); emptyDirs[manifest.String(mount, "name")] {
					h.Tmpfs = append(h.Tmpfs, manifest.String(mount, "mountPath"))
				}
			}
			slices.Sort(h.Tmpfs)
			found = append(found, h)
		}
	}
	if len(found) == 0 {
		return container.Hardening{}, fmt.Errorf("brak kontenera %s w workloadach renderu", name)
	}
	for _, h := range found[1:] {
		if !Same(h, found[0]) {
			return container.Hardening{}, fmt.Errorf("kontener %s ma różne ustawienia w workloadach: %s i %s", name, found[0], h)
		}
	}
	return found[0], nil
}

// Same — te same ustawienia poda.
func Same(a, b container.Hardening) bool {
	return a.ReadOnlyRoot == b.ReadOnlyRoot && a.CapDropAll == b.CapDropAll && a.NoNewPrivileges == b.NoNewPrivileges &&
		a.User == b.User && slices.Equal(a.Tmpfs, b.Tmpfs)
}

// securityField — pole kontekstu kontenera, a bez niego poda.
func securityField(field string, container, pod map[string]any) string {
	if v, ok := container[field]; ok {
		return fmt.Sprint(v)
	}
	if v, ok := pod[field]; ok {
		return fmt.Sprint(v)
	}
	return ""
}
