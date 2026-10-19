package policy

import (
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/kit/manifest"
)

const deployment = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  template:
    spec:
      containers:
        - name: api
          image: %s
          resources: %s
`

func render(image, resources string) []map[string]any {
	src := strings.Replace(strings.Replace(deployment, "%s", image, 1), "%s", resources, 1)
	docs, err := manifest.Parse(strings.NewReader(src + "\n---\napiVersion: v1\nkind: Service\nmetadata: {name: s}\n"))
	if err != nil {
		panic(err)
	}
	return docs
}

func TestCheck(t *testing.T) {
	good := "{requests: {cpu: 10m, memory: 16Mi}, limits: {memory: 64Mi}}"
	tests := []struct {
		name, image, resources string
		requireDigest          bool
		want                   []string
	}{
		{"zgodny", "img@sha256:abc", good, true, nil},
		{"limit CPU", "img@sha256:abc", "{requests: {cpu: 10m, memory: 16Mi}, limits: {memory: 64Mi, cpu: 100m}}", true, []string{"limits.cpu=100m"}},
		{"brak requestów i limitu pamięci", "img@sha256:abc", "{}", true, []string{"brak requests.cpu", "brak requests.memory", "brak limits.memory"}},
		{"obraz bez digestu", "img:1.0", good, true, []string{"bez digestu"}},
		{"obraz bez digestu, gdy nie wymagany", "img:1.0", good, false, nil},
	}
	for _, tt := range tests {
		got := Check(render(tt.image, tt.resources), Options{RequireDigest: tt.requireDigest})
		if len(got) != len(tt.want) {
			t.Errorf("%s: %v, oczekiwano %d naruszeń", tt.name, got, len(tt.want))
			continue
		}
		for i, w := range tt.want {
			if !strings.Contains(got[i], w) {
				t.Errorf("%s: %q nie zawiera %q", tt.name, got[i], w)
			}
		}
	}
}

// TestRotatableSecrets: Secret przez subPath, zmienna z secretKeyRef
// i envFrom z Secreta to sekrety, do których rotacja nie dociera; wolumen
// bez subPath przechodzi.
func TestRotatableSecrets(t *testing.T) {
	deployment := func(mount map[string]any, env []any) map[string]any {
		c := map[string]any{
			"name": "api", "image": "x@sha256:abc",
			"resources":    map[string]any{"requests": map[string]any{"cpu": "10m", "memory": "64Mi"}, "limits": map[string]any{"memory": "128Mi"}},
			"volumeMounts": []any{mount}, "env": env,
		}
		return map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "api"}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{c},
			"volumes":    []any{map[string]any{"name": "secrets", "secret": map[string]any{"secretName": "s"}}, map[string]any{"name": "config", "configMap": map[string]any{"name": "c"}}},
		}}}}
	}
	tests := []struct {
		name  string
		mount map[string]any
		env   []any
		want  string
	}{
		{"katalog bez subPath", map[string]any{"name": "secrets", "mountPath": "/run/secrets"}, nil, ""},
		{"ConfigMap przez subPath", map[string]any{"name": "config", "mountPath": "/app/app.yml", "subPath": "app.yml"}, nil, ""},
		{"Secret przez subPath", map[string]any{"name": "secrets", "mountPath": "/run/secrets/t", "subPath": "t"}, nil, "subPath"},
		{"zmienna z Secreta", map[string]any{"name": "secrets", "mountPath": "/run/secrets"}, []any{map[string]any{"name": "TOKEN", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "s", "key": "t"}}}}, "zmienna TOKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(Check([]map[string]any{deployment(tt.mount, tt.env)}, Options{RotatableSecrets: true}), "; ")
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Errorf("naruszenia %q, oczekiwano %q", got, tt.want)
			}
		})
	}
}

// TestHardened: pod naszego charta utwardzony ponad PSA restricted — każde
// pole z osobna, bo PSA części z nich nie sprawdza wcale (decyzja 55).
func TestHardened(t *testing.T) {
	hardened := `
apiVersion: apps/v1
kind: Deployment
metadata: {name: docfind-api}
spec:
  template:
    spec:
      automountServiceAccountToken: false
      securityContext: {runAsNonRoot: true, runAsUser: 10001, seccompProfile: {type: RuntimeDefault}}
      containers:
        - name: api
          image: x@sha256:abc
          resources: {requests: {cpu: 10m, memory: 64Mi}, limits: {memory: 128Mi}}
          securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
`
	check := func(src string) string {
		docs, err := manifest.Parse(strings.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(Check(docs, Options{Hardened: true}), "; ")
	}
	if got := check(hardened); got != "" {
		t.Fatalf("utwardzony pod odrzucony: %s", got)
	}
	for _, tt := range []struct{ name, old, new, want string }{
		{"token konta usługi", "automountServiceAccountToken: false", "automountServiceAccountToken: true", "automountServiceAccountToken"},
		{"token domyślny", "      automountServiceAccountToken: false\n", "", "automountServiceAccountToken"},
		{"zapisywalny system plików", "readOnlyRootFilesystem: true", "readOnlyRootFilesystem: false", "readOnlyRootFilesystem"},
		{"eskalacja uprawnień", "allowPrivilegeEscalation: false", "allowPrivilegeEscalation: true", "allowPrivilegeEscalation"},
		{"capabilities bez drop ALL", "capabilities: {drop: [ALL]}", "capabilities: {drop: [NET_RAW]}", "capabilities.drop"},
		{"bez runAsNonRoot", "runAsNonRoot: true, ", "", "runAsNonRoot"},
		{"root w kontenerze", "readOnlyRootFilesystem: true,", "readOnlyRootFilesystem: true, runAsUser: 0,", "runAsUser 0"},
		{"seccomp Unconfined w kontenerze", "readOnlyRootFilesystem: true,", "readOnlyRootFilesystem: true, seccompProfile: {type: Unconfined},", "seccompProfile"},
		{"bez seccomp", ", seccompProfile: {type: RuntimeDefault}", "", "seccompProfile"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := strings.Replace(hardened, tt.old, tt.new, 1)
			if src == hardened {
				t.Fatalf("zamiana %q nie zmieniła manifestu", tt.old)
			}
			if got := check(src); !strings.Contains(got, tt.want) {
				t.Errorf("naruszenia %q, oczekiwano %q", got, tt.want)
			}
		})
	}
	// Bez opcji (charty z zewnątrz) te reguły nie obowiązują.
	docs, _ := manifest.Parse(strings.NewReader(strings.Replace(hardened, "readOnlyRootFilesystem: true", "readOnlyRootFilesystem: false", 1)))
	if got := Check(docs, Options{}); len(got) > 0 {
		t.Errorf("chart z zewnątrz oceniony regułami utwardzenia: %v", got)
	}
}

func TestImmutableConfig(t *testing.T) {
	check := func(src string, opts Options) []string {
		docs, err := manifest.Parse(strings.NewReader(src))
		if err != nil {
			t.Fatal(err)
		}
		return Check(docs, opts)
	}
	immutable := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: api-config-77c4a94277}\nimmutable: true\ndata: {app.yml: x}\n"
	if got := check(immutable, Options{ImmutableConfig: true}); len(got) > 0 {
		t.Errorf("niezmienna ConfigMapa odrzucona: %v", got)
	}
	mutable := strings.Replace(immutable, "immutable: true\n", "", 1)
	if got := check(mutable, Options{ImmutableConfig: true}); len(got) != 1 || !strings.Contains(got[0], "brak immutable: true") {
		t.Errorf("zmienna ConfigMapa: %v", got)
	}
	if got := check(mutable, Options{}); len(got) > 0 {
		t.Errorf("chart z zewnątrz oceniony regułą ConfigMapy: %v", got)
	}
}
