package gitops

import (
	"slices"
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

func TestDescending(t *testing.T) {
	got := Descending([]string{"0.3.2-dev.9", "sha256-abc", "0.3.2-dev.44", "0.3.1", "0.3.2-dev.10", "0.4.0-dev.1_abc"})
	want := []string{"0.4.0-dev.1_abc", "0.3.2-dev.44", "0.3.2-dev.10", "0.3.2-dev.9", "0.3.1"}
	if !slices.Equal(got, want) {
		t.Errorf("Descending = %q, oczekiwano %q", got, want)
	}
}

func TestUncoveredPlatform(t *testing.T) {
	project := map[string]any{"spec": map[string]any{
		"clusterResourceWhitelist": []any{map[string]any{"group": "rbac.authorization.k8s.io", "kind": "ClusterRole"}},
		"destinations":             []any{map[string]any{"namespace": "cert-manager"}, map[string]any{"namespace": "kube-system"}},
	}}
	crds := []map[string]any{
		{"kind": "CustomResourceDefinition", "spec": map[string]any{"group": "cert-manager.io", "scope": "Cluster", "names": map[string]any{"kind": "ClusterIssuer"}}},
		{"kind": "CustomResourceDefinition", "spec": map[string]any{"group": "cert-manager.io", "scope": "Namespaced", "names": map[string]any{"kind": "Certificate"}}},
	}
	ok := []map[string]any{
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": map[string]any{"name": "a"}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": map[string]any{"name": "leader", "namespace": "kube-system"}},
		// Bez jawnej przestrzeni — trafia do przestrzeni docelowej Application.
		{"apiVersion": "cert-manager.io/v1", "kind": "Certificate", "metadata": map[string]any{"name": "c"}},
	}
	if got := UncoveredPlatform(project, ok, crds); len(got) != 0 {
		t.Fatalf("render w granicach projektu zgłoszony: %v", got)
	}
	bad := append(ok,
		map[string]any{"apiVersion": "cert-manager.io/v1", "kind": "ClusterIssuer", "metadata": map[string]any{"name": "ca"}},
		map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": "x"}},
		map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s", "namespace": "default"}},
	)
	got := strings.Join(UncoveredPlatform(project, bad, crds), "\n")
	for _, want := range []string{"ClusterIssuer ca", "CustomResourceDefinition x", "Secret s w przestrzeni default"} {
		if !strings.Contains(got, want) {
			t.Errorf("brak %q w:\n%s", want, got)
		}
	}
}

func TestLateProjects(t *testing.T) {
	obj := func(kind, name, wave, project string) map[string]any {
		m := map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": kind, "metadata": map[string]any{"name": name}}
		if wave != "" {
			m["metadata"].(map[string]any)["annotations"] = map[string]any{SyncWave: wave}
		}
		if project != "" {
			m["spec"] = map[string]any{"project": project}
		}
		return m
	}
	ok := []map[string]any{
		obj("AppProject", "platform", "-1", ""),
		obj("AppProject", "docfind", "5", ""),
		obj("Application", "envoy-gateway", "", "platform"),
		obj("Application", "platform", "5", "platform"),
		obj("Application", "docfind", "6", "docfind"),
	}
	if got := LateProjects(ok); len(got) != 0 {
		t.Fatalf("projekty przed Applications zgłoszone: %v", got)
	}
	bad := []map[string]any{
		// Bez adnotacji — fala 0, jak Application w fali 0.
		obj("AppProject", "platform", "", ""),
		obj("AppProject", "docfind", "6", ""),
		obj("Application", "envoy-gateway", "0", "platform"),
		obj("Application", "docfind", "6", "docfind"),
		obj("Application", "obca", "7", "default"),
		obj("Application", "zła", "x", "platform"),
	}
	got := strings.Join(LateProjects(bad), "\n")
	for _, want := range []string{"envoy-gateway w fali 0, a jej projekt platform w fali 0", "docfind w fali 6, a jej projekt docfind w fali 6", "obca: projekt default spoza pakietu", "zła: fala"} {
		if !strings.Contains(got, want) {
			t.Errorf("brak %q w:\n%s", want, got)
		}
	}
}

func TestStalled(t *testing.T) {
	want := "sha256:" + strings.Repeat("a", 64)
	stalled := cluster.Application{Sync: "Synced", Revision: want, Health: "Healthy",
		Operation: cluster.AppOperation{Phase: "Error", Revision: want, Message: "counter cannot decrease in value"}}
	if !Stalled(stalled, want) {
		t.Error("zsynchronizowana Application z operacją Error na tej wersji nie rozpoznana")
	}
	failed := stalled
	failed.Operation.Phase, failed.Operation.Revision = "Failed", ""
	if !Stalled(failed, want) {
		t.Error("operacja Failed bez wyniku synchronizacji nie rozpoznana")
	}
	for name, app := range map[string]cluster.Application{
		"operacja zlecona":     {Sync: "Synced", Revision: want, Operation: cluster.AppOperation{Phase: "Error", Revision: want, Requested: true}},
		"operacja w toku":      {Sync: "Synced", Revision: want, Operation: cluster.AppOperation{Phase: "Running", Revision: want}},
		"sukces":               {Sync: "Synced", Revision: want, Operation: cluster.AppOperation{Phase: "Succeeded", Revision: want}},
		"inna wersja":          {Sync: "Synced", Revision: "sha256:" + strings.Repeat("b", 64), Operation: cluster.AppOperation{Phase: "Error"}},
		"niezsynchronizowana":  {Sync: "OutOfSync", Revision: want, Operation: cluster.AppOperation{Phase: "Error", Revision: want}},
		"błąd na innej wersji": {Sync: "Synced", Revision: want, Operation: cluster.AppOperation{Phase: "Error", Revision: "sha256:" + strings.Repeat("b", 64)}},
	} {
		if Stalled(app, want) {
			t.Errorf("%s: uznana za porzuconą przez Argo", name)
		}
	}
}
