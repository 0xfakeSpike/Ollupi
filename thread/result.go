package thread

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/0xfakeSpike/ollupi/tools"
)

// ToolResult is the stored result of a registry tool call. The output is
// stored in full; the error text is in Item.Error.
type ToolResult struct {
	Output    json.RawMessage         `json:"output,omitempty"`
	Kind      string                  `json:"kind,omitempty"`
	Count     int                     `json:"count,omitempty"`
	Empty     bool                    `json:"empty,omitempty"`
	Artifacts []tools.ContextArtifact `json:"artifacts,omitempty"`
}

// ResultOf converts a live tool result into its stored form and error text.
func ResultOf(result tools.Result) (*ToolResult, string, error) {
	out := &ToolResult{Kind: result.Kind, Count: result.Count, Empty: result.Empty, Artifacts: result.Artifacts}
	if result.Output != nil {
		raw, err := json.Marshal(result.Output)
		if err != nil {
			return nil, "", fmt.Errorf("encode tool output: %w", err)
		}
		out.Output = raw
	}
	if result.Err == nil {
		return out, "", nil
	}
	return out, result.Err.Error(), nil
}

// LiveResult rebuilds the live tool result of a registry tool call item.
func (it Item) LiveResult() (tools.Result, error) {
	if it.Result == nil {
		return tools.Result{}, fmt.Errorf("item %s has no tool result", it.ID)
	}
	o := it.Result
	r := tools.Result{Tool: it.Tool, Kind: o.Kind, Count: o.Count, Empty: o.Empty, Artifacts: o.Artifacts}
	if len(o.Output) > 0 {
		// UseNumber keeps large integers (ids, hashes) exact.
		dec := json.NewDecoder(bytes.NewReader(o.Output))
		dec.UseNumber()
		if err := dec.Decode(&r.Output); err != nil {
			return tools.Result{}, fmt.Errorf("decode tool output of item %s: %w", it.ID, err)
		}
	}
	if it.Error != "" {
		r.Err = errors.New(it.Error)
	}
	return r, nil
}

// Fingerprint identifies an exact tool invocation. encoding/json sorts map
// keys, so equal arguments always produce the same fingerprint.
func Fingerprint(tool string, arguments map[string]any) string {
	b, err := json.Marshal(struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
	}{tool, arguments})
	if err != nil {
		// Arguments are always decoded from JSON, so they always re-encode.
		panic(fmt.Sprintf("thread: fingerprint %s: %v", tool, err))
	}
	return string(b)
}

const maxArtifactRunes = 20000

// CheckArtifacts validates the instructions a tool result carries.
func CheckArtifacts(artifacts []tools.ContextArtifact) error {
	for _, artifact := range artifacts {
		if artifact.Name == "" || strings.TrimSpace(artifact.Content) == "" {
			return fmt.Errorf("context artifact requires a name and content")
		}
		if artifact.Kind != tools.ArtifactAgentSkill && artifact.Kind != tools.ArtifactInstructions {
			return fmt.Errorf("context artifact %s/%s has unsupported kind", artifact.Kind, artifact.Name)
		}
		if len([]rune(artifact.Content)) > maxArtifactRunes {
			return fmt.Errorf("context artifact %s/%s exceeds %d runes", artifact.Kind, artifact.Name, maxArtifactRunes)
		}
	}
	return nil
}
