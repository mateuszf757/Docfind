package crdschema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mateuszf757/Docfind/tools/internal/kit/manifest"
)

const crd = `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: gateways.example.io}
spec:
  group: example.io
  names: {kind: Gateway}
  versions:
    - name: v1
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                port: {type: integer, maximum: 9223372036854775807}
                note: {type: string, nullable: true}
                patch: {type: object, x-kubernetes-preserve-unknown-fields: true, properties: {a: {type: string}}}
`

func TestWrite(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(crd))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if n, err := Write(docs, dir); err != nil || n != 1 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "example.io", "gateway_v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	// Duża liczba bez utraty precyzji (json.Number, nie float64).
	if !strings.Contains(text, "9223372036854775807") {
		t.Errorf("maximum straciło precyzję:\n%s", text)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	spec := schema["properties"].(map[string]any)["spec"].(map[string]any)
	if spec["additionalProperties"] != false {
		t.Error("obiekt z właściwościami bez additionalProperties: false — literówka przeszłaby")
	}
	props := spec["properties"].(map[string]any)
	if _, ok := props["patch"].(map[string]any)["additionalProperties"]; ok {
		t.Error("pole z x-kubernetes-preserve-unknown-fields zamknięte")
	}
	if typ, _ := props["note"].(map[string]any)["type"].([]any); len(typ) != 2 {
		t.Errorf("nullable bez typu null: %v", props["note"])
	}
	if _, err := Write(nil, dir); err == nil {
		t.Error("brak CRD przyjęty")
	}
}

// TestStrictFieldNamedLikeKeyword: pola o nazwach słów kluczowych schematu
// (CRD SecretStore z ESO ma pole „properties") i dane w default nie są
// traktowane jak schemat. Wcześniej mapa pól dostawała additionalProperties:
// false — „pole" ze schematem false, którego kubeconform nie kompiluje
// i zgłasza jako brak schematu — a pole „nullable" znikało.
func TestStrictFieldNamedLikeKeyword(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"rules": map[string]any{"type": "array", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"properties": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"nullable":   map[string]any{"type": "boolean"},
				},
			}},
			"options": map[string]any{"type": "object", "default": map[string]any{"nullable": true, "properties": "x"}},
		},
	}
	out := Strict(schema).(map[string]any)
	rule := out["properties"].(map[string]any)["rules"].(map[string]any)["items"].(map[string]any)
	fields := rule["properties"].(map[string]any)
	if _, ok := fields["additionalProperties"]; ok {
		t.Errorf("mapa pól dostała additionalProperties: %v", fields)
	}
	if _, ok := fields["nullable"]; !ok {
		t.Error("pole o nazwie nullable usunięte")
	}
	if rule["additionalProperties"] != false {
		t.Error("obiekt reguły nie jest zamknięty")
	}
	def := out["properties"].(map[string]any)["options"].(map[string]any)["default"].(map[string]any)
	if def["nullable"] != true || def["properties"] != "x" {
		t.Errorf("wartość default zmieniona: %v", def)
	}
}
