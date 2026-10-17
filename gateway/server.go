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

	"github.com/0xfakeSpike/ollupi/cron"
	"github.com/0xfakeSpike/ollupi/jsonrpc"
	"github.com/0xfakeSpike/ollupi/memory"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/threadmanager"
	"github.com/coder/websocket"
)

const (
	maxMessageBytes = 1 << 20
	defaultLimit    = 20
	maxLimit        = 100
)

// Server speaks the Codex app-server protocol, JSON-RPC 2.0 over a
// WebSocket. A connection receives the notifications and server requests of
// the threads it started, resumed, or started a turn on.
type Server struct {
	threads ThreadManager
	cron    Cron
	config  Config
	memory  memory.AdminStore
	runJob  CronRunner
	origins []string
	mu      sync.Mutex
	conns   map[*conn]struct{}
	// served counts connections being served. http.Server.Shutdown does not
	// wait for upgraded connections, so Wait does.
	served sync.WaitGroup
}

// ThreadManager is what the gateway needs from the thread manager.
type ThreadManager interface {
	StartThread(p threadmanager.StartThreadParams) (thread.Thread, error)
	ListThreads(f thread.Filter) []thread.Summary
	ReadThread(threadID string) (thread.View, bool)
	StartTurn(threadID, text string) (thread.Turn, error)
	StartTurnWithModel(threadID, text, modelProfileID string) (thread.Turn, error)
	ListModels() threadmanager.ModelList
	SteerTurn(threadID, expectedTurnID, text string) (thread.Turn, error)
	// PendingRequests are the requests waiting for the user in a
	// conversation, its subagents' included, oldest first.
	PendingRequests(threadID string) []threadmanager.Request
	InterruptTurn(threadID string) error
	ResolveApproval(threadID, approvalID string, decision thread.Decision, decidedBy string) error
	AnswerUserInput(threadID, itemID string, answers map[string]thread.UserInputAnswer) error
	Subscribe(fn func(threadmanager.Event))
}

// Cron is what the gateway needs from the cron scheduler.
type Cron interface {
	List() []cron.JobStatus
	Create(id, spec, prompt string) (cron.Job, error)
	Delete(id string) error
}

// CronRunner starts a cron job now; it returns the job run's new thread.
type CronRunner func(id string) (thread.Thread, error)

// Config reads and replaces the workspace config.json. A written config
// takes effect when the server restarts.
type Config interface {
	Path() string
	Read() ([]byte, error)
	Write(content []byte) error
}

// NewServer subscribes to the thread manager's events. Browser clients are
// accepted from localhost and from allowedOrigins (host patterns).
func NewServer(threads ThreadManager, jobs Cron, runJob CronRunner, cfg Config, allowedOrigins []string) *Server {
	origins := append([]string{"localhost:*", "127.0.0.1:*", "[::1]:*"}, allowedOrigins...)
	s := &Server{threads: threads, cron: jobs, runJob: runJob, config: cfg, origins: origins, conns: make(map[*conn]struct{})}
	threads.Subscribe(s.deliver)
	return s
}

// SetMemoryStore enables memory administration. Configure it before serving.
func (s *Server) SetMemoryStore(store memory.AdminStore) { s.memory = store }

// ServeHTTP upgrades one connection and serves it until it closes.
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
	// At shutdown the connection is closed with a close frame; reads use a
	// context of their own because cancelling a read drops the connection.
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

// Wait blocks until every connection has closed.
func (s *Server) Wait() {
	s.served.Wait()
}

// deliver forwards an event to every connection watching its thread, or
// the parent of a subagent thread. Status changes go to every initialized
// connection.
func (s *Server) deliver(e threadmanager.Event) {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		switch {
		case e.Kind == threadmanager.EventThreadStatusChanged:
			if c.isInitialized() {
				c.rpc.Notify(notifyThreadStatusChanged, threadStatusNotification{ThreadID: e.Thread.ID, Status: e.Status})
			}
		case c.watches(e.Thread.ID) || (e.Thread.ParentThreadID != "" && c.watches(e.Thread.ParentThreadID)):
			c.event(e)
		}
	}
}

// conn is one client connection. It implements jsonrpc.Handler.
type conn struct {
	s   *Server
	rpc *jsonrpc.Conn

	mu          sync.Mutex
	initialized bool
	threads     map[string]bool
	// asked holds the items this connection has an unanswered request for.
	asked map[string]bool
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

// Notify handles client notifications, which need no answer.
func (c *conn) Notify(method string, _ json.RawMessage) {
	if method != methodInitialized {
		log.Printf("gateway: ignored client notification %q", method)
	}
}

// Call runs one client request.
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
	threads := c.s.threads
	switch method {
	case methodMemoryList, methodMemoryStatus, methodMemoryUpdate, methodMemoryForget:
		return c.memoryRequest(method, params)
	case methodWorkspaceList, methodWorkspaceAdd, methodWorkspaceRename, methodWorkspaceArchive:
		return c.workspaceRequest(method, params)
	case methodModelList:
		if err := jsonrpc.DecodeParams(params, &struct{}{}); err != nil {
			return nil, nil, err
		}
		return threads.ListModels(), nil, nil
	case methodThreadStart:
		var p threadStartParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		t, err := threads.StartThread(threadmanager.StartThreadParams{Source: thread.SourceGateway, ApprovalPolicy: p.ApprovalPolicy, WorkspaceID: p.WorkspaceID})
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
		view, ok := threads.ReadThread(p.ThreadID)
		if !ok {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "thread %q not found", p.ThreadID)
		}
		if method == methodThreadRead {
			return view, nil, nil
		}
		// A resumed conversation's open requests, its subagents' included,
		// are sent again to this connection.
		c.watch(p.ThreadID)
		after := func() {
			for _, r := range threads.PendingRequests(p.ThreadID) {
				c.request(r.Thread.ID, r.Item)
			}
		}
		return resumeResult{view}, after, nil

	case methodThreadList:
		var p threadListParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		if p.Limit == 0 {
			p.Limit = defaultLimit
		}
		if p.Limit < 1 || p.Limit > maxLimit {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "limit must be 1 to %d", maxLimit)
		}
		return threadListResult{Data: threads.ListThreads(thread.Filter{CronJobID: p.CronJobID, Limit: p.Limit, WorkspaceID: p.WorkspaceID})}, nil, nil

	case methodTurnStart:
		var p turnStartParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		text, rpcErr := inputText(p.Input)
		if rpcErr != nil {
			return nil, nil, rpcErr
		}
		if _, ok := threads.ReadThread(p.ThreadID); !ok {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "thread %q not found", p.ThreadID)
		}
		// Watch before the turn starts, so no event of it is missed.
		c.watch(p.ThreadID)
		turn, err := threads.StartTurnWithModel(p.ThreadID, text, p.ModelProfileID)
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
		if _, ok := threads.ReadThread(p.ThreadID); !ok {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "thread %q not found", p.ThreadID)
		}
		turn, err := threads.SteerTurn(p.ThreadID, p.ExpectedTurnID, text)
		switch {
		case errors.Is(err, threadmanager.ErrNoActiveTurn), errors.Is(err, threadmanager.ErrTurnMismatch), errors.Is(err, threadmanager.ErrTurnNotSteerable):
			// As in Codex: the turn cannot take the input now, and the
			// client sends it as a new turn instead.
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
		view, ok := threads.ReadThread(p.ThreadID)
		if !ok {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "thread %q not found", p.ThreadID)
		}
		if turn, active := view.ActiveTurn(); !active || turn.ID != p.TurnID {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "turn %q is not in progress", p.TurnID)
		}
		if err := threads.InterruptTurn(p.ThreadID); err != nil {
			return nil, nil, serverError(err)
		}
		return struct{}{}, nil, nil

	case methodCronList:
		if err := jsonrpc.DecodeParams(params, &struct{}{}); err != nil {
			return nil, nil, err
		}
		return cronListResult{Data: c.s.cron.List()}, nil, nil

	case methodCronAdd:
		var p cronAddParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		var job cron.Job
		var err error
		if scoped, ok := c.s.cron.(interface {
			CreateInWorkspace(string, string, string, string) (cron.Job, error)
		}); ok {
			job, err = scoped.CreateInWorkspace(p.ID, p.Spec, p.Prompt, p.WorkspaceID)
		} else if p.WorkspaceID != "" {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "project scheduled jobs are unavailable")
		} else {
			job, err = c.s.cron.Create(p.ID, p.Spec, p.Prompt)
		}
		if err != nil {
			return nil, nil, serverError(err)
		}
		return job, nil, nil

	case methodCronRemove:
		var p cronJobParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		if err := c.s.cron.Delete(p.ID); err != nil {
			return nil, nil, serverError(err)
		}
		return struct{}{}, nil, nil

	case methodCronRun:
		var p cronJobParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		t, err := c.s.runJob(p.ID)
		if err != nil {
			return nil, nil, serverError(err)
		}
		c.watch(t.ID)
		return cronRunResult{Thread: t}, nil, nil

	case methodConfigRead:
		if err := jsonrpc.DecodeParams(params, &struct{}{}); err != nil {
			return nil, nil, err
		}
		content, err := c.s.config.Read()
		if err != nil {
			return nil, nil, serverError(err)
		}
		return configResult{Path: c.s.config.Path(), Content: string(content)}, nil, nil

	case methodConfigWrite:
		var p configWriteParams
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		if err := c.s.config.Write([]byte(p.Content)); err != nil {
			return nil, nil, serverError(err)
		}
		return struct{}{}, nil, nil

	default:
		return nil, nil, jsonrpc.Errorf(jsonrpc.CodeMethodNotFound, "method %q not found", method)
	}
}

// event forwards one thread event to this connection.
func (c *conn) event(e threadmanager.Event) {
	switch e.Kind {
	case threadmanager.EventThreadStarted:
		c.rpc.Notify(notifyThreadStarted, threadResult{Thread: e.Thread})
	case threadmanager.EventTurnStarted:
		c.rpc.Notify(notifyTurnStarted, turnNotification{ThreadID: e.Thread.ID, Turn: e.Turn})
	case threadmanager.EventItemStarted:
		c.rpc.Notify(notifyItemStarted, itemNotification{ThreadID: e.Thread.ID, TurnID: e.Turn.ID, Item: *e.Item})
	case threadmanager.EventItemCompleted:
		c.rpc.Notify(notifyItemCompleted, itemNotification{ThreadID: e.Thread.ID, TurnID: e.Turn.ID, Item: *e.Item})
	case threadmanager.EventItemDelta:
		method := notifyAgentMessageDelta
		if e.Item.Type == thread.ItemReasoning {
			method = notifyReasoningDelta
		}
		c.rpc.Notify(method, deltaNotification{ThreadID: e.Thread.ID, TurnID: e.Turn.ID, ItemID: e.Item.ID, Delta: e.Delta})
	case threadmanager.EventTurnCompleted:
		c.rpc.Notify(notifyTurnCompleted, turnNotification{ThreadID: e.Thread.ID, Turn: e.Turn})
	case threadmanager.EventApprovalRequested, threadmanager.EventUserInputRequested:
		c.request(e.Thread.ID, *e.Item)
	}
}

// request sends the server request for a pending item, unless this
// connection already has it unanswered. The response resumes the turn.
func (c *conn) request(threadID string, it thread.Item) {
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
	if it.Type == thread.ItemUserInputRequest {
		c.rpc.Request(requestUserInput, userInputRequestParams{ThreadID: threadID, TurnID: it.TurnID, ItemID: it.ID, Questions: it.Questions}, answered)
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

// resolve applies the client's response to a request about it.
func (c *conn) resolve(threadID string, it thread.Item, result json.RawMessage) error {
	if it.Type == thread.ItemUserInputRequest {
		var r userInputResponse
		if err := json.Unmarshal(result, &r); err != nil {
			return err
		}
		return c.s.threads.AnswerUserInput(threadID, it.ID, r.Answers)
	}
	var r approvalResponse
	if err := json.Unmarshal(result, &r); err != nil {
		return err
	}
	return c.s.threads.ResolveApproval(threadID, it.Approval.ID, r.Decision, "gateway")
}

// inputText joins the text elements of turn input.
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
