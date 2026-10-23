package tools

import (
	"context"
)

type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

type Spec struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	InputSchema any          `json:"inputSchema"`
	Annotations *Annotations `json:"annotations,omitempty"`

	PermissionContent func(arguments map[string]any) (string, error) `json:"-"`
	Local             *Local                                         `json:"-"`
	Serial            bool                                           `json:"-"`
}

type Local struct {
	Prepare func(ctx context.Context, arguments map[string]any) (*Prepared, error)
	Run     func(ctx context.Context, arguments map[string]any, p *Prepared) (Result, error)
}

type ToolDescriptor struct {
	Provider    string         `json:"provider"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Annotations Annotations    `json:"annotations"`
	InputSchema map[string]any `json:"input_schema"`
	Serial      bool           `json:"-"`
}

func OpenWorld(a Annotations) bool {
	return a.OpenWorldHint == nil || *a.OpenWorldHint
}

func Hint(v bool) *bool {
	return &v
}
