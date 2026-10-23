package tools

import (
	"context"
	"fmt"
	"slices"
	"sort"
)

type Registry struct {
	providers   map[string]Provider
	owner       map[string]Provider
	specs       map[string]*Spec
	descriptors []ToolDescriptor
}

func New(ctx context.Context, providers ...Provider) (*Registry, error) {
	r := &Registry{
		providers: make(map[string]Provider, len(providers)+1),
		owner:     make(map[string]Provider),
		specs:     make(map[string]*Spec),
	}
	for _, p := range append(providers, newIntrospection(r)) {
		id, _ := p.Name()
		if id == "" {
			return nil, fmt.Errorf("tool registry: provider with empty name")
		}
		if _, exists := r.providers[id]; exists {
			return nil, fmt.Errorf("tool registry: duplicate provider %q", id)
		}
		r.providers[id] = p
	}
	for _, id := range r.providerIDs() {
		p := r.providers[id]
		list, err := p.ToolList(ctx)
		if err != nil {
			return nil, fmt.Errorf("tool registry: list tools of %q: %w", id, err)
		}
		for _, t := range list {
			if t.Name == "" {
				return nil, fmt.Errorf("tool registry: provider %q exposes a tool without a name", id)
			}
			if prev, ok := r.owner[t.Name]; ok {
				prevID, _ := prev.Name()
				return nil, fmt.Errorf("tool registry: duplicate tool %q (providers %q and %q)", t.Name, prevID, id)
			}
			r.owner[t.Name] = p
			r.specs[t.Name] = t
		}
	}
	descriptors, err := r.buildDescriptors()
	if err != nil {
		return nil, err
	}
	r.descriptors = descriptors
	return r, nil
}

func (r *Registry) buildDescriptors() ([]ToolDescriptor, error) {
	out := make([]ToolDescriptor, 0, len(r.specs))
	for name, spec := range r.specs {
		schema, err := normalizeInputSchema(spec.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool registry: tool %q input schema: %w", name, err)
		}
		id, _ := r.owner[name].Name()
		descriptor := ToolDescriptor{Provider: id, Name: name, Description: spec.Description, InputSchema: schema, Serial: spec.Serial}
		if spec.Annotations != nil {
			descriptor.Annotations = *spec.Annotations
		}
		out = append(out, descriptor)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider == out[j].Provider {
			return out[i].Name < out[j].Name
		}
		return out[i].Provider < out[j].Provider
	})
	return out, nil
}

func (r *Registry) Descriptors() []ToolDescriptor {
	return slices.Clone(r.descriptors)
}

func (r *Registry) Invoke(ctx context.Context, tool string, arguments map[string]any) Result {
	result, err := r.dispatch(ctx, tool, arguments)
	if err != nil {
		result.Err = err
	}
	return result.For(tool)
}

func (r *Registry) Call(ctx context.Context, caller Caller, tool string, arguments map[string]any) Result {
	if spec, ok := r.specs[tool]; ok && spec.Local != nil {
		return Result{Err: fmt.Errorf("tool registry: %s runs only in a turn", tool)}.For(tool)
	}
	result := r.Invoke(WithCaller(ctx, caller), tool, arguments)
	if result.Err == nil && result.Await != nil {
		return Result{Err: fmt.Errorf("tool registry: %s is answered only in a turn", tool)}.For(tool)
	}
	return result
}

func (r *Registry) dispatch(ctx context.Context, tool string, arguments map[string]any) (Result, error) {
	p, ok := r.owner[tool]
	if !ok {
		return Result{}, fmt.Errorf("tool registry: unknown tool %q", tool)
	}
	if err := ValidateArguments(arguments, r.specs[tool].InputSchema); err != nil {
		return Result{}, fmt.Errorf("tool registry: invalid arguments for %q: %w", tool, err)
	}
	return p.Invoke(ctx, tool, arguments)
}

func (r *Registry) HasPermissionContent(tool string) bool {
	spec, ok := r.specs[tool]
	return ok && spec.PermissionContent != nil
}

func (r *Registry) PermissionContent(tool string, arguments map[string]any) (content string, ok bool, err error) {
	if !r.HasPermissionContent(tool) {
		return "", false, nil
	}
	content, err = r.specs[tool].PermissionContent(arguments)
	if err != nil {
		return "", false, fmt.Errorf("tool registry: permission content of %q: %w", tool, err)
	}
	return content, true, nil
}

func (r *Registry) providerIDs() []string {
	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *Registry) Prepare(ctx context.Context, tool string, arguments map[string]any) (*Prepared, error) {
	if _, ok := r.owner[tool]; !ok {
		return nil, fmt.Errorf("tool registry: unknown tool %q", tool)
	}
	if err := ValidateArguments(arguments, r.specs[tool].InputSchema); err != nil {
		return nil, err
	}
	if local := r.specs[tool].Local; local != nil {
		return local.Prepare(ctx, arguments)
	}
	return nil, nil
}

func (r *Registry) InvokePrepared(ctx context.Context, tool string, arguments map[string]any, prepared *Prepared) Result {
	if _, ok := r.owner[tool]; !ok {
		return Result{Err: fmt.Errorf("tool registry: unknown tool %q", tool)}.For(tool)
	}
	if err := ValidateArguments(arguments, r.specs[tool].InputSchema); err != nil {
		return Result{Err: err}.For(tool)
	}
	if local := r.specs[tool].Local; local != nil {
		result, err := local.Run(ctx, arguments, prepared)
		if err != nil {
			result.Err = err
		}
		return result.For(tool)
	}
	if prepared != nil {
		return Result{Err: fmt.Errorf("tool registry: tool %q no longer supports saved execution parameters", tool)}.For(tool)
	}
	return r.Invoke(ctx, tool, arguments)
}
