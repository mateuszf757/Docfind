package gitops

import (
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/core/cluster"
)

func TestLatest(t *testing.T) {
	// Tagi jak w repozytorium charta: buildy z main, wydanie, atestacja
	// i podpis obok (tagi sha256-…).
	tags := []string{
		"0.4.0", "0.4.1-dev.1", "0.4.1-dev.9", "0.4.1-dev.10", "0.3.1-dev.40",
		"sha256-" + strings.Repeat("a", 64), "sha256-" + strings.Repeat("b", 64) + ".sig",
	}
	cases := []struct {
		name, constraint string
		tags             []string
		want             string
	}{
		// dev.10 po dev.9 — identyfikatory liczbowe porównuje się liczbowo,
		// nie jak tekst.
		{"każdy commit z main", ">=0.0.0-0", tags, "0.4.1-dev.10"},
		// Zakres bez części przedwydaniowej nie dopuszcza buildów z main.
		{"tylko wydania", ">=0.0.0", tags, "0.4.0"},
		{"przypięcie bez zaglądania w tagi", "0.4.1-dev.1", nil, "0.4.1-dev.1"},
		// Helm zapisuje „+" w tagu OCI jako „_", Argo odwraca to w GetTags.
		{"metadane budowania", ">=0.0.0-0", []string{"0.4.0", "0.4.1-dev.2_abc1234"}, "0.4.1-dev.2+abc1234"},
		{"wydanie powyżej buildów sprzed niego", ">=0.0.0-0", []string{"0.4.0-dev.7", "0.4.0"}, "0.4.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Latest(tc.constraint, tc.tags)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("Latest(%q) = %s, chcę %s", tc.constraint, got, tc.want)
			}
		})
	}
}

func TestLatestRefuses(t *testing.T) {
	cases := []struct {
		name, constraint string
		tags             []string
		want             string
	}{
		{"brak wersji w zakresie", ">=1.0.0", []string{"0.4.0", "0.4.1-dev.1"}, "nie spełnia zakresu"},
		// Argo przerywa rozwiązywanie na takim tagu — dostawa stoi, więc dft
		// też nie może po cichu go pominąć.
		{"zero wiodące w części przedwydaniowej", ">=0.0.0-0", []string{"0.4.1-dev.01"}, "Argo CD też nie rozwiąże"},
		{"zakres z literówką", ">==0.0.0", nil, "zakres wersji"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Latest(tc.constraint, tc.tags)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("błąd %v, chcę zawierający %q", err, tc.want)
			}
		})
	}
}

func TestValidateRange(t *testing.T) {
	for _, ok := range []string{">=0.0.0-0", ">=0.0.0", "0.4.1", "~0.4"} {
		if err := ValidateRange(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "latest", ">==1"} {
		if err := ValidateRange(bad); err == nil {
			t.Errorf("%q przeszedł", bad)
		}
	}
}

func TestDeployed(t *testing.T) {
	done := cluster.Application{
		Sync: "Synced", Revision: "0.4.1-dev.10", Health: "Healthy",
		Operation: cluster.AppOperation{Phase: "Succeeded", Revision: "0.4.1-dev.10"},
		// Ostrzeżenie nie zatrzymuje dostawy.
		Conditions: []cluster.AppCondition{{Type: "OrphanedResourceWarning", Message: "1 zasób poza aplikacją"}},
	}
	if ok, why := Deployed(done, "0.4.1-dev.10"); !ok {
		t.Fatalf("wdrożona aplikacja niegotowa: %s", why)
	}
	// Przed pierwszą operacją (Argo jeszcze nie synchronizował) faza jest
	// pusta — sama synchronizacja i zdrowie wystarczą.
	noOp := done
	noOp.Operation = cluster.AppOperation{}
	if ok, why := Deployed(noOp, "0.4.1-dev.10"); !ok {
		t.Fatalf("aplikacja bez operacji niegotowa: %s", why)
	}

	cases := []struct {
		name   string
		change func(*cluster.Application)
		want   string
	}{
		// Zdrowa i zsynchronizowana na starszej wersji: Argo nie widzi
		// jeszcze nowego tagu.
		{"starsza wersja", func(a *cluster.Application) { a.Revision = "0.4.1-dev.9" }, `wersja "0.4.1-dev.9" zamiast 0.4.1-dev.10`},
		{"pody nie wstały", func(a *cluster.Application) { a.Health, a.HealthMessage = "Progressing", "Waiting for rollout" }, "zdrowie Progressing (Waiting for rollout)"},
		{"poza synchronizacją", func(a *cluster.Application) { a.Sync = "OutOfSync" }, "synchronizacja OutOfSync"},
		{"operacja nieudana", func(a *cluster.Application) {
			a.Operation = cluster.AppOperation{Phase: "Failed", Revision: "0.4.1-dev.10", Message: "one or more objects failed to apply"}
		}, "operacja Failed"},
		{"operacja na starszej wersji", func(a *cluster.Application) { a.Operation.Revision = "0.4.1-dev.9" }, `operacja Succeeded na wersji "0.4.1-dev.9"`},
		{"błąd porównania", func(a *cluster.Application) {
			a.Conditions = []cluster.AppCondition{{Type: "ComparisonError", Message: "unable to resolve '>=0.0.0-0'"}}
		}, "ComparisonError: unable to resolve"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := done
			app.Conditions = nil
			tc.change(&app)
			ok, why := Deployed(app, "0.4.1-dev.10")
			if ok || !strings.Contains(why, tc.want) {
				t.Fatalf("Deployed = %v, %q; chcę false z %q", ok, why, tc.want)
			}
		})
	}
}

func TestUncovered(t *testing.T) {
	project := map[string]any{"spec": map[string]any{"namespaceResourceWhitelist": []any{
		map[string]any{"group": "", "kind": "ConfigMap"},
		map[string]any{"group": "apps", "kind": "Deployment"},
	}}}
	manifests := []map[string]any{
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "api"}},
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "api", "namespace": "docfind"}},
	}
	if got := Uncovered(project, manifests, "docfind"); len(got) != 0 {
		t.Fatalf("render w granicach projektu zgłoszony: %v", got)
	}
	manifests = append(manifests,
		// Rola w charcie aplikacji: projekt nie pozwala nadać sobie uprawnień.
		map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": map[string]any{"name": "api"}},
		// Ta sama nazwa rodzaju z innej grupy to inny zasób.
		map[string]any{"apiVersion": "example.com/v1", "kind": "Deployment", "metadata": map[string]any{"name": "x"}},
		map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "obcy", "namespace": "kube-system"}},
	)
	got := strings.Join(Uncovered(project, manifests, "docfind"), "\n")
	for _, want := range []string{`Role api (grupa "rbac.authorization.k8s.io")`, `Deployment x (grupa "example.com")`, "ConfigMap obcy w przestrzeni kube-system"} {
		if !strings.Contains(got, want) {
			t.Errorf("brak %q w:\n%s", want, got)
		}
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.4.1-dev.10", "0.4.1-dev.9", true},
		{"0.4.1", "0.4.1-dev.10", true},
		{"0.4.1-dev.9", "0.4.1-dev.9", false},
		{"0.4.1-dev.9", "0.4.1-dev.10", false},
		{"", "0.4.1-dev.9", false},
	} {
		if got := Newer(tc.a, tc.b); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}
