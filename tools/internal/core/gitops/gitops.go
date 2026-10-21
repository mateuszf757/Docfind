// Package gitops to rdzeń dostawy przez Argo CD (decyzja 46): którą wersję
// charta Argo musi wdrożyć z zakresu i kiedy uznać, że ją wdrożył.
//
// Zakres rozwiązuje ta sama biblioteka w tej samej wersji i ta sama reguła
// co w Argo CD 3.5.4 (util/versions/tags.go, MaxVersion). dft liczy z listy
// tagów w rejestrze wersję, którą Argo powinien wybrać, i porównuje ją ze
// stanem Application — zamiast przyjąć, że to, co Argo wdrożył, jest tym,
// co miało stanąć.
package gitops

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	semver "github.com/Masterminds/semver/v3"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

// ArgoManager — menedżer pól, pod którym Argo CD stosuje zasoby, przez
// Server-Side Apply i bez niego (gitops-engine, sync_context;
// common.ArgoCDSSAManager w Argo CD 3.5).
const ArgoManager = "argocd-controller"

// ValidateRange odrzuca zakres, którego Argo nie rozwiąże: ani wersja, ani
// ograniczenie SemVer.
func ValidateRange(constraint string) error {
	if _, err := semver.NewVersion(constraint); err == nil {
		return nil
	}
	if _, err := semver.NewConstraint(constraint); err != nil {
		return fmt.Errorf("zakres wersji %q: %w", constraint, err)
	}
	return nil
}

// Latest zwraca wersję, którą Argo CD wybierze z tagów repozytorium charta
// dla zakresu constraint — jak MaxVersion bez prefiksu tagów:
//   - wersja zamiast zakresu to przypięcie, zwracane bez zaglądania w tagi;
//   - tag spoza SemVer jest pomijany (podpisy i atestacje w tym samym
//     repozytorium, sha256-…);
//   - tag, który parsuje się jako SemVer z błędem (np. zero wiodące
//     w części przedwydaniowej), przerywa rozwiązywanie — w Argo też, więc
//     taki tag w rejestrze zatrzymuje dostawę na każdym środowisku;
//   - „_" w tagu to „+" (Helm zapisuje tak metadane budowania, a Argo
//     odwraca to w GetTags);
//   - przedwydaniowe wersje tylko wtedy, gdy zakres sam ma część
//     przedwydaniową (>=0.0.0-0 — każdy commit z main; >=0.0.0 — wydania).
func Latest(constraint string, tags []string) (string, error) {
	if v, err := semver.NewVersion(constraint); err == nil {
		return v.Original(), nil
	}
	c, err := semver.NewConstraint(constraint)
	if err != nil {
		return "", fmt.Errorf("zakres wersji %q: %w", constraint, err)
	}
	var latest *semver.Version
	for _, tag := range tags {
		v, err := semver.NewVersion(strings.ReplaceAll(tag, "_", "+"))
		if errors.Is(err, semver.ErrInvalidSemVer) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("tag %q w repozytorium charta: %w — Argo CD też nie rozwiąże zakresu", tag, err)
		}
		if c.Check(v) && (latest == nil || v.GreaterThan(latest)) {
			latest = v
		}
	}
	if latest == nil {
		return "", fmt.Errorf("żadna z %d wersji w repozytorium charta nie spełnia zakresu %q", len(tags), constraint)
	}
	return latest.Original(), nil
}

// Descending — tagi wersji SemVer od najwyższej, w zapisie z repozytorium
// („_” zamiast „+”); tagi spoza SemVer (np. podpisy sha256-…) pominięte.
func Descending(tags []string) []string {
	type tagged struct {
		tag string
		v   *semver.Version
	}
	var list []tagged
	for _, tag := range tags {
		if v, err := semver.NewVersion(strings.ReplaceAll(tag, "_", "+")); err == nil {
			list = append(list, tagged{tag, v})
		}
	}
	slices.SortStableFunc(list, func(a, b tagged) int { return b.v.Compare(a.v) })
	out := make([]string, len(list))
	for i, t := range list {
		out[i] = t.tag
	}
	return out
}

// Newer mówi, czy wersja a jest wyższa od b w porządku SemVer. Napis spoza
// SemVer (np. pusta wersja Application przed pierwszym rozwiązaniem) nie
// jest wyższy od niczego.
func Newer(a, b string) bool {
	va, err := semver.NewVersion(a)
	if err != nil {
		return false
	}
	vb, err := semver.NewVersion(b)
	return err == nil && va.GreaterThan(vb)
}

// Deployed mówi, czy Application wdrożyła wersję want: zsynchronizowana z tą
// wersją, zdrowa, a ostatnia operacja synchronizacji — jeśli była — zakończona
// sukcesem na tej wersji. Inaczej zwraca, na co jeszcze czeka.
//
// Zdrowie bez synchronizacji nie wystarcza: Application zdrowa na starszej
// wersji to dostawa, która nie doszła (np. Argo nie widzi jeszcze nowego
// tagu). Synchronizacja bez zdrowia też nie: Synced znaczy tylko, że
// manifesty na klastrze zgadzają się z chartem, a nie że pody wstały.
func Deployed(app cluster.Application, want string) (bool, string) {
	var waiting []string
	if app.Revision != want {
		waiting = append(waiting, fmt.Sprintf("wersja %q zamiast %s", app.Revision, want))
	}
	if app.Sync != "Synced" {
		waiting = append(waiting, "synchronizacja "+orUnknown(app.Sync))
	}
	if app.Health != "Healthy" {
		health := "zdrowie " + orUnknown(app.Health)
		if app.HealthMessage != "" {
			health += " (" + app.HealthMessage + ")"
		}
		waiting = append(waiting, health)
	}
	if op := app.Operation; op.Phase != "" && (op.Phase != "Succeeded" || op.Revision != want) {
		operation := fmt.Sprintf("operacja %s na wersji %q", op.Phase, op.Revision)
		if op.Message != "" {
			operation += ": " + op.Message
		}
		waiting = append(waiting, operation)
	}
	// Błędy (ComparisonError, SyncError, …), nie ostrzeżenia: ostrzeżenie
	// nie zatrzymuje synchronizacji.
	for _, c := range app.Conditions {
		if strings.HasSuffix(c.Type, "Error") {
			waiting = append(waiting, c.Type+": "+c.Message)
		}
	}
	return len(waiting) == 0, strings.Join(waiting, "; ")
}

// Stalled mówi, czy Application stoi na wersji want z nieudaną ostatnią
// operacją, której Argo CD sam nie ponowi (decyzja 82): manifesty zgadzają
// się z wersją (Synced), więc synchronizacja automatyczna nie rusza,
// a operacja jest zakończona — Error albo Failed, bez zleconej następnej.
// Tak kończy się panika kontrolera po udanej synchronizacji, gdy zegar
// węzła cofnie się w jej trakcie: czas trwania wychodzi ujemny, a licznik
// Prometheusa odmawia ujemnego przyrostu (IncAppSyncDuration
// w controller/metrics/metrics.go, Argo CD 3.5.4); ścieżka paniki omija
// politykę ponowień. Deployed takiej Application nie przepuści, więc bez
// ponownej synchronizacji dostawa czekałaby do limitu.
func Stalled(app cluster.Application, want string) bool {
	op := app.Operation
	return app.Revision == want && app.Sync == "Synced" && !op.Requested &&
		(op.Phase == "Error" || op.Phase == "Failed") && (op.Revision == want || op.Revision == "")
}

func orUnknown(s string) string {
	if s == "" {
		return "nieznana"
	}
	return s
}

// Uncovered zwraca zasoby z renderu charta aplikacji, których projekt Argo CD
// nie pozwala wdrożyć: rodzaj spoza namespaceResourceWhitelist albo zasób
// z jawną przestrzenią nazw inną niż docelowa. Argo odmówiłby synchronizacji
// dopiero na klastrze („resource … is not permitted in project") — tu
// rozjazd między chartem a projektem wychodzi przy `dft test`.
func Uncovered(project map[string]any, manifests []map[string]any, namespace string) []string {
	allowed := map[string]bool{}
	spec, _ := project["spec"].(map[string]any)
	list, _ := spec["namespaceResourceWhitelist"].([]any)
	for _, raw := range list {
		if gk, ok := raw.(map[string]any); ok {
			group, _ := gk["group"].(string)
			kind, _ := gk["kind"].(string)
			allowed[group+"/"+kind] = true
		}
	}
	var found []string
	for _, m := range manifests {
		apiVersion, _ := m["apiVersion"].(string)
		kind, _ := m["kind"].(string)
		group := ""
		if g, _, ok := strings.Cut(apiVersion, "/"); ok {
			group = g
		}
		meta, _ := m["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if !allowed[group+"/"+kind] {
			found = append(found, fmt.Sprintf("%s %s (grupa %q) spoza namespaceResourceWhitelist projektu", kind, name, group))
		}
		if ns, _ := meta["namespace"].(string); ns != "" && ns != namespace {
			found = append(found, fmt.Sprintf("%s %s w przestrzeni %s — projekt dopuszcza tylko %s", kind, name, ns, namespace))
		}
	}
	return found
}

// builtinCluster — wbudowane rodzaje Kubernetesa na poziomie klastra, które
// mogą przyjść z charta. Rodzaje z CRD rozpoznaje się po polu scope ich CRD.
var builtinCluster = map[string]bool{
	"/Namespace": true, "/PersistentVolume": true,
	"rbac.authorization.k8s.io/ClusterRole": true, "rbac.authorization.k8s.io/ClusterRoleBinding": true,
	"apiextensions.k8s.io/CustomResourceDefinition": true, "apiregistration.k8s.io/APIService": true,
	"admissionregistration.k8s.io/ValidatingWebhookConfiguration":   true,
	"admissionregistration.k8s.io/MutatingWebhookConfiguration":     true,
	"admissionregistration.k8s.io/ValidatingAdmissionPolicy":        true,
	"admissionregistration.k8s.io/ValidatingAdmissionPolicyBinding": true,
	"scheduling.k8s.io/PriorityClass":                               true, "storage.k8s.io/StorageClass": true, "storage.k8s.io/CSIDriver": true,
	"networking.k8s.io/IngressClass": true, "node.k8s.io/RuntimeClass": true,
}

// groupKind — grupa i rodzaj z apiVersion i kind manifestu.
func groupKind(m map[string]any) (string, string) {
	apiVersion, _ := m["apiVersion"].(string)
	kind, _ := m["kind"].(string)
	group, _, ok := strings.Cut(apiVersion, "/")
	if !ok {
		group = ""
	}
	return group, kind
}

// UncoveredPlatform zwraca zasoby z renderów chartów platformy, których
// projekt platformy nie pozwala wdrożyć: rodzaj na poziomie klastra spoza
// clusterResourceWhitelist albo zasób z jawną przestrzenią nazw spoza
// destinations. crds — CRD z renderów: z ich scope wiadomo, które rodzaje
// niestandardowe są na poziomie klastra.
func UncoveredPlatform(project map[string]any, manifests, crds []map[string]any) []string {
	clusterScoped := maps.Clone(builtinCluster)
	for _, crd := range crds {
		if k, _ := crd["kind"].(string); k != "CustomResourceDefinition" {
			continue
		}
		spec, _ := crd["spec"].(map[string]any)
		names, _ := spec["names"].(map[string]any)
		group, _ := spec["group"].(string)
		kind, _ := names["kind"].(string)
		if scope, _ := spec["scope"].(string); scope == "Cluster" {
			clusterScoped[group+"/"+kind] = true
		}
	}
	spec, _ := project["spec"].(map[string]any)
	allowed := map[string]bool{}
	list, _ := spec["clusterResourceWhitelist"].([]any)
	for _, raw := range list {
		if gk, ok := raw.(map[string]any); ok {
			group, _ := gk["group"].(string)
			kind, _ := gk["kind"].(string)
			allowed[group+"/"+kind] = true
		}
	}
	destinations := map[string]bool{}
	dests, _ := spec["destinations"].([]any)
	for _, raw := range dests {
		if d, ok := raw.(map[string]any); ok {
			ns, _ := d["namespace"].(string)
			destinations[ns] = true
		}
	}
	var found []string
	for _, m := range manifests {
		group, kind := groupKind(m)
		meta, _ := m["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if clusterScoped[group+"/"+kind] {
			if !allowed[group+"/"+kind] {
				found = append(found, fmt.Sprintf("%s %s (grupa %q, poziom klastra) spoza clusterResourceWhitelist projektu", kind, name, group))
			}
			continue
		}
		if ns, _ := meta["namespace"].(string); ns != "" && !destinations[ns] {
			found = append(found, fmt.Sprintf("%s %s w przestrzeni %s spoza destinations projektu", kind, name, ns))
		}
	}
	slices.Sort(found)
	return slices.Compact(found)
}

// SyncWave — adnotacja fali synchronizacji Argo CD.
const SyncWave = "argocd.argoproj.io/sync-wave"

// LateProjects zwraca Applications z renderu pakietu, których projekt nie
// stoi w fali wcześniejszej niż one (decyzja 81). Application z projektem
// z tej samej fali kontroler ocenia, zanim zobaczy projekt (InvalidSpecError
// do następnego uzgodnienia), a projekt zmieniony wcześniej niż jego
// Applications odrzuca ich stare źródła na czas fal pomiędzy. Brak adnotacji
// to fala 0, jak w Argo CD.
func LateProjects(manifests []map[string]any) []string {
	wave := func(m map[string]any) (int, error) {
		meta, _ := m["metadata"].(map[string]any)
		annotations, _ := meta["annotations"].(map[string]any)
		raw, _ := annotations[SyncWave].(string)
		if raw == "" {
			return 0, nil
		}
		var w int
		_, err := fmt.Sscan(raw, &w)
		return w, err
	}
	name := func(m map[string]any) string {
		meta, _ := m["metadata"].(map[string]any)
		n, _ := meta["name"].(string)
		return n
	}
	projects := map[string]int{}
	var found []string
	for _, m := range manifests {
		if _, kind := groupKind(m); kind == "AppProject" {
			w, err := wave(m)
			if err != nil {
				found = append(found, fmt.Sprintf("AppProject %s: fala %v nie jest liczbą", name(m), err))
				continue
			}
			projects[name(m)] = w
		}
	}
	for _, m := range manifests {
		if _, kind := groupKind(m); kind != "Application" {
			continue
		}
		spec, _ := m["spec"].(map[string]any)
		project, _ := spec["project"].(string)
		w, err := wave(m)
		if err != nil {
			found = append(found, fmt.Sprintf("Application %s: fala %v nie jest liczbą", name(m), err))
			continue
		}
		pw, ok := projects[project]
		switch {
		case !ok:
			found = append(found, fmt.Sprintf("Application %s: projekt %s spoza pakietu", name(m), project))
		case pw >= w:
			found = append(found, fmt.Sprintf("Application %s w fali %d, a jej projekt %s w fali %d", name(m), w, project, pw))
		}
	}
	return found
}
