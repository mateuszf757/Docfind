// Package manifest czyta wielodokumentowy YAML Kubernetesa (wyjście
// `helm template`, CRD) do map, z tą samą konwersją YAML→JSON co API server
// (sigs.k8s.io/yaml) i z liczbami jako json.Number — duża liczba w schemacie
// CRD (np. maximum 2^63-1) nie traci precyzji w float64.
package manifest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Parse dzieli strumień na dokumenty i zwraca niepuste obiekty.
func Parse(r io.Reader) ([]map[string]any, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReaderSize(r, 1<<20))
	var docs []map[string]any
	for i := 1; ; i++ {
		raw, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("dokument %d: %w", i, err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		data, err := yaml.YAMLToJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("dokument %d: %w", i, err)
		}
		if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("dokument %d: oczekiwano obiektu: %w", i, err)
		}
		if len(doc) > 0 {
			docs = append(docs, doc)
		}
	}
}

// Map zwraca pod-mapę pod kluczem albo nil.
func Map(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

// List zwraca listę pod kluczem albo nil.
func List(m map[string]any, key string) []any {
	v, _ := m[key].([]any)
	return v
}

// String zwraca napis pod kluczem albo "".
func String(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
