package gateway

import (
	"encoding/json"

	"github.com/0xfakeSpike/ollupi/cron"
	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/thread"
)

// errNotInitialized is the server error for a request before initialize.
const errNotInitialized = -32002

// Client methods (Codex app-server names).
const (
	methodInitialize       = "initialize"
	methodInitialized      = "initialized"
	methodThreadStart      = "thread/start"
	methodThreadResume     = "thread/resume"
	methodThreadList       = "thread/list"
	methodThreadRead       = "thread/read"
	methodTurnStart        = "turn/start"
	methodTurnSteer        = "turn/steer"
	methodTurnInterrupt    = "turn/interrupt"
	methodModelList        = "model/list"
	methodWorkspaceList    = "workspace/list"
	methodWorkspaceAdd     = "workspace/add"
	methodWorkspaceRename  = "workspace/rename"
	methodWorkspaceArchive = "workspace/archive"

	// Ollupi extensions, outside the Codex protocol.
	methodCronList     = "cron/list"
	methodCronAdd      = "cron/add"
	methodCronRemove   = "cron/remove"
	methodCronRun      = "cron/run"
	methodConfigRead   = "config/read"
	methodConfigWrite  = "config/write"
	methodMemoryList   = "memory/list"
	methodMemoryStatus = "memory/status"
	methodMemoryUpdate = "memory/update"
	methodMemoryForget = "memory/forget"
)

// Server notifications, sent for the threads a connection started or
// resumed.
const (
	notifyThreadStarted = "thread/started"
	notifyTurnStarted   = "turn/started"
	notifyItemStarted   = "item/started"
	notifyItemCompleted = "item/completed"
	// Streamed text of an in-progress agentMessage or reasoning item.
	notifyAgentMessageDelta = "item/agentMessage/delta"
	notifyReasoningDelta    = "item/reasoning/textDelta"
	notifyTurnCompleted     = "turn/completed"
	// Sent to every connection, so thread lists stay current.
	notifyThreadStatusChanged = "thread/status/changed"
)

// Server requests. The client's response resumes the turn.
const (
	requestApproval  = "item/mcpToolCall/requestApproval"
	requestUserInput = "item/tool/requestUserInput"
)

type initializeParams struct {
	ClientInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ServerInfo serverInfo `json:"serverInfo"`
}

type threadStartParams struct {
	WorkspaceID    string `json:"workspaceId,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
}

type threadIDParams struct {
	ThreadID string `json:"threadId"`
}

type threadListParams struct {
	WorkspaceID *string `json:"workspaceId,omitempty"`
	Limit       int     `json:"limit,omitempty"`
	CronJobID   string  `json:"cronJobId,omitempty"`
}

type threadResult struct {
	Thread thread.Thread `json:"thread"`
}

// resumeResult is thread/resume's result: the thread with its turns and
// items, like thread/read.
type resumeResult struct {
	thread.View
}

type threadListResult struct {
	Data []thread.Summary `json:"data"`
}

type threadStatusNotification struct {
	ThreadID string        `json:"threadId"`
	Status   thread.Status `json:"status"`
}

type cronJobParams struct {
	ID string `json:"id"`
}

type cronAddParams struct {
	WorkspaceID string `json:"workspaceId,omitempty"`
	ID          string `json:"id"`
	Spec        string `json:"spec"`
	Prompt      string `json:"prompt"`
}

type cronListResult struct {
	Data []cron.JobStatus `json:"data"`
}

type cronRunResult struct {
	Thread thread.Thread `json:"thread"`
}

// configResult is config.json as stored, and where it lives.
type configResult struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type configWriteParams struct {
	Content string `json:"content"`
}

// userInput is one element of turn/start or turn/steer input. Only text
// is supported.
type userInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type turnStartParams struct {
	ThreadID       string      `json:"threadId"`
	Input          []userInput `json:"input"`
	ModelProfileID string      `json:"modelProfileId,omitempty"`
}

// turnSteerParams add input to the turn in progress, which must be
// ExpectedTurnID.
type turnSteerParams struct {
	ThreadID       string      `json:"threadId"`
	Input          []userInput `json:"input"`
	ExpectedTurnID string      `json:"expectedTurnId"`
}

type turnSteerResult struct {
	TurnID string `json:"turnId"`
}

type turnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type turnResult struct {
	Turn thread.Turn `json:"turn"`
}

type turnNotification struct {
	ThreadID string      `json:"threadId"`
	Turn     thread.Turn `json:"turn"`
}

type deltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type itemNotification struct {
	ThreadID string      `json:"threadId"`
	TurnID   string      `json:"turnId"`
	Item     thread.Item `json:"item"`
}

type approvalRequestParams struct {
	PreparedExecution *execution.Prepared `json:"preparedExecution,omitempty"`
	ThreadID          string              `json:"threadId"`
	TurnID            string              `json:"turnId"`
	ItemID            string              `json:"itemId"`
	ApprovalID        string              `json:"approvalId"`
	Server            string              `json:"server"`
	Tool              string              `json:"tool"`
	Arguments         json.RawMessage     `json:"arguments"`
	// Reason and AvailableDecisions as in Codex's approval requests.
	Reason             string            `json:"reason,omitempty"`
	AvailableDecisions []thread.Decision `json:"availableDecisions"`
}

type approvalResponse struct {
	Decision thread.Decision `json:"decision"`
}

type userInputRequestParams struct {
	ThreadID  string                     `json:"threadId"`
	TurnID    string                     `json:"turnId"`
	ItemID    string                     `json:"itemId"`
	Questions []thread.UserInputQuestion `json:"questions"`
}

type userInputResponse struct {
	Answers map[string]thread.UserInputAnswer `json:"answers"`
}
