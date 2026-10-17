package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/tools"
)

// Runtime tools are handled by the Runner itself rather than the tool registry.
const (
	ToolUpdatePlan       = "update_plan"
	ToolRequestUserInput = "request_user_input"
	// ToolAgent launches a subagent (Claude Code's Agent tool).
	ToolAgent = "Agent"
)

// updatePlanSchema matches Codex's update_plan tool.
var updatePlanSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"explanation": map[string]any{"type": "string", "maxLength": 500},
		"plan": map[string]any{
			"type":     "array",
			"minItems": 1,
			"maxItems": 12,
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"step":   map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
					"status": map[string]any{"type": "string", "enum": []any{"pending", "in_progress", "completed"}},
				},
				"required":             []any{"step", "status"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []any{"plan"},
	"additionalProperties": false,
}

// requestUserInputSchema matches Codex's request_user_input tool.
var requestUserInputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"questions": map[string]any{
			"type":        "array",
			"minItems":    1,
			"maxItems":    3,
			"description": "Questions to show the user. Prefer 1 and do not exceed 3.",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":       map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_]{0,63}$", "description": "Stable identifier for mapping answers (snake_case)."},
					"header":   map[string]any{"type": "string", "minLength": 1, "maxLength": 12, "description": "Short header label shown in the UI (12 or fewer chars)."},
					"question": map[string]any{"type": "string", "minLength": 1, "maxLength": 600, "description": "Single-sentence prompt shown to the user, in the user's language."},
					"options": map[string]any{
						"type":        "array",
						"maxItems":    4,
						"description": "Optional mutually exclusive choices. The user can always answer in their own words instead.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"label":       map[string]any{"type": "string", "minLength": 1, "maxLength": 60, "description": "User-facing label (1-5 words)."},
								"description": map[string]any{"type": "string", "maxLength": 200, "description": "One short sentence explaining the impact of this option."},
							},
							"required":             []any{"label"},
							"additionalProperties": false,
						},
					},
				},
				"required":             []any{"id", "header", "question"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []any{"questions"},
	"additionalProperties": false,
}

func runtimeToolDescriptors() []tools.ToolDescriptor {
	return []tools.ToolDescriptor{
		{
			Provider:    "runtime",
			Name:        ToolUpdatePlan,
			Description: "Updates the task plan. Provide an optional explanation and a list of plan items, each with a step and status. At most one step can be in_progress at a time.",
			InputSchema: updatePlanSchema,
		},
		{
			Provider:    "runtime",
			Name:        ToolRequestUserInput,
			Description: "Request user input for one to three short questions and wait for the response. Use only for information that is required before safe progress is possible and cannot be obtained with other tools.",
			InputSchema: requestUserInputSchema,
		},
	}
}

// agentToolDescriptor is the Agent tool offered to an agent with
// subagents. Its description lists them, so it is the same on every call.
func agentToolDescriptor(subagents []Subagent) tools.ToolDescriptor {
	names := make([]any, 0, len(subagents))
	var b strings.Builder
	b.WriteString("Launch a subagent to do one task on its own and report back. The subagent knows nothing of this conversation: put everything it needs in prompt, including what its report must contain. " +
		"To work on independent tasks at the same time, call Agent several times in the same response; the subagents run in parallel. " +
		"The turn waits until every subagent launched in the response has reported, and each call returns that subagent's final report. Subagents cannot launch subagents.\n\nAvailable agents (subagent_type):")
	for _, s := range subagents {
		names = append(names, s.Name)
		fmt.Fprintf(&b, "\n- %s: %s", s.Name, s.Description)
	}
	return tools.ToolDescriptor{
		Provider:    "runtime",
		Name:        ToolAgent,
		Description: b.String(),
		InputSchema: agentToolSchema(names),
	}
}

func agentToolSchema(names []any) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"description":   map[string]any{"type": "string", "minLength": 1, "maxLength": 80, "description": "A short (3-5 word) description of the task, shown to the user."},
			"prompt":        map[string]any{"type": "string", "minLength": 1, "maxLength": 20000, "description": "The complete task for the subagent, self-contained."},
			"subagent_type": map[string]any{"type": "string", "enum": names, "description": "Which agent to launch."},
		},
		"required":             []any{"description", "prompt", "subagent_type"},
		"additionalProperties": false,
	}
}

// AgentCall is the decoded arguments of an Agent call.
type AgentCall struct {
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type"`
}

// ParseAgentCall decodes and checks the raw arguments of an Agent call.
func ParseAgentCall(arguments string, subagents []Subagent) (AgentCall, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return AgentCall{}, fmt.Errorf("arguments are not a valid JSON object: %v", err)
	}
	names := make([]any, 0, len(subagents))
	for _, s := range subagents {
		names = append(names, s.Name)
	}
	var call AgentCall
	if err := decodeInto(args, agentToolSchema(names), &call); err != nil {
		return AgentCall{}, err
	}
	if strings.TrimSpace(call.Prompt) == "" || strings.TrimSpace(call.Description) == "" {
		return AgentCall{}, fmt.Errorf("description and prompt must not be blank")
	}
	return call, nil
}

func decodeInto(args map[string]any, schema map[string]any, out any) error {
	if err := tools.ValidateArguments(args, schema); err != nil {
		return err
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func parsePlan(args map[string]any) (thread.Plan, error) {
	var plan thread.Plan
	if err := decodeInto(args, updatePlanSchema, &plan); err != nil {
		return thread.Plan{}, err
	}
	if err := thread.ValidatePlan(plan); err != nil {
		return thread.Plan{}, err
	}
	return plan, nil
}
func parseQuestions(args map[string]any) ([]thread.UserInputQuestion, error) {
	var req struct {
		Questions []thread.UserInputQuestion `json:"questions"`
	}
	if err := decodeInto(args, requestUserInputSchema, &req); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(req.Questions))
	for _, q := range req.Questions {
		if seen[q.ID] {
			return nil, fmt.Errorf("question id %q is duplicated", q.ID)
		}
		seen[q.ID] = true
		if strings.TrimSpace(q.Question) == "" || strings.TrimSpace(q.Header) == "" {
			return nil, fmt.Errorf("question %q needs a non-blank header and question", q.ID)
		}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" {
				return nil, fmt.Errorf("question %q has a blank option label", q.ID)
			}
		}
	}
	return req.Questions, nil
}
