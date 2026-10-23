package tools

import (
	"context"
	"fmt"
)

const (
	introspectionProvider = "tool_registry"
	ToolListProviders     = "list_tool_providers"
	ToolListProviderTools = "list_tools_for_provider"
)

type introspection struct {
	reg *Registry
}

func newIntrospection(reg *Registry) *introspection {
	return &introspection{reg: reg}
}

func (*introspection) Name() (string, string) {
	return introspectionProvider, "Lists the assistant's tool providers and their tools."
}

func (*introspection) ToolList(context.Context) ([]*Spec, error) {
	readCatalog := &Annotations{ReadOnlyHint: true, OpenWorldHint: Hint(false)}
	return []*Spec{
		{
			Name:        ToolListProviders,
			Annotations: readCatalog,
			Description: "List tool providers with their id and description. Use only when the user asks what the assistant can do.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
		{
			Name:        ToolListProviderTools,
			Annotations: readCatalog,
			Description: "List the tools of one provider. Use only when the user asks about a provider's capabilities.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"provider": map[string]any{"type": "string", "minLength": 1, "description": `Provider id from list_tool_providers.`},
				},
				"required":             []string{"provider"},
				"additionalProperties": false,
			},
		},
	}, nil
}

func (i *introspection) Invoke(_ context.Context, tool string, args map[string]any) (Result, error) {
	switch tool {
	case ToolListProviders:
		providers := make([]map[string]string, 0, len(i.reg.providers))
		for _, id := range i.reg.providerIDs() {
			_, description := i.reg.providers[id].Name()
			providers = append(providers, map[string]string{"id": id, "description": description})
		}
		return Result{Output: map[string]any{"providers": providers}}, nil
	case ToolListProviderTools:
		provider, _ := args["provider"].(string)
		if _, ok := i.reg.providers[provider]; !ok {
			err := fmt.Errorf("tool registry: unknown provider %q", provider)
			return Result{Err: err}, err
		}
		out := make([]ToolDescriptor, 0)
		for _, descriptor := range i.reg.descriptors {
			if descriptor.Provider == provider {
				out = append(out, descriptor)
			}
		}
		return Result{Output: map[string]any{"provider": provider, "tools": out}}, nil
	default:
		err := fmt.Errorf("tool registry: unknown tool %q", tool)
		return Result{Err: err}, err
	}
}
