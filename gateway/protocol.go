package gateway

import (
	"encoding/json"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/agent/tools/userinput"
	"github.com/0xfakeSpike/ollupi/repo"
)

const errNotInitialized = -32002

const (
	methodInitialize           = "initialize"
	methodInitialized          = "initialized"
	methodThreadStart          = "thread/start"
	methodThreadResume         = "thread/resume"
	methodThreadList           = "thread/list"
	methodThreadRead           = "thread/read"
	methodTurnStart            = "turn/start"
	methodTurnSteer            = "turn/steer"
	methodTurnInterrupt        = "turn/interrupt"
	methodModelList            = "model/list"
	methodWorkspaceList        = "workspace/list"
	methodWorkspaceAdd         = "workspace/add"
	methodWorkspaceRename      = "workspace/rename"
	methodWorkspaceArchive     = "workspace/archive"
	methodWorkspacePermissions = "workspace/updatePermissions"

	methodToolCall     = "tool/call"
	methodMemoryList   = "memory/list"
	methodMemoryStatus = "memory/status"
	methodMemoryUpdate = "memory/update"
	methodMemoryForget = "memory/forget"
)

const (
	notifyThreadStarted       = "thread/started"
	notifyTurnStarted         = "turn/started"
	notifyItemStarted         = "item/started"
	notifyItemCompleted       = "item/completed"
	notifyAgentMessageDelta   = "item/agentMessage/delta"
	notifyReasoningDelta      = "item/reasoning/textDelta"
	notifyTurnCompleted       = "turn/completed"
	notifyThreadStatusChanged = "thread/status/changed"
)

const (
	requestApproval  = "item/toolCall/requestApproval"
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

type threadResult struct {
	Thread repo.ThreadRow `json:"thread"`
}

type threadListResult struct {
	Data []repo.ThreadSummary `json:"data"`
}

type threadStatusNotification struct {
	ThreadID string            `json:"threadId"`
	Status   repo.ThreadStatus `json:"status"`
}

type toolCallParams struct {
	ThreadID  string         `json:"threadId,omitempty"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type toolCallResult struct {
	Output any `json:"output"`
}

type userInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type turnStartParams struct {
	ThreadID   string      `json:"threadId"`
	Input      []userInput `json:"input"`
	Connection string      `json:"connection,omitempty"`
}

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
	Turn repo.TurnRow `json:"turn"`
}

type turnNotification struct {
	ThreadID string       `json:"threadId"`
	Turn     repo.TurnRow `json:"turn"`
}

type deltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

type itemNotification struct {
	ThreadID string       `json:"threadId"`
	TurnID   string       `json:"turnId"`
	Item     repo.ItemRow `json:"item"`
}

type approvalRequestParams struct {
	PreparedExecution  *tools.Prepared `json:"preparedExecution,omitempty"`
	ThreadID           string          `json:"threadId"`
	TurnID             string          `json:"turnId"`
	ItemID             string          `json:"itemId"`
	ApprovalID         string          `json:"approvalId"`
	Server             string          `json:"server"`
	Tool               string          `json:"tool"`
	Arguments          json.RawMessage `json:"arguments"`
	Reason             string          `json:"reason,omitempty"`
	AvailableDecisions []repo.Decision `json:"availableDecisions"`
}

type approvalResponse struct {
	Decision repo.Decision `json:"decision"`
}

type userInputRequestParams struct {
	ThreadID  string               `json:"threadId"`
	TurnID    string               `json:"turnId"`
	ItemID    string               `json:"itemId"`
	Questions []userinput.Question `json:"questions"`
}

type userInputResponse struct {
	Answers map[string]userinput.Answer `json:"answers"`
}
