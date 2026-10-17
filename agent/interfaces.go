package agent

import (
	"context"

	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/llm"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/tools"
)

// ChatModel performs one native tool-calling completion, streaming answer
// and reasoning text to onDelta as it arrives.
type ChatModel interface {
	Chat(ctx context.Context, req llm.ChatRequest, onDelta func(llm.Delta)) (llm.ChatResponse, error)
}

// ToolRegistry is the tool catalog and its dispatcher. Invoke runs one call
// with arguments already validated by the Runner and reports failures in the
// result. Permission rules match a call's content when its tool takes a
// specifier (see tools.PermissionContentProvider).
type ToolRegistry interface {
	ToolDescriptors(ctx context.Context) ([]tools.ToolDescriptor, error)
	Invoke(ctx context.Context, tool string, arguments map[string]any) tools.Result
	HasPermissionContent(tool string) bool
	PermissionContent(tool string, arguments map[string]any) (content string, ok bool, err error)
}

// SkillCatalog summarizes the available Agent Skills for the system prompt.
type SkillCatalog interface {
	Descriptors() []map[string]any
}

// Recorder persists one item of the turn: a new item is appended, an
// existing one rewritten. It returns the stored item. The Runner changes its
// copy of the thread only after the recorder accepted the item, so the
// stored record is never behind what the Runner saw.
type Recorder interface {
	Record(item thread.Item) (thread.Item, error)
	// Delta announces text streamed into an in-progress agentMessage or
	// reasoning item. It is not persisted: the completed item holds the
	// whole text.
	Delta(item thread.Item, text string)
}

// PendingInput holds the messages the user sends to the turn while it runs
// (Codex's turn/steer). The Runner takes them between model calls, once
// every tool result of the last response is recorded, and records each as a
// userMessage item of the turn.
type PendingInput interface {
	// Take returns the messages sent since the last take, oldest first.
	Take() []string
	// TakeOrClose is Take when messages wait. Otherwise it closes the input,
	// refusing later messages, because the turn is about to end.
	TakeOrClose() []string
}

// ExecutionToolRegistry is implemented by the real registry for explicit
// per-turn environments. The legacy interface remains usable in isolated tests.
type ExecutionToolRegistry interface {
	Prepare(context.Context, string, map[string]any, *execution.Context) (*execution.Prepared, error)
	InvokePrepared(context.Context, string, map[string]any, *execution.Context, *execution.Prepared) tools.Result
}
