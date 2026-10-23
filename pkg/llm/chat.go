package llm

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ChatMessage struct {
	Role             Role       `json:"role"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning,omitempty"`
	ToolCalls        []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID       string     `json:"toolCallId,omitempty"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolSpec struct {
	Name        string
	Description string
	Parameters  map[string]any
}

type ToolChoice string

const (
	ToolChoiceAuto ToolChoice = "auto"
	ToolChoiceNone ToolChoice = "none"
)

type Model struct {
	Name       string `json:"name"`
	Connection string `json:"connection"`
	Thinking   *bool  `json:"thinking,omitempty"`
}

func (m Model) Clone() Model {
	if m.Thinking != nil {
		t := *m.Thinking
		m.Thinking = &t
	}
	return m
}

type ChatRequest struct {
	Model             Model
	Messages          []ChatMessage
	Tools             []ToolSpec
	ToolChoice        ToolChoice
	ParallelToolCalls bool
}

type Usage struct {
	PromptTokens       int64
	CachedPromptTokens int64
	CompletionTokens   int64
}

type ChatResponse struct {
	Message       ChatMessage
	FinishReason  string
	Usage         Usage
	UsageReported bool
}

type Delta struct {
	Content   string
	Reasoning string
}
