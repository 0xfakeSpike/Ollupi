// Package thread is the durable record of the assistant's work, named after
// the Codex app-server protocol: a Thread is a conversation, a Turn is one
// user input plus all the work the agent does for it, and an Item is the
// smallest unit inside a turn (user message, agent message, reasoning, tool
// call, plan, request for user input, compaction, error).
//
// Items are appended to their thread in order and never removed. An item is
// written when it starts and rewritten when it completes; its position never
// changes. Everything the model sees is rebuilt from the items.
package thread

import (
	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/tools"
	"time"
)

// Source is where a thread was started.
type Source string

const (
	// SourceGateway: started by a gateway client.
	SourceGateway Source = "gateway"
	// SourceCron: started by a cron job run.
	SourceCron Source = "cron"
	// SourceSubagent: started by a parent thread's Agent call; its one turn
	// does the task, and the parent's collabAgentToolCall item gets the
	// result.
	SourceSubagent Source = "subagent"
)

// Thread is one persistent conversation.
type Thread struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	// Preview is the first user message, for thread lists.
	Preview string `json:"preview"`
	Source  Source `json:"source"`
	// CronJobID is the cron job whose run started the thread.
	CronJobID string `json:"cronJobId,omitempty"`
	// ApprovalPolicy overrides the agent's approval policy for this thread.
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	// ParentThreadID and ParentItemID are the thread and the
	// collabAgentToolCall item that launched a subagent thread; Agent is the
	// subagent definition it runs.
	ParentThreadID string    `json:"parentThreadId,omitempty"`
	ParentItemID   string    `json:"parentItemId,omitempty"`
	Agent          string    `json:"agent,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type TurnStatus string

const (
	TurnInProgress  TurnStatus = "inProgress"
	TurnCompleted   TurnStatus = "completed"
	TurnInterrupted TurnStatus = "interrupted"
	TurnFailed      TurnStatus = "failed"
)

// Turn is one user input and the agent's work on it, up to its final
// answer, an interruption, or a failure. A turn waiting for the user's
// approval or answer stays in progress; see View.PendingRequest.
type Turn struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
	// Seq is the turn's 1-based position in its thread.
	Seq    int        `json:"seq"`
	Status TurnStatus `json:"status"`
	// Agent, Model, and ApprovalPolicy record the definition the turn ran.
	Agent            string             `json:"agent"`
	Model            string             `json:"model"`
	ModelProfileID   string             `json:"modelProfileId"`
	ModelSettings    ModelSettings      `json:"modelSettings"`
	ExecutionContext *execution.Context `json:"executionContext,omitempty"`
	ApprovalPolicy   string             `json:"approvalPolicy"`
	Usage            Usage              `json:"usage"`
	Error            string             `json:"error,omitempty"`
	StartedAt        time.Time          `json:"startedAt"`
	CompletedAt      *time.Time         `json:"completedAt,omitempty"`
}

// ModelSettings snapshots request options for an entire turn, including its
// later resumptions. False and unspecified are intentionally distinct.
type ModelSettings struct {
	EnableThinking *bool `json:"enableThinking,omitempty"`
}

// ContextStats snapshots the local input-token estimate before one model
// request. EstimatedTokens is the sum of SystemTokens, MessagesTokens, and
// ToolsTokens; these estimates are distinct from provider-reported Usage.
type ContextStats struct {
	BudgetTokens   int `json:"budgetTokens"`
	SystemTokens   int `json:"systemTokens"`
	MessagesTokens int `json:"messagesTokens"`
	// MemoryTokens is the memory context's share of MessagesTokens, not an
	// additional component of EstimatedTokens.
	MemoryTokens    int `json:"memoryTokens,omitempty"`
	ToolsTokens     int `json:"toolsTokens"`
	EstimatedTokens int `json:"estimatedTokens"`
}

// Usage is provider-reported token usage, named as in Codex.
type Usage struct {
	InputTokens       int64 `json:"inputTokens"`
	CachedInputTokens int64 `json:"cachedInputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
}

// Add accumulates u into the receiver.
func (t *Usage) Add(u Usage) {
	t.InputTokens += u.InputTokens
	t.CachedInputTokens += u.CachedInputTokens
	t.OutputTokens += u.OutputTokens
}

type ItemType string

const (
	ItemUserMessage  ItemType = "userMessage"
	ItemAgentMessage ItemType = "agentMessage"
	ItemReasoning    ItemType = "reasoning"
	// ItemMcpToolCall is a call to a registry tool; every registry tool is
	// MCP-shaped, and Server names its provider.
	ItemMcpToolCall ItemType = "mcpToolCall"
	// ItemTodoList is an update_plan call and the plan it set.
	ItemTodoList ItemType = "todoList"
	// ItemUserInputRequest is a request_user_input call and the user's
	// answers.
	ItemUserInputRequest ItemType = "userInputRequest"
	// ItemContextCompaction is a summary of the thread so far, written by
	// the model when the context filled up; the model sees it instead of
	// every earlier item.
	ItemContextCompaction ItemType = "contextCompaction"
	// ItemMemoryContext freezes the selected long-term memories for one turn.
	// It is contextual data, not a model response or user instruction.
	ItemMemoryContext ItemType = "memoryContext"
	// ItemToolProgress is a line a long-running tool streamed into the feed.
	// It is shown to the user but never replayed to the model.
	ItemToolProgress ItemType = "toolProgress"
	// ItemError is a runtime message to the model, such as the notice that
	// tool use stopped.
	ItemError ItemType = "error"
	// ItemCollabAgentToolCall is an Agent call (Codex's item name): it
	// launches a subagent thread and stays in progress until that thread's
	// turn ends; Text is then the subagent's final report.
	ItemCollabAgentToolCall ItemType = "collabAgentToolCall"
)

type ItemStatus string

// MemoryReference records exactly which revision and text a turn consulted.
// The empty workspace identifies global memory. Sources and change history
// remain in the memory store rather than being copied into every prompt.
type MemoryReference struct {
	ID          string `json:"id"`
	Revision    int    `json:"revision"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	Topic       string `json:"topic,omitempty"`
	Text        string `json:"text"`
}

const (
	ItemInProgress ItemStatus = "inProgress"
	ItemCompleted  ItemStatus = "completed"
	ItemFailed     ItemStatus = "failed"
	// ItemDeclined is a tool call the user declined to approve.
	ItemDeclined ItemStatus = "declined"
)

// Item is one unit of a turn. Exactly the fields of its Type are set.
type Item struct {
	ID     string     `json:"id"`
	TurnID string     `json:"turnId"`
	Seq    int        `json:"seq"`
	Type   ItemType   `json:"type"`
	Status ItemStatus `json:"status"`

	// Text: userMessage, agentMessage, reasoning, error, the summary of a
	// contextCompaction, and the report of a collabAgentToolCall.
	Text string `json:"text,omitempty"`

	// ResponseID groups the items produced by one model response, so the
	// model's message can be replayed exactly.
	ResponseID string `json:"responseId,omitempty"`

	// ContextStats and Usage belong to one model request. The runner puts
	// them on exactly one item for each ResponseID, even when that response
	// produces several items. Usage is absent unless the provider reports it,
	// including successful requests whose provider omits usage information.
	// A turn's Usage continues to accumulate usage across its requests.
	ContextStats *ContextStats `json:"contextStats,omitempty"`
	Usage        *Usage        `json:"usage,omitempty"`
	// Memory is present only on memoryContext items. A completed empty item
	// also freezes selection, so a resumed turn does not retrieve again.
	Memory []MemoryReference `json:"memory,omitempty"`

	// Tool calls (mcpToolCall, todoList, userInputRequest): the call
	// exactly as the model produced it. Arguments is the raw JSON text.
	CallID            string              `json:"callId,omitempty"`
	Tool              string              `json:"tool,omitempty"`
	Arguments         string              `json:"arguments,omitempty"`
	Server            string              `json:"server,omitempty"`
	Annotations       *tools.Annotations  `json:"annotations,omitempty"`
	Approval          *Approval           `json:"approval,omitempty"`
	PreparedExecution *execution.Prepared `json:"preparedExecution,omitempty"`
	// Result is set once a registry tool ran.
	Result *ToolResult `json:"result,omitempty"`
	// Error says why a call failed, was refused, or was declined.
	Error string `json:"error,omitempty"`

	// collabAgentToolCall: the subagent launched, set once the call is
	// accepted, and the thread it runs in, set once that thread exists.
	Agent             string   `json:"agent,omitempty"`
	ReceiverThreadIDs []string `json:"receiverThreadIds,omitempty"`

	// todoList
	Plan *Plan `json:"plan,omitempty"`

	// userInputRequest: the questions, and the answers keyed by question
	// id once the user replied.
	Questions []UserInputQuestion        `json:"questions,omitempty"`
	Answers   map[string]UserInputAnswer `json:"answers,omitempty"`

	StartedAt   time.Time  `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// IsCall reports whether the item answers one model tool call.
func (it Item) IsCall() bool {
	return it.Type == ItemMcpToolCall || it.Type == ItemTodoList || it.Type == ItemUserInputRequest || it.Type == ItemCollabAgentToolCall
}

// WaitsOnAgent reports whether the item is an Agent call whose subagent has
// not reported yet.
func (it Item) WaitsOnAgent() bool {
	return it.Type == ItemCollabAgentToolCall && it.Status == ItemInProgress && it.Agent != ""
}

// NewUserMessage is the completed userMessage item for text the user sent
// to a turn, when it starts or while it runs.
func NewUserMessage(id, turnID, text string, at time.Time) Item {
	return Item{ID: id, TurnID: turnID, Type: ItemUserMessage, Status: ItemCompleted, Text: text, StartedAt: at, CompletedAt: &at}
}

// Approval is the user's decision on a tool call the approval policy or a
// permission rule held.
type Approval struct {
	ID          string    `json:"id"`
	RequestedAt time.Time `json:"requestedAt"`
	// Reason says why the call needs approval, when not just the approval
	// policy.
	Reason string `json:"reason,omitempty"`
	// Rule is the ask permission rule that held the call, if one did. Such a
	// call asks every time, so it cannot be accepted for the session.
	Rule      string     `json:"rule,omitempty"`
	Decision  Decision   `json:"decision,omitempty"`
	DecidedBy string     `json:"decidedBy,omitempty"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
	// Error says why no decision was obtained.
	Error string `json:"error,omitempty"`
}

// Decision values follow Codex's approval responses.
type Decision string

const (
	DecisionAccept Decision = "accept"
	// DecisionAcceptForSession accepts the call and, for the rest of the
	// thread, the same command, or any call of a tool whose calls have no
	// specifier content.
	DecisionAcceptForSession Decision = "acceptForSession"
	DecisionDecline          Decision = "decline"
)

// AvailableDecisions are the decisions the user may give.
func (a Approval) AvailableDecisions() []Decision {
	if a.Rule != "" {
		return []Decision{DecisionAccept, DecisionDecline}
	}
	return []Decision{DecisionAccept, DecisionAcceptForSession, DecisionDecline}
}

// UserInputQuestion is one question of request_user_input (Codex). The
// user may pick an option or answer in their own words.
type UserInputQuestion struct {
	ID       string            `json:"id"`
	Header   string            `json:"header"`
	Question string            `json:"question"`
	Options  []UserInputOption `json:"options,omitempty"`
}

type UserInputOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// UserInputAnswer is the user's answer to one question.
type UserInputAnswer struct {
	Answers []string `json:"answers"`
}

// Status is where a thread stands, as in Codex: idle (no turn in progress),
// active while a turn runs, or waiting on the user's approval or answers.
type Status string

const (
	StatusIdle               Status = "idle"
	StatusActive             Status = "active"
	StatusWaitingOnApproval  Status = "waitingOnApproval"
	StatusWaitingOnUserInput Status = "waitingOnUserInput"
)

// Status derives the thread's status from its turns and items.
func (v View) Status() Status {
	if _, active := v.ActiveTurn(); !active {
		return StatusIdle
	}
	it, waiting := v.PendingRequest()
	switch {
	case !waiting:
		return StatusActive
	case it.Type == ItemUserInputRequest:
		return StatusWaitingOnUserInput
	default:
		return StatusWaitingOnApproval
	}
}

// Summary is a thread with its status, as listed by thread/list.
type Summary struct {
	Thread
	Status Status `json:"status"`
}

// View is a thread with its turns and items, as returned by thread/read.
type View struct {
	Thread Thread `json:"thread"`
	Turns  []Turn `json:"turns"`
	Items  []Item `json:"items"`
}

// ActiveTurn returns the thread's turn in progress.
func (v View) ActiveTurn() (Turn, bool) {
	if n := len(v.Turns); n > 0 && v.Turns[n-1].Status == TurnInProgress {
		return v.Turns[n-1], true
	}
	return Turn{}, false
}

// AgentCalls returns the turn in progress's Agent calls still waiting for
// their subagents.
func (v View) AgentCalls() []Item {
	turn, ok := v.ActiveTurn()
	if !ok {
		return nil
	}
	var out []Item
	for _, it := range v.Items {
		if it.TurnID == turn.ID && it.WaitsOnAgent() {
			out = append(out, it)
		}
	}
	return out
}

// FinalAnswer returns the text of the last completed agentMessage of turn.
func (v View) FinalAnswer(turnID string) (string, bool) {
	for i := len(v.Items) - 1; i >= 0; i-- {
		if it := v.Items[i]; it.TurnID == turnID && it.Type == ItemAgentMessage && it.Status == ItemCompleted {
			return it.Text, true
		}
	}
	return "", false
}

// PendingRequest returns the item the turn in progress waits on: a tool
// call awaiting the user's approval, or unanswered questions.
func (v View) PendingRequest() (Item, bool) {
	turn, ok := v.ActiveTurn()
	if !ok {
		return Item{}, false
	}
	for _, it := range v.Items {
		if it.TurnID == turn.ID && it.IsPending() {
			return it, true
		}
	}
	return Item{}, false
}

// IsPending reports whether the item waits for the user.
func (it Item) IsPending() bool {
	if it.Status != ItemInProgress {
		return false
	}
	switch it.Type {
	case ItemMcpToolCall:
		return it.Approval != nil && it.Approval.Decision == ""
	case ItemUserInputRequest:
		return len(it.Questions) > 0 && it.Answers == nil
	default:
		return false
	}
}
