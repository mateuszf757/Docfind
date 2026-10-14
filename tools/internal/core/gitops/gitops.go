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
	"strings"

	semver "github.com/Masterminds/semver/v3"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

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
