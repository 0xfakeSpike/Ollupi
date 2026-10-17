// Package threadmanager manages threads and runs their turns (Codex's
// ThreadManager). Every client (gateway connections, cron runs) works
// on threads; a thread has at most one turn in progress.
//
// The manager is also the scheduler of turns (scheduler.go): a turn that can
// run waits in one queue, and a bounded set of workers runs the agent on the
// oldest. A turn runs until it ends or needs the user: a tool call to
// approve, or questions to answer. Then the request is recorded on its item,
// subscribers are notified, the worker is released, and nothing waits in
// memory. The user's response is recorded on the item and the turn is queued
// again, to run from where it stopped, even after a restart.
package threadmanager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/0xfakeSpike/ollupi/agent"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/google/uuid"
)

const (
	// maxConcurrentTurns bounds how many turns run at once.
	maxConcurrentTurns = 4
	// MaxInputRunes bounds one user message or answer.
	MaxInputRunes = 4000
)

// ErrJobBusy is returned when a cron job fires while the turn of its
// previous run is still in progress; the firing is skipped.
var ErrJobBusy = errors.New("the previous run of this cron job is still in progress")

// Runner runs one turn of an agent. Implemented by agent.Runner.
type Runner interface {
	Run(ctx context.Context, a agent.Agent, turn agent.Turn, recorder agent.Recorder) (agent.Result, error)
}

// Manager owns every thread and runs their turns.
type Manager struct {
	runner     Runner
	agent      agent.Agent
	profiles   map[string]ModelProfile
	modelOrder []string
	options    Options
	recovering bool
	// subagents are the agents the main agent may launch, by name.
	subagents map[string]agent.Agent
	threads   *thread.Store
	ctx       context.Context

	// mu guards the scheduler state below and orders every change of a
	// turn's state (start, pause, resume, end).
	mu sync.Mutex
	// executions holds each thread whose turn is queued or running (not
	// waiting for the user).
	executions map[string]*execution
	// runnable lists the threads whose execution waits for a worker, oldest
	// first.
	runnable []string
	// workers counts the running executions.
	workers int

	eventsMu    sync.Mutex
	subscribers []func(Event)
	queue       []Event
	delivering  bool

	background sync.WaitGroup
}

// New builds the manager. ctx bounds every turn; cancelling it stops
// background work at shutdown and leaves the turns in progress for the next
// start. subagents are the agents ag.Subagents names. Every turn that was in
// progress when the process stopped is recovered and runs again (see
// recoverLocked).
func New(ctx context.Context, runner Runner, ag agent.Agent, subagents []agent.Agent, store thread.Persistence, profiles ...ModelProfile) (*Manager, error) {
	return NewWithOptions(ctx, runner, ag, subagents, store, Options{}, profiles...)
}

func NewWithOptions(ctx context.Context, runner Runner, ag agent.Agent, subagents []agent.Agent, store thread.Persistence, options Options, profiles ...ModelProfile) (*Manager, error) {
	byName := make(map[string]agent.Agent, len(subagents))
	for _, s := range subagents {
		byName[s.Name] = s
	}
	for _, s := range ag.Subagents {
		if _, ok := byName[s.Name]; !ok {
			return nil, fmt.Errorf("threadmanager: subagent %q has no definition", s.Name)
		}
	}
	threads, err := thread.Open(store)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		runner: runner, agent: ag, subagents: byName, threads: threads, ctx: ctx,
		executions: make(map[string]*execution), options: options, recovering: true,
	}
	if err := m.configureModels(profiles); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recoverLocked(); err != nil {
		return nil, err
	}
	m.recovering = false
	for _, id := range m.threads.ActiveThreads() {
		m.resumeIfReadyLocked(id)
	}
	m.dispatchLocked()
	return m, nil
}

// Wait blocks until every background goroutine has returned.
func (m *Manager) Wait() {
	m.background.Wait()
}

// ---------------------------------------------------------------------------
// Events

func (m *Manager) Subscribe(fn func(Event)) {
	m.eventsMu.Lock()
	defer m.eventsMu.Unlock()
	m.subscribers = append(m.subscribers, fn)
}

// emit queues e. Events are delivered one at a time, in order.
func (m *Manager) emit(e Event) {
	m.eventsMu.Lock()
	defer m.eventsMu.Unlock()
	m.queue = append(m.queue, e)
	if m.delivering {
		return
	}
	m.delivering = true
	m.background.Add(1)
	go m.deliver()
}

func (m *Manager) deliver() {
	defer m.background.Done()
	for {
		m.eventsMu.Lock()
		if len(m.queue) == 0 {
			m.delivering = false
			m.eventsMu.Unlock()
			return
		}
		e := m.queue[0]
		m.queue = m.queue[1:]
		subscribers := append([]func(Event){}, m.subscribers...)
		m.eventsMu.Unlock()
		for _, fn := range subscribers {
			fn(e)
		}
	}
}

// emitStatus announces a thread's current status. A subagent thread is not
// listed, so its parent's status is announced instead.
func (m *Manager) emitStatus(threadID string) {
	t, ok := m.threads.Get(threadID)
	if !ok {
		panic(fmt.Sprintf("threadmanager: thread %s vanished", threadID))
	}
	if t.ParentThreadID != "" {
		m.emitStatus(t.ParentThreadID)
		return
	}
	status, _ := m.threads.Status(threadID)
	m.emit(Event{Kind: EventThreadStatusChanged, Thread: t, Status: status})
}

// emitRequest announces the request a paused thread waits on.
func (m *Manager) emitRequest(view thread.View) {
	it, ok := view.PendingRequest()
	if !ok {
		return
	}
	turn, _ := view.ActiveTurn()
	kind := EventApprovalRequested
	if it.Type == thread.ItemUserInputRequest {
		kind = EventUserInputRequested
	}
	m.emit(Event{Kind: kind, Thread: view.Thread, Turn: turn, Item: &it})
}

// ---------------------------------------------------------------------------
// Threads

func (m *Manager) StartThread(p StartThreadParams) (thread.Thread, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.requireActiveWorkspace(p.WorkspaceID); err != nil {
		return thread.Thread{}, err
	}
	if p.Source != thread.SourceGateway && p.Source != thread.SourceCron {
		return thread.Thread{}, fmt.Errorf("threadmanager: unknown thread source %q", p.Source)
	}
	if p.ApprovalPolicy != "" {
		if _, err := agent.ParseApprovalPolicy(p.ApprovalPolicy); err != nil {
			return thread.Thread{}, fmt.Errorf("threadmanager: %w", err)
		}
	}
	return m.threads.Create(thread.Thread{Source: p.Source, ApprovalPolicy: p.ApprovalPolicy, WorkspaceID: p.WorkspaceID})
}

func (m *Manager) ListThreads(f thread.Filter) []thread.Summary {
	return m.threads.List(f)
}

func (m *Manager) ReadThread(threadID string) (thread.View, bool) {
	return m.threads.Read(threadID)
}

// ---------------------------------------------------------------------------
// Turns

func (m *Manager) StartTurn(threadID, text string) (thread.Turn, error) {
	return m.StartTurnWithModel(threadID, text, "")
}

// StartTurnWithModel selects a configured connection for a new turn. Empty
// selection keeps this conversation's last profile, or the default for a new
// conversation. It never changes a turn already in progress.
func (m *Manager) StartTurnWithModel(threadID, text, modelProfileID string) (thread.Turn, error) {
	text, err := checkMessage(text)
	if err != nil {
		return thread.Turn{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	view, ok := m.threads.Read(threadID)
	if !ok {
		return thread.Turn{}, fmt.Errorf("threadmanager: thread %s not found", threadID)
	}
	if view.Thread.Source == thread.SourceSubagent {
		return thread.Turn{}, ErrSubagentThread
	}
	return m.beginTurnLocked(view, text, modelProfileID)
}

// beginTurnLocked starts a turn of the thread with the user's message and
// schedules it.
func (m *Manager) beginTurnLocked(view thread.View, text string, modelProfileID ...string) (thread.Turn, error) {
	threadID := view.Thread.ID
	if _, active := view.ActiveTurn(); active {
		return thread.Turn{}, ErrTurnActive
	}
	selected := ""
	if len(modelProfileID) > 0 {
		selected = modelProfileID[0]
	}
	ag, err := m.agentForNewTurn(view, selected)
	if err != nil {
		return thread.Turn{}, err
	}
	snapshot, err := m.newExecutionContext(view)
	if err != nil {
		return thread.Turn{}, err
	}
	now := time.Now().UTC()
	turn := thread.Turn{
		ID: uuid.NewString(), ThreadID: threadID, Status: thread.TurnInProgress,
		Agent: ag.Name, Model: ag.Model, ApprovalPolicy: string(ag.ApprovalPolicy), StartedAt: now,
		ExecutionContext: snapshot, ModelProfileID: ag.ModelProfileID, ModelSettings: thread.ModelSettings{EnableThinking: copyBool(ag.ModelSettings.EnableThinking)},
	}
	message := thread.NewUserMessage(uuid.NewString(), turn.ID, text, now)
	turn, err = m.threads.BeginTurn(threadID, turn, []thread.Item{message})
	if err != nil {
		return thread.Turn{}, err
	}
	m.emit(Event{Kind: EventTurnStarted, Thread: view.Thread, Turn: turn})
	m.emitItem(view.Thread, turn, message, true)
	m.scheduleLocked(threadID, turn.ID)
	return turn, nil
}

// SubmitCronRun starts a new thread for one run of a cron job.
func (m *Manager) SubmitCronRun(jobID, prompt string, workspaceIDs ...string) (thread.Thread, error) {
	prompt, err := checkMessage(prompt)
	if err != nil {
		return thread.Thread{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	workspaceID := ""
	if len(workspaceIDs) > 0 {
		workspaceID = workspaceIDs[0]
	}
	if err := m.requireActiveWorkspace(workspaceID); err != nil {
		return thread.Thread{}, err
	}
	for _, previous := range m.threads.List(thread.Filter{CronJobID: jobID, Limit: 1}) {
		if previous.Status != thread.StatusIdle {
			return thread.Thread{}, fmt.Errorf("%w (job %s, thread %s)", ErrJobBusy, jobID, previous.ID)
		}
	}
	t, err := m.threads.Create(thread.Thread{Source: thread.SourceCron, CronJobID: jobID, WorkspaceID: workspaceID})
	if err != nil {
		return thread.Thread{}, err
	}
	view, _ := m.threads.Read(t.ID)
	if _, err := m.beginTurnLocked(view, prompt); err != nil {
		return thread.Thread{}, err
	}
	return t, nil
}

func (m *Manager) InterruptTurn(threadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ex, ok := m.executions[threadID]; ok {
		m.stopLocked(threadID, ex)
		return nil
	}
	view, ok := m.threads.Read(threadID)
	if !ok {
		return fmt.Errorf("threadmanager: thread %s not found", threadID)
	}
	turn, active := view.ActiveTurn()
	if !active {
		return ErrNoActiveTurn
	}
	// The turn waits for the user or its subagents. Ending it closes the
	// open request and fails its Agent calls; then its subagents are stopped
	// (they belong to the turn), and their ends no longer reach it.
	calls := view.AgentCalls()
	turn.Status = thread.TurnInterrupted
	turn.Error = "interrupted while waiting for the user"
	if len(calls) > 0 {
		turn.Error = "interrupted while waiting for subagents"
	}
	m.endLocked(turn)
	for _, call := range calls {
		for _, child := range call.ReceiverThreadIDs {
			m.stopThreadLocked(child)
		}
	}
	return nil
}

// stopThreadLocked interrupts the thread's turn in progress, if any.
func (m *Manager) stopThreadLocked(threadID string) {
	if ex, ok := m.executions[threadID]; ok {
		m.stopLocked(threadID, ex)
		return
	}
	view, ok := m.threads.Read(threadID)
	if !ok {
		return
	}
	if turn, active := view.ActiveTurn(); active {
		turn.Status = thread.TurnInterrupted
		turn.Error = "stopped with the turn that launched it"
		m.endLocked(turn)
	}
}

// ---------------------------------------------------------------------------
// Responses to requests

func (m *Manager) ResolveApproval(threadID, approvalID string, decision thread.Decision, decidedBy string) error {
	return m.respond(threadID, func(it thread.Item) (thread.Item, error) {
		if it.Type != thread.ItemMcpToolCall || it.Approval.ID != approvalID {
			return thread.Item{}, ErrNoSuchRequest
		}
		if available := it.Approval.AvailableDecisions(); !slices.Contains(available, decision) {
			return thread.Item{}, fmt.Errorf("threadmanager: decision %q is not one of %q", decision, available)
		}
		now := time.Now().UTC()
		it.Approval.Decision, it.Approval.DecidedBy, it.Approval.DecidedAt = decision, decidedBy, &now
		return it, nil
	})
}

func (m *Manager) AnswerUserInput(threadID, itemID string, answers map[string]thread.UserInputAnswer) error {
	return m.respond(threadID, func(it thread.Item) (thread.Item, error) {
		if it.Type != thread.ItemUserInputRequest || it.ID != itemID {
			return thread.Item{}, ErrNoSuchRequest
		}
		if err := checkAnswers(it.Questions, answers); err != nil {
			return thread.Item{}, err
		}
		now := time.Now().UTC()
		it.Answers, it.Status, it.CompletedAt = answers, thread.ItemCompleted, &now
		return it, nil
	})
}

// respond records the user's response on the request the thread waits on
// and resumes the turn.
func (m *Manager) respond(threadID string, apply func(thread.Item) (thread.Item, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	view, ok := m.threads.Read(threadID)
	if !ok {
		return fmt.Errorf("threadmanager: thread %s not found", threadID)
	}
	pending, ok := view.PendingRequest()
	if _, scheduled := m.executions[threadID]; scheduled || !ok {
		return ErrNoSuchRequest
	}
	activeTurn, _ := view.ActiveTurn()
	if err := m.validateExecutionContext(activeTurn.ExecutionContext); err != nil {
		activeTurn.Status, activeTurn.Error = thread.TurnInterrupted, err.Error()
		m.endLocked(activeTurn)
		return err
	}
	answered, err := apply(pending)
	if err != nil {
		return err
	}
	stored, _, err := m.threads.Record(threadID, answered)
	if err != nil {
		return err
	}
	turn, _ := view.ActiveTurn()
	m.emitItem(view.Thread, turn, stored, false)
	if !m.resumeIfReadyLocked(threadID) {
		// It still waits for subagents.
		m.emitStatus(threadID)
	}
	return nil
}

// checkAnswers requires a non-blank answer to every question and nothing
// else.
func checkAnswers(questions []thread.UserInputQuestion, answers map[string]thread.UserInputAnswer) error {
	total := 0
	for _, q := range questions {
		answer, ok := answers[q.ID]
		if !ok || len(answer.Answers) == 0 {
			return fmt.Errorf("threadmanager: question %q has no answer", q.ID)
		}
		for _, text := range answer.Answers {
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("threadmanager: question %q has a blank answer", q.ID)
			}
			total += len([]rune(text))
		}
	}
	if len(answers) != len(questions) {
		return fmt.Errorf("threadmanager: answers name a question that was not asked")
	}
	if total > MaxInputRunes {
		return fmt.Errorf("threadmanager: answers have %d characters; at most %d are accepted", total, MaxInputRunes)
	}
	return nil
}

// endLocked records how a turn ended and notifies subscribers. The end of
// a subagent's turn is also its report: in the same write, it completes the
// Agent call that launched it, and the parent turn goes on once it has every
// report it waits for.
func (m *Manager) endLocked(turn thread.Turn) {
	view, _ := m.threads.Read(turn.ThreadID)
	var closed []thread.Item
	var err error
	report, parent, reports := m.reportLocked(view, turn)
	if reports {
		var stored thread.Item
		closed, stored, err = m.threads.CompleteSubagentTurn(turn.ThreadID, turn, report)
		if err == nil {
			m.emitItem(parent.Thread, parent.Turn, stored, false)
		}
	} else {
		closed, err = m.threads.CompleteTurn(turn.ThreadID, turn)
	}
	if err != nil {
		log.Printf("threadmanager: thread %s: record end of turn %s: %v", turn.ThreadID, turn.ID, err)
	}
	view, _ = m.threads.Read(turn.ThreadID)
	for _, it := range closed {
		m.emitItem(view.Thread, turn, it, false)
	}
	ended, _ := findTurn(view, turn.ID)
	m.emit(Event{Kind: EventTurnCompleted, Thread: view.Thread, Turn: ended})
	m.emitStatus(turn.ThreadID)
	if reports && err == nil {
		m.resumeIfReadyLocked(parent.Thread.ID)
	}
}

// emitItem announces an item that was appended (isNew) or changed. An item
// is started once and completed once it reaches a final status.
func (m *Manager) emitItem(t thread.Thread, turn thread.Turn, it thread.Item, isNew bool) {
	if isNew {
		started := it
		m.emit(Event{Kind: EventItemStarted, Thread: t, Turn: turn, Item: &started})
	}
	if it.Status != thread.ItemInProgress {
		m.emit(Event{Kind: EventItemCompleted, Thread: t, Turn: turn, Item: &it})
	}
}

// agentFor is the agent definition a thread's turns run with: the main
// agent, or the subagent the thread was launched with.
func (m *Manager) agentFor(t thread.Thread) (agent.Agent, error) {
	ag := m.agent
	if t.Agent != "" {
		sub, ok := m.subagents[t.Agent]
		if !ok {
			return agent.Agent{}, fmt.Errorf("threadmanager: thread %s runs subagent %q, which is no longer defined", t.ID, t.Agent)
		}
		ag = sub
	}
	if t.ApprovalPolicy != "" {
		ag.ApprovalPolicy = agent.ApprovalPolicy(t.ApprovalPolicy)
	}
	return ag, nil
}

// recorder persists the Runner's items into the thread store and announces
// them.
type recorder struct {
	m      *Manager
	thread thread.Thread
	turn   thread.Turn
}

func (r recorder) Record(it thread.Item) (thread.Item, error) {
	stored, isNew, err := r.m.threads.Record(r.thread.ID, it)
	if err != nil {
		return thread.Item{}, err
	}
	r.m.emitItem(r.thread, r.turn, stored, isNew)
	return stored, nil
}

func (r recorder) Delta(it thread.Item, text string) {
	r.m.emit(Event{Kind: EventItemDelta, Thread: r.thread, Turn: r.turn, Item: &it, Delta: text})
}

func findTurn(view thread.View, turnID string) (thread.Turn, bool) {
	for _, t := range view.Turns {
		if t.ID == turnID {
			return t, true
		}
	}
	return thread.Turn{}, false
}
