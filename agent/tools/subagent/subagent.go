package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0xfakeSpike/ollupi/agent/tools"
)

const Tool = "Agent"

type Call struct {
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
}

func Decode(raw json.RawMessage) (Call, error) {
	var c Call
	if err := json.Unmarshal(raw, &c); err != nil {
		return Call{}, fmt.Errorf("agent call: %w", err)
	}
	return c, nil
}

type Provider struct{}

func New() *Provider { return &Provider{} }

func (*Provider) Name() (string, string) { return "subagents", "Launches subagents." }

func (p *Provider) ToolList(context.Context) ([]*tools.Spec, error) {
	return []*tools.Spec{{
		Name: Tool,
		Description: "Launch a subagent: another run of you, with your tools, that does one task on its own and reports back. " +
			"It knows nothing of this conversation: put everything it needs in prompt, including what its report must contain. " +
			"To work on independent tasks at the same time, call Agent several times in the same response; the subagents run in parallel. " +
			"The turn waits until every subagent launched in the response has reported, and each call returns that subagent's final report. Subagents cannot launch subagents.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{"type": "string", "minLength": 1, "maxLength": 80, "description": "A short (3-5 word) description of the task, shown to the user."},
				"prompt":      map[string]any{"type": "string", "minLength": 1, "maxLength": 20000, "description": "The complete task for the subagent, self-contained."},
			},
			"required":             []any{"description", "prompt"},
			"additionalProperties": false,
		},
		Annotations: &tools.Annotations{Title: "Subagent", ReadOnlyHint: true, OpenWorldHint: tools.Hint(false)},
	}}, nil
}

func (p *Provider) Invoke(_ context.Context, _ string, arguments map[string]any) (tools.Result, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return tools.Result{}, err
	}
	c, err := Decode(raw)
	if err != nil {
		return tools.Result{}, err
	}
	if strings.TrimSpace(c.Prompt) == "" || strings.TrimSpace(c.Description) == "" {
		return tools.Result{}, fmt.Errorf("description and prompt must not be blank")
	}
	return tools.Awaiting(tools.AwaitAgent, c)
}
