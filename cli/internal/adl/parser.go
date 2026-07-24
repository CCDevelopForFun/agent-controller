// Package adl parses, validates, and compiles ADL YAML documents.
package adl

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

// Parse decodes ADL YAML bytes into a generic map. It does not validate
// shape — that's the validator's job — but it does fail fast on YAML
// syntax errors.
func Parse(data []byte) (map[string]any, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("yaml parse: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("yaml parse: empty document")
	}
	return doc, nil
}
