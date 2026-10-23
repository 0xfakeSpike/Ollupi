package userinput

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0xfakeSpike/ollupi/agent/tools"
)

const Tool = "request_user_input"

type Question struct {
	ID       string   `json:"id"`
	Header   string   `json:"header"`
	Question string   `json:"question"`
	Options  []Option `json:"options,omitempty"`
}

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Answer struct {
	Answers []string `json:"answers"`
}

type Request struct {
	Questions []Question `json:"questions"`
}

func Decode(raw json.RawMessage) (Request, error) {
	var r Request
	if err := json.Unmarshal(raw, &r); err != nil {
		return Request{}, fmt.Errorf("user input request: %w", err)
	}
	return r, nil
}

func (r Request) validate() error {
	seen := make(map[string]bool, len(r.Questions))
	for _, q := range r.Questions {
		if seen[q.ID] {
			return fmt.Errorf("question id %q is duplicated", q.ID)
		}
		seen[q.ID] = true
		if strings.TrimSpace(q.Question) == "" || strings.TrimSpace(q.Header) == "" {
			return fmt.Errorf("question %q needs a non-blank header and question", q.ID)
		}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" {
				return fmt.Errorf("question %q has a blank option label", q.ID)
			}
		}
	}
	return nil
}

func (r Request) Check(answers map[string]Answer, maxRunes int) error {
	total := 0
	for _, q := range r.Questions {
		answer, ok := answers[q.ID]
		if !ok || len(answer.Answers) == 0 {
			return fmt.Errorf("question %q has no answer", q.ID)
		}
		for _, text := range answer.Answers {
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("question %q has a blank answer", q.ID)
			}
			total += len([]rune(text))
		}
	}
	if len(answers) != len(r.Questions) {
		return fmt.Errorf("answers name a question that was not asked")
	}
	if total > maxRunes {
		return fmt.Errorf("answers have %d characters; at most %d are accepted", total, maxRunes)
	}
	return nil
}

var schema = map[string]any{
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

type Provider struct{}

func New() *Provider { return &Provider{} }

func (*Provider) Name() (string, string) { return "user_input", "Asks the user questions." }

func (*Provider) ToolList(context.Context) ([]*tools.Spec, error) {
	return []*tools.Spec{{
		Name: Tool,
		Description: "Asks the user one to three short questions and waits for the answers. Use it when a specific answer is required before safe progress is possible " +
			"and you cannot find it out with other tools; ask instead of guessing. Make it the only call of its response: other calls in the same response run before the user answers.",
		InputSchema: schema,
		Annotations: &tools.Annotations{Title: "Question", ReadOnlyHint: true, OpenWorldHint: tools.Hint(false)},
	}}, nil
}

func (*Provider) Invoke(_ context.Context, _ string, arguments map[string]any) (tools.Result, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return tools.Result{}, err
	}
	r, err := Decode(raw)
	if err != nil {
		return tools.Result{}, err
	}
	if err := r.validate(); err != nil {
		return tools.Result{}, fmt.Errorf("invalid questions: %w", err)
	}
	return tools.Awaiting(tools.AwaitUser, r)
}
