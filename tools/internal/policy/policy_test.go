package policy

import (
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/manifest"
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
