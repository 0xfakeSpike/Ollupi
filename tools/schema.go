package tools

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// ValidateArguments performs full JSON Schema validation, including required
// fields, value types, nested objects, arrays, bounds, and enums.
func ValidateArguments(args map[string]any, schema any) error {
	if schema == nil {
		return fmt.Errorf("missing input schema")
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("marshal input schema: %w", err)
	}
	var parsed jsonschema.Schema
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("parse input schema: %w", err)
	}
	resolved, err := parsed.Resolve(nil)
	if err != nil {
		return fmt.Errorf("resolve input schema: %w", err)
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := resolved.Validate(args); err != nil {
		return fmt.Errorf("validate arguments: %w", err)
	}
	return nil
}
