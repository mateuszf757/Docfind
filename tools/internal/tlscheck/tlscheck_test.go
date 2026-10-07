package tlscheck

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGatewayAndConditions(t *testing.T) {
	gw := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"listeners": []any{
			map[string]any{"name": "http", "port": int64(80)},
			map[string]any{"name": "https", "hostname": "local.docfind.lol"},
		}},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Accepted", "status": "True"},
			map[string]any{"type": "Programmed", "status": "False"},
		}},
	}}
	if h := httpsHostname(gw); h != "local.docfind.lol" {
		t.Errorf("httpsHostname = %q", h)
	}
	if !conditionTrue(gw, "Accepted") || conditionTrue(gw, "Programmed") || conditionTrue(gw, "Brak") {
		t.Error("conditionTrue czyta warunki źle")
	}
}
