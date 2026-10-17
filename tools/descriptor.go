package tools

// Annotations are the safety hints a tool declares. They replace the MCP SDK
// type so the core library does not depend on MCP.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Spec is the provider-independent definition of one tool: its name,
// description, input schema, and annotations.
type Spec struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	InputSchema any          `json:"inputSchema"`
	Annotations *Annotations `json:"annotations,omitempty"`
}

// ToolDescriptor is the model-facing, provider-independent tool contract.
// Annotations are the tool's MCP annotations; a tool without annotations has
// the MCP defaults (not read-only, destructive, open world).
type ToolDescriptor struct {
	Provider    string         `json:"provider"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Annotations Annotations    `json:"annotations"`
	InputSchema map[string]any `json:"input_schema"`
	// Redact says how the runner treats placeholders in this tool's
	// arguments. RedactMask (the default) leaves them as-is; RedactRestore
	// resolves them to their real values before the tool runs.
	Redact RedactionPolicy `json:"redact,omitempty"`
}

// RedactionPolicy says whether a tool may receive the real values behind
// redaction placeholders.
type RedactionPolicy string

const (
	// RedactMask leaves placeholders as-is when the model passes them to a
	// tool, so the tool never sees the real value. It is the default and the
	// safe choice for tools that could exfiltrate data (shell, network, ...).
	RedactMask RedactionPolicy = "mask"
	// RedactRestore resolves placeholders back to their real values before
	// the tool runs, for tools that must operate on real data (for example,
	// dialing a phone number). Opt tools in deliberately.
	RedactRestore RedactionPolicy = "restore"
)

// RedactionPolicyProvider optionally declares which of a provider's tools may
// receive the real values behind redaction placeholders. Tools not listed
// keep the RedactMask default.
type RedactionPolicyProvider interface {
	RedactionPolicies() map[string]RedactionPolicy
}

// OpenWorld reports the effective openWorldHint (MCP default true).
func OpenWorld(a Annotations) bool {
	return a.OpenWorldHint == nil || *a.OpenWorldHint
}

// Hint returns a pointer for the optional annotation fields.
func Hint(v bool) *bool {
	return &v
}
