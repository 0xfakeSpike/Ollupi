package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/0xfakeSpike/ollupi/agent"
	"github.com/0xfakeSpike/ollupi/agent/tools/userinput"
	"github.com/0xfakeSpike/ollupi/pkg/jsonrpc"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/coder/websocket"
)

const (
	maxMessageBytes = 1 << 20
	defaultLimit    = 20
	maxLimit        = 100
)

type Server struct {
	agent   *agent.Agent
	token   string
	methods map[string]Method
	origins []string
	mu      sync.Mutex
	conns   map[*conn]struct{}
	served  sync.WaitGroup
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.served.Add(1)
	defer s.served.Done()
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.origins})
	if err != nil {
		log.Printf("gateway: accept: %v", err)
		return
	}
	ws.SetReadLimit(maxMessageBytes)
	c := &conn{s: s, rpc: jsonrpc.NewConn(ws), threads: make(map[string]bool), asked: make(map[string]bool)}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()

	stop := context.AfterFunc(r.Context(), func() { ws.Close(websocket.StatusGoingAway, "server shutting down") })
	defer stop()
	err = c.rpc.Serve(context.Background(), c)
	switch status := websocket.CloseStatus(err); {
	case status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway || r.Context().Err() != nil:
		ws.Close(websocket.StatusNormalClosure, "")
	default:
		log.Printf("gateway: connection closed: %v", err)
		ws.Close(websocket.StatusInternalError, "connection error")
	}
}

func (s *Server) Deliver(e agent.Event) {
	if e.Kind == agent.EventThreadStatusChanged {
		s.broadcast(notifyThreadStatusChanged, threadStatusNotification{ThreadID: e.Thread.ID, Status: e.Status})
		return
	}
	for _, c := range s.connections() {
		if c.watches(e.Thread.ID) || (e.Thread.ParentThreadID != "" && c.watches(e.Thread.ParentThreadID)) {
			c.event(e)
		}
	}
}

func (s *Server) broadcast(method string, payload any) {
	for _, c := range s.connections() {
		if c.isInitialized() {
			c.rpc.Notify(method, payload)
		}
	}
}

func (s *Server) connections() []*conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	return conns
}

type conn struct {
	s   *Server
	rpc *jsonrpc.Conn

	mu          sync.Mutex
	initialized bool
	threads     map[string]bool
	asked       map[string]bool
}

func (c *conn) isInitialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialized
}

func (c *conn) watches(threadID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threads[threadID]
}

func (c *conn) watch(threadID string) {
	c.mu.Lock()
	c.threads[threadID] = true
	c.mu.Unlock()
}

func (c *conn) Notify(method string, _ json.RawMessage) {
	if method != methodInitialized {
		log.Printf("gateway: ignored client notification %q", method)
	}
}

func (c *conn) Call(method string, params json.RawMessage) (any, func(), *jsonrpc.Error) {
	c.mu.Lock()
	initialized := c.initialized
	c.mu.Unlock()
	if method == methodInitialize {
		if initialized {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidRequest, "already initialized")
		}
		var p initializeParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		c.mu.Lock()
		c.initialized = true
		c.mu.Unlock()
		return initializeResult{ServerInfo: serverInfo{Name: "ollupi", Version: version()}}, nil, nil
	}
	if !initialized {
		return nil, nil, jsonrpc.Errorf(errNotInitialized, "call initialize first")
	}
	ag := c.s.agent
	switch method {
	case methodMemoryList, methodMemoryStatus, methodMemoryUpdate, methodMemoryForget:
		return c.memoryRequest(method, params)
	case methodWorkspaceList, methodWorkspaceAdd, methodWorkspaceRename, methodWorkspaceArchive:
		return c.workspaceRequest(method, params)
	case methodModelList:
		if err := jsonrpc.DecodeParams(params, &struct{}{}); err != nil {
			return nil, nil, err
		}
		return ag.ListModels(), nil, nil
	case methodThreadStart:
		var p threadStartParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		t, err := ag.StartThread(p.WorkspaceID, p.ApprovalPolicy)
		if err != nil {
			return nil, nil, serverError(err)
		}
		c.watch(t.ID)
		result := threadResult{Thread: t}
		return result, func() { c.rpc.Notify(notifyThreadStarted, result) }, nil

	case methodThreadResume, methodThreadRead:
		var p threadIDParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		view, ok := ag.ReadThread(p.ThreadID)
		if !ok {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "thread %q not found", p.ThreadID)
		}
		if method == methodThreadRead {
			return view, nil, nil
		}

		c.watch(p.ThreadID)
		after := func() {
			for _, r := range ag.PendingRequests(p.ThreadID) {
				c.request(r.Thread.ID, r.Item)
			}
		}
		return view, after, nil

	case methodThreadList:
		var p repo.ThreadFilter
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		if p.Limit == 0 {
			p.Limit = defaultLimit
		}
		if p.Limit < 1 || p.Limit > maxLimit {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "limit must be 1 to %d", maxLimit)
		}
		threads, err := ag.ListThreads(p)
		if err != nil {
			return nil, nil, serverError(err)
		}
		return threadListResult{Data: threads}, nil, nil

	case methodTurnStart:
		var p turnStartParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		text, rpcErr := inputText(p.Input)
		if rpcErr != nil {
			return nil, nil, rpcErr
		}
		if _, ok := ag.ReadThread(p.ThreadID); !ok {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "thread %q not found", p.ThreadID)
		}

		c.watch(p.ThreadID)
		turn, err := ag.StartTurn(p.ThreadID, text, p.Connection)
		if err != nil {
			return nil, nil, serverError(err)
		}
		return turnResult{Turn: turn}, nil, nil

	case methodTurnSteer:
		var p turnSteerParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		if p.ExpectedTurnID == "" {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "expectedTurnId is required")
		}
		text, rpcErr := inputText(p.Input)
		if rpcErr != nil {
			return nil, nil, rpcErr
		}
		if _, ok := ag.ReadThread(p.ThreadID); !ok {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "thread %q not found", p.ThreadID)
		}
		turn, err := ag.SteerTurn(p.ThreadID, p.ExpectedTurnID, text)
		switch {
		case errors.Is(err, agent.ErrNoActiveTurn), errors.Is(err, agent.ErrTurnMismatch):

			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidRequest, "%v", err)
		case err != nil:
			return nil, nil, serverError(err)
		}
		return turnSteerResult{TurnID: turn.ID}, nil, nil

	case methodTurnInterrupt:
		var p turnInterruptParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		view, ok := ag.ReadThread(p.ThreadID)
		if !ok {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "thread %q not found", p.ThreadID)
		}
		if turn, active := view.ActiveTurn(); !active || turn.ID != p.TurnID {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "turn %q is not in progress", p.TurnID)
		}
		if err := ag.InterruptTurn(p.ThreadID); err != nil {
			return nil, nil, serverError(err)
		}
		return struct{}{}, nil, nil

	case methodToolCall:
		var p toolCallParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		result, err := ag.CallTool(context.Background(), p.ThreadID, p.Tool, p.Arguments)
		if err != nil {
			return nil, nil, serverError(err)
		}
		if result.Err != nil {
			return nil, nil, serverError(result.Err)
		}
		return toolCallResult{Output: result.Output}, nil, nil

	default:
		if m, ok := c.s.methods[method]; ok {
			return callMethod(m, params)
		}
		return nil, nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
	}
}

func (c *conn) event(e agent.Event) {
	switch e.Kind {
	case agent.EventThreadStarted:
		c.rpc.Notify(notifyThreadStarted, threadResult{Thread: e.Thread})
	case agent.EventTurnStarted:
		c.rpc.Notify(notifyTurnStarted, turnNotification{ThreadID: e.Thread.ID, Turn: e.Turn})
	case agent.EventItemStarted:
		c.rpc.Notify(notifyItemStarted, itemNotification{ThreadID: e.Thread.ID, TurnID: e.Turn.ID, Item: *e.Item})
	case agent.EventItemCompleted:
		c.rpc.Notify(notifyItemCompleted, itemNotification{ThreadID: e.Thread.ID, TurnID: e.Turn.ID, Item: *e.Item})
	case agent.EventItemDelta:
		method := notifyAgentMessageDelta
		if e.Item.Type == repo.ItemReasoning {
			method = notifyReasoningDelta
		}
		c.rpc.Notify(method, deltaNotification{ThreadID: e.Thread.ID, TurnID: e.Turn.ID, ItemID: e.Item.ID, Delta: e.Delta})
	case agent.EventTurnCompleted:
		c.rpc.Notify(notifyTurnCompleted, turnNotification{ThreadID: e.Thread.ID, Turn: e.Turn})
	case agent.EventApprovalRequested, agent.EventUserInputRequested:
		c.request(e.Thread.ID, *e.Item)
	}
}

func (c *conn) request(threadID string, it repo.ItemRow) {
	c.mu.Lock()
	if c.asked[it.ID] {
		c.mu.Unlock()
		return
	}
	c.asked[it.ID] = true
	c.mu.Unlock()
	answered := func(result json.RawMessage, rerr *jsonrpc.Error) {
		c.mu.Lock()
		delete(c.asked, it.ID)
		c.mu.Unlock()
		if rerr != nil {
			log.Printf("gateway: client refused request for item %s: %v", it.ID, rerr)
			return
		}
		if err := c.resolve(threadID, it, result); err != nil {
			log.Printf("gateway: response for item %s: %v", it.ID, err)
		}
	}
	if !it.WaitsForApproval() {
		req, err := userinput.Decode(it.Await.Request)
		if err != nil {
			log.Printf("gateway: item %s: %v", it.ID, err)
			return
		}
		c.rpc.Request(requestUserInput, userInputRequestParams{ThreadID: threadID, TurnID: it.TurnID, ItemID: it.ID, Questions: req.Questions}, answered)
		return
	}
	args := json.RawMessage(it.Arguments)
	if strings.TrimSpace(it.Arguments) == "" {
		args = json.RawMessage("{}")
	}
	c.rpc.Request(requestApproval, approvalRequestParams{
		ThreadID: threadID, TurnID: it.TurnID, ItemID: it.ID, ApprovalID: it.Approval.ID,
		Server: it.Server, Tool: it.Tool, Arguments: args, PreparedExecution: it.PreparedExecution,
		Reason: it.Approval.Reason, AvailableDecisions: it.Approval.AvailableDecisions(),
	}, answered)
}

func (c *conn) resolve(threadID string, it repo.ItemRow, result json.RawMessage) error {
	if !it.WaitsForApproval() {
		var r userInputResponse
		if err := json.Unmarshal(result, &r); err != nil {
			return err
		}
		return c.s.agent.AnswerUserInput(threadID, it.ID, r.Answers)
	}
	var r approvalResponse
	if err := json.Unmarshal(result, &r); err != nil {
		return err
	}
	return c.s.agent.ResolveApproval(threadID, it.Approval.ID, r.Decision, "gateway")
}

func inputText(input []userInput) (string, *jsonrpc.Error) {
	texts := make([]string, 0, len(input))
	for _, in := range input {
		if in.Type != "text" {
			return "", jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "input type %q is not supported", in.Type)
		}
		texts = append(texts, in.Text)
	}
	return strings.Join(texts, "\n"), nil
}

func serverError(err error) *jsonrpc.Error {
	return jsonrpc.Errorf(jsonrpc.CodeServerError, "%v", err)
}

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}
