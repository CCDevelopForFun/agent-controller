package adl

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas/adl.v1alpha1.json
var adlSchemaBytes []byte

//go:embed schemas/runtimebinding.v1alpha1.json
var runtimeBindingSchemaBytes []byte

// Validator validates a parsed document against the right schema based on
// its `kind`. v0.3.2 added kind-dispatch — prior versions only knew about
// the ADL Agent schema. Today the Validator handles two kinds:
//
//   - Agent (validated against adl.v1alpha1.json)
//   - RuntimeBinding (validated against runtimebinding.v1alpha1.json)
//
// Both schemas live under `agent-controller.dev/v1alpha1` but are distinct
// resources. Future kinds (e.g. Workflow, Conversation) will plug into the
// same dispatch table.
type Validator struct {
	agentSchema   *jsonschema.Schema
	bindingSchema *jsonschema.Schema
}

// NewValidator compiles the embedded schemas. Failure here is a
// programmer error (the embedded schemas must always compile) — callers
// can treat it as fatal at startup.
func NewValidator() (*Validator, error) {
	compile := func(id string, raw []byte) (*jsonschema.Schema, error) {
		var schemaDoc any
		if err := json.Unmarshal(raw, &schemaDoc); err != nil {
			return nil, fmt.Errorf("decode %s: %w", id, err)
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource(id, schemaDoc); err != nil {
			return nil, fmt.Errorf("add %s: %w", id, err)
		}
		s, err := c.Compile(id)
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", id, err)
		}
		return s, nil
	}

	agent, err := compile("adl.v1alpha1.json", adlSchemaBytes)
	if err != nil {
		return nil, err
	}
	binding, err := compile("runtimebinding.v1alpha1.json", runtimeBindingSchemaBytes)
	if err != nil {
		return nil, err
	}
	return &Validator{agentSchema: agent, bindingSchema: binding}, nil
}

// Validate checks doc against the schema matching its `kind` field.
//
// The returned error includes all schema violations in a human-readable
// form. Unknown kinds produce a clear error rather than passing silently —
// in v0.1.x/v0.2.x the validator accepted only `kind: Agent` implicitly;
// v0.3.2 makes the dispatch explicit so adding a new kind requires touching
// this switch (and a matching schema, and a test).
func (v *Validator) Validate(doc map[string]any) error {
	kind, _ := doc["kind"].(string)
	var schema *jsonschema.Schema
	var label string
	switch kind {
	case "Agent":
		schema = v.agentSchema
		label = "ADL"
	case "RuntimeBinding":
		schema = v.bindingSchema
		label = "RuntimeBinding"
	case "":
		return fmt.Errorf("missing required field: kind")
	default:
		return fmt.Errorf("unknown kind %q (supported: Agent, RuntimeBinding)", kind)
	}
	if err := schema.Validate(doc); err != nil {
		var verr *jsonschema.ValidationError
		var buf bytes.Buffer
		if errorsAs(err, &verr) {
			fmt.Fprintf(&buf, "%s validation failed:\n", label)
			for _, cause := range verr.Causes {
				fmt.Fprintf(&buf, "  - %s\n", cause)
			}
		} else {
			buf.WriteString(err.Error())
		}
		return fmt.Errorf("%s", buf.String())
	}
	return nil
}

// errorsAs is a tiny indirection so we can test or stub later if needed.
func errorsAs(err error, target any) bool {
	return jsonschema_errorsAs(err, target)
}

// jsonschema_errorsAs delegates to the stdlib errors.As; named here so the
// shim above stays alphabetized near the top of the file.
func jsonschema_errorsAs(err error, target any) bool {
	// indirection so callers don't import errors here
	return stdErrorsAs(err, target)
}
