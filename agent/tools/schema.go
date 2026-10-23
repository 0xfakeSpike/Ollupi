package tools

import (
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

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

func normalizeInputSchema(schema any) (map[string]any, error) {
	if schema == nil {
		return nil, fmt.Errorf("missing schema")
	}
	if value, ok := schema.(map[string]any); ok {
		return value, nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}
