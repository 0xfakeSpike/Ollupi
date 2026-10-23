package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0xfakeSpike/ollupi/agent/tools"
)

const Tool = "update_plan"

type Status string

const (
	Pending    Status = "pending"
	InProgress Status = "in_progress"
	Completed  Status = "completed"
)

const maxSteps = 12

type Step struct {
	Step   string `json:"step"`
	Status Status `json:"status"`
}

type Plan struct {
	Explanation string `json:"explanation,omitempty"`
	Steps       []Step `json:"plan"`
}

func (p Plan) Validate() error {
	if len(p.Steps) == 0 || len(p.Steps) > maxSteps {
		return fmt.Errorf("plan requires 1 to %d steps", maxSteps)
	}
	inProgress := 0
	for i, step := range p.Steps {
		if strings.TrimSpace(step.Step) == "" {
			return fmt.Errorf("plan step %d is empty", i+1)
		}
		switch step.Status {
		case Pending, Completed:
		case InProgress:
			inProgress++
		default:
			return fmt.Errorf("plan step %d has invalid status %q", i+1, step.Status)
		}
	}
	if inProgress > 1 {
		return fmt.Errorf("at most one plan step can be in_progress")
	}
	return nil
}

var schema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"explanation": map[string]any{"type": "string", "maxLength": 500},
		"plan": map[string]any{
			"type":     "array",
			"minItems": 1,
			"maxItems": maxSteps,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"step":   map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
					"status": map[string]any{"type": "string", "enum": []any{string(Pending), string(InProgress), string(Completed)}},
				},
				"required":             []any{"step", "status"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []any{"plan"},
	"additionalProperties": false,
}

type Provider struct{}

func New() *Provider { return &Provider{} }

func (*Provider) Name() (string, string) { return "plan", "Keeps the task plan the user sees." }

func (*Provider) ToolList(context.Context) ([]*tools.Spec, error) {
	return []*tools.Spec{{
		Name: Tool,
		Description: "Updates the task plan the user sees. For non-trivial work with several distinct steps, call it with a short plan and keep it current: " +
			"exactly one step in_progress while you work on it, and mark steps completed as soon as they are done. Skip it for simple requests. Each call replaces the whole plan.",
		InputSchema: schema,
		Annotations: &tools.Annotations{Title: "Plan", ReadOnlyHint: true, OpenWorldHint: tools.Hint(false)},
	}}, nil
}

func (*Provider) Invoke(_ context.Context, _ string, arguments map[string]any) (tools.Result, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return tools.Result{}, err
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return tools.Result{}, err
	}
	if err := p.Validate(); err != nil {
		return tools.Result{}, fmt.Errorf("invalid plan: %w", err)
	}
	return tools.Result{Output: "Plan updated"}, nil
}
