package tools

import (
	"context"
	"github.com/0xfakeSpike/ollupi/execution"
)

// Provider is a named tool source (shell, skills, one MCP server, ...) that
// exposes MCP-shaped tools. Toolsets implement it; the tool registry
// aggregates providers and dispatches by tool name. A provider owns the full
// contract of its tools: schema, MCP annotations, and execution.
type Provider interface {
	// Name returns the stable provider id and a short human description.
	Name() (id string, description string)
	// ToolList returns the provider's tools. It is called once at startup.
	ToolList(ctx context.Context) ([]*Spec, error)
	// Invoke runs one tool with arguments already validated against its schema.
	Invoke(ctx context.Context, tool string, arguments map[string]any) (Result, error)
}

// PermissionContentProvider is implemented by providers with tools that
// permission rules can narrow with a specifier, Tool(specifier), as in
// Claude Code's Bash(npm run *). The provider decides what text of a call the
// specifier matches, such as a command line.
type PermissionContentProvider interface {
	Provider
	// HasPermissionContent reports whether tool takes a specifier.
	HasPermissionContent(tool string) bool
	// PermissionContent is the text of one call of such a tool that
	// specifiers match. Arguments are already validated against the schema.
	PermissionContent(tool string, arguments map[string]any) (string, error)
}

// ClosableProvider holds external resources released at shutdown.
type ClosableProvider interface {
	Provider
	Close() error
}

// ExecutionProvider opts local tools into per-turn execution contexts. Prepare
// must not execute a command: its returned record is persisted before approval.
// Remote MCP tools keep their existing server-defined environment.
type ExecutionProvider interface {
	Provider
	Prepare(context.Context, string, map[string]any, *execution.Context) (*execution.Prepared, error)
	InvokePrepared(context.Context, string, map[string]any, *execution.Context, *execution.Prepared) (Result, error)
}
