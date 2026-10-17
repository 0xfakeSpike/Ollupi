// Package tools aggregates toolset providers into one flat registry:
// it indexes tools by globally unique name, validates arguments, and
// dispatches calls. It knows no toolset by name. The toolsets live in its
// subpackages (shell, mcp, opencli, skills).
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/0xfakeSpike/ollupi/execution"
	"log"
	"sort"
	"strings"
)

type Registry struct {
	providers map[string]Provider
	// owner maps each tool name to the provider that serves it.
	owner map[string]Provider
	specs map[string]*Spec
	// descriptors is the model-facing catalog, snapshotted at startup.
	descriptors []ToolDescriptor
}

// New indexes providers. Tool names must be unique across providers. It does
// not take ownership of providers on failure; the caller closes them.
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
		descriptor := ToolDescriptor{Provider: id, Name: name, Description: spec.Description, InputSchema: schema}
		if spec.Annotations != nil {
			descriptor.Annotations = *spec.Annotations
		}
		if p, ok := r.owner[name].(RedactionPolicyProvider); ok {
			descriptor.Redact = p.RedactionPolicies()[name]
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

// ToolDescriptors returns the complete model-facing catalog, ordered by
// provider and tool name. It is a startup snapshot.
func (r *Registry) ToolDescriptors(context.Context) ([]ToolDescriptor, error) {
	return append([]ToolDescriptor(nil), r.descriptors...), nil
}

// Invoke validates arguments against the tool schema, dispatches the call,
// and normalizes the result. A provider error and an error inside the result
// are the same thing to the agent: a failed tool call.
func (r *Registry) Invoke(ctx context.Context, tool string, arguments map[string]any) Result {
	result, err := r.dispatch(ctx, tool, arguments)
	if err != nil {
		result.Err = err
	}
	result = result.WithInferredMeta(tool)
	if result.Err != nil {
		log.Printf("tool registry: tool=%s error=%s", tool, clip(result.Err.Error(), 320))
	} else {
		log.Printf("tool registry: tool=%s result_kind=%s count=%d empty=%v", tool, result.Kind, result.Count, result.Empty)
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

// HasPermissionContent reports whether permission rules may name tool with
// a specifier (see PermissionContentProvider).
func (r *Registry) HasPermissionContent(tool string) bool {
	p, ok := r.owner[tool].(PermissionContentProvider)
	return ok && p.HasPermissionContent(tool)
}

// PermissionContent is the text of a call that rule specifiers match; ok is
// false when the tool takes no specifier.
func (r *Registry) PermissionContent(tool string, arguments map[string]any) (content string, ok bool, err error) {
	if !r.HasPermissionContent(tool) {
		return "", false, nil
	}
	content, err = r.owner[tool].(PermissionContentProvider).PermissionContent(tool, arguments)
	if err != nil {
		return "", false, fmt.Errorf("tool registry: permission content of %q: %w", tool, err)
	}
	return content, true, nil
}

func clip(s string, max int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "..."
}

// Close closes every closable provider.
func (r *Registry) Close() error {
	var err error
	for _, id := range r.providerIDs() {
		if c, ok := r.providers[id].(ClosableProvider); ok {
			if cerr := c.Close(); cerr != nil {
				err = errors.Join(err, fmt.Errorf("tool registry: close %q: %w", id, cerr))
			}
		}
	}
	return err
}

func (r *Registry) providerIDs() []string {
	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
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

// Prepare resolves local execution parameters before the Runner requests user
// approval. Tools without local execution semantics return nil.
func (r *Registry) Prepare(ctx context.Context, tool string, arguments map[string]any, scope *execution.Context) (*execution.Prepared, error) {
	p, ok := r.owner[tool]
	if !ok {
		return nil, fmt.Errorf("tool registry: unknown tool %q", tool)
	}
	if err := ValidateArguments(arguments, r.specs[tool].InputSchema); err != nil {
		return nil, err
	}
	if scoped, ok := p.(ExecutionProvider); ok {
		return scoped.Prepare(ctx, tool, arguments, scope)
	}
	return nil, nil
}

// InvokePrepared dispatches with the saved local execution record. It never
// silently reconstructs missing parameters on a resumed scoped call.
func (r *Registry) InvokePrepared(ctx context.Context, tool string, arguments map[string]any, scope *execution.Context, prepared *execution.Prepared) Result {
	p, ok := r.owner[tool]
	if !ok {
		return Result{Err: fmt.Errorf("tool registry: unknown tool %q", tool)}.WithInferredMeta(tool)
	}
	if err := ValidateArguments(arguments, r.specs[tool].InputSchema); err != nil {
		return Result{Err: err}.WithInferredMeta(tool)
	}
	if scoped, ok := p.(ExecutionProvider); ok {
		result, err := scoped.InvokePrepared(ctx, tool, arguments, scope, prepared)
		if err != nil {
			result.Err = err
		}
		return result.WithInferredMeta(tool)
	}
	if prepared != nil {
		return Result{Err: fmt.Errorf("tool registry: tool %q no longer supports saved execution parameters", tool)}.WithInferredMeta(tool)
	}
	return r.Invoke(ctx, tool, arguments)
}
