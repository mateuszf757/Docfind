// Package crdschema generuje schematy JSON dla kubeconform z definicji CRD
// (dawniej ci/crd_schemas.py, decyzja 28).
//
// Schematy powstają z tych samych CRD, które instalują przypięte charty,
// a nie z zewnętrznego katalogu. Gotowe katalogi schematów CRD nie nadążają
// za wydaniami — walidacja względem starszej wersji CRD przepuszczałaby
// pola, których API server nie przyjmie, albo odrzucała nowe.
//
// Tryb ścisły: obiekty z listą właściwości dostają additionalProperties:
// false, chyba że CRD jawnie dopuszcza nieznane pola
// (x-kubernetes-preserve-unknown-fields). Literówka w nazwie pola zasobu jest
// wtedy błędem, a nie polem po cichu pominiętym przez API server — tak jak
// -strict dla zasobów wbudowanych.
package crdschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mateuszf757/Docfind/tools/internal/manifest"
)

// Strict zwraca kopię schematu OpenAPI v3 w postaci, którą rozumie walidator
// JSON Schema.
func Strict(schema any) any {
	switch v := schema.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = Strict(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, value := range v {
			out[key] = Strict(value)
		}
		// OpenAPI `nullable` nie istnieje w JSON Schema — null trzeba
		// dopuścić typem.
		if nullable, _ := out["nullable"].(bool); nullable {
			if t, ok := out["type"].(string); ok {
				out["type"] = []any{t, "null"}
			}
		}
		delete(out, "nullable")
		preserves, _ := out["x-kubernetes-preserve-unknown-fields"].(bool)
		_, hasProperties := out["properties"]
		_, hasAdditional := out["additionalProperties"]
		if hasProperties && !hasAdditional && !preserves {
			out["additionalProperties"] = false
		}
		return out
	default:
		return v
	}
}

// Write zapisuje <dir>/<grupa>/<rodzaj>_<wersja>.json dla każdej wersji
// każdego CRD — w układzie, którego oczekuje kubeconform:
//
//	-schema-location '<dir>/{{ .Group }}/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json'
func Write(docs []map[string]any, dir string) (int, error) {
	written := 0
	for _, doc := range docs {
		if manifest.String(doc, "kind") != "CustomResourceDefinition" {
			continue
		}
		spec := manifest.Map(doc, "spec")
		group := manifest.String(spec, "group")
		kind := strings.ToLower(manifest.String(manifest.Map(spec, "names"), "kind"))
		if group == "" || kind == "" {
			return written, fmt.Errorf("CRD %q bez grupy albo rodzaju", manifest.String(manifest.Map(doc, "metadata"), "name"))
		}
		for _, raw := range manifest.List(spec, "versions") {
			version, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			schema, ok := manifest.Map(manifest.Map(version, "schema"), "openAPIV3Schema"), true
			if !ok || schema == nil {
				continue
			}
			path := filepath.Join(dir, group, fmt.Sprintf("%s_%s.json", kind, manifest.String(version, "name")))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return written, err
			}
			data, err := json.MarshalIndent(Strict(schema), "", " ")
			if err != nil {
				return written, err
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return written, err
			}
			written++
		}
	}
	if written == 0 {
		return 0, errors.New("na wejściu nie było żadnego CRD ze schematem")
	}
	return written, nil
}
