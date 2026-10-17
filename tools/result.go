package tools

import (
	"reflect"
	"strings"
)

// Result is the normalized result of one tool invocation.
type Result struct {
	Output    any
	Err       error
	Kind      string
	Count     int
	Empty     bool
	Tool      string
	Artifacts []ContextArtifact
}

const (
	ArtifactAgentSkill = "agent_skill"
	// ArtifactInstructions is the AGENTS.md of a project subdirectory,
	// loaded when a command first runs there. Name is the file's path.
	ArtifactInstructions = "instructions"
)

// ContextArtifact is instruction content that arrives with a tool result
// (an activated skill, a subdirectory's AGENTS.md) and must remain
// available independently of the bounded execution trajectory.
type ContextArtifact struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Source  string `json:"source,omitempty"`
	Digest  string `json:"digest,omitempty"`
}

func (r Result) WithInferredMeta(tool string) Result {
	r.Tool = strings.TrimSpace(tool)
	if r.Err != nil {
		r.Kind = "error"
		r.Empty = true
		r.Count = 0
		return r
	}
	if r.Kind == "" {
		r.Kind = inferOutputKind(r.Output)
	}
	if r.Count == 0 {
		r.Count = inferOutputCount(r.Output)
	}
	r.Empty = inferOutputEmpty(r.Output, r.Count)
	return r
}

func inferOutputKind(v any) string {
	switch x := v.(type) {
	case nil:
		return "empty"
	case string:
		return "text"
	case []any:
		return "list"
	case map[string]any:
		for _, key := range []string{"relations", "tools", "skills", "providers", "jobs"} {
			if _, ok := x[key]; ok {
				return key
			}
		}
		if _, ok := x["stdout"]; ok {
			return "command"
		}
		return "object"
	default:
		return "value"
	}
}

func inferOutputCount(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case string:
		if strings.TrimSpace(x) == "" {
			return 0
		}
		return 1
	case []any:
		return len(x)
	case map[string]any:
		for _, key := range []string{"relations", "tools", "skills", "providers", "jobs"} {
			items, exists := x[key]
			if !exists {
				continue
			}
			switch items := items.(type) {
			case []any:
				return len(items)
			case []map[string]any:
				return len(items)
			case []string:
				return len(items)
			}
			value := reflect.ValueOf(items)
			if value.IsValid() && (value.Kind() == reflect.Slice || value.Kind() == reflect.Array) {
				return value.Len()
			}
		}
		if stdout, ok := x["stdout"].(string); ok && strings.TrimSpace(stdout) != "" {
			return 1
		}
		return len(x)
	default:
		return 1
	}
}

func inferOutputEmpty(v any, count int) bool {
	if v == nil || count == 0 {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}
