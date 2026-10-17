package threadmanager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/0xfakeSpike/ollupi/agent"
	execcontext "github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/google/uuid"
)

// At shutdown the manager's context is cancelled: running turns stop where
// they are and stay in progress, queued ones are not started, and the next
// start recovers them all.
//
// The scheduler runs the turns of every thread on at most
// maxConcurrentTurns workers. A turn is scheduled when it starts and each
// time what it waited on is there: the user's response to its request, or
// the reports of all the subagents it launched. It then waits in one queue,
// oldest first, whatever thread it belongs to. A free worker takes the
// oldest, runs the agent until the turn ends or pauses, and takes the next.
// A paused turn holds no worker, so turns waiting on each other cannot
// starve the workers. Every decision to run something is made in
// dispatchLocked.

// execution is one scheduled run of a thread's turn: from its start or
// resumption until it ends or waits for the user. It is queued until a
// worker takes it.
type execution struct {
	turnID string
	ctx    context.Context
	cancel context.CancelFunc
	// input holds the messages sent with SteerTurn, queued or running.
	input *pendingInput
	// running is set when a worker takes the execution.
	running bool
}

// scheduleLocked queues the thread's turn in progress to run. The caller
// holds m.mu and has checked that the thread has no execution.
func (m *Manager) scheduleLocked(threadID, turnID string) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.executions[threadID] = &execution{turnID: turnID, ctx: ctx, cancel: cancel, input: &pendingInput{}}
	m.runnable = append(m.runnable, threadID)
	m.emitStatus(threadID)
	m.dispatchLocked()
}

// dispatchLocked hands queued executions to free workers, oldest first.
// Nothing new starts once the manager's context is cancelled at shutdown;
// turns still queued then run after the next start.
func (m *Manager) dispatchLocked() {
	if m.recovering {
		return
	}
	for m.workers < maxConcurrentTurns && len(m.runnable) > 0 && m.ctx.Err() == nil {
		threadID := m.runnable[0]
		m.runnable = m.runnable[1:]
		ex := m.executions[threadID]
		ex.running = true
		m.workers++
		m.background.Add(1)
		go func() {
			defer m.background.Done()
			m.work(threadID, ex)
		}()
	}
}

// work runs one execution on a worker until the turn ends or pauses, then
// frees the worker.
func (m *Manager) work(threadID string, ex *execution) {
	view, _ := m.threads.Read(threadID)
	turn, _ := findTurn(view, ex.turnID)
	var result agent.Result
	ag, runErr := m.agentForTurn(view.Thread, turn)
	if runErr == nil {
		runErr = m.validateExecutionContext(turn.ExecutionContext)
	}
	if runErr == nil {
		ctx := execcontext.WithWorkspaceID(ex.ctx, view.Thread.WorkspaceID)
		result, runErr = m.runner.Run(ctx, ag, agent.Turn{ID: ex.turnID, Items: view.Items, Input: ex.input, ExecutionContext: execcontext.Clone(turn.ExecutionContext)}, recorder{m: m, thread: view.Thread, turn: turn})
	}
	if err := m.threads.AddUsage(threadID, ex.turnID, result.Usage); err != nil {
		log.Printf("threadmanager: thread %s: record usage of turn %s: %v", threadID, ex.turnID, err)
	}
	// Decide whether the turn was interrupted before its context is
	// released: cancelling it would make every failure look like one.
	interrupted := ex.ctx.Err() != nil
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workers--
	m.finishLocked(threadID, ex, result, runErr, interrupted)
	m.dispatchLocked()
}

// resumeIfReadyLocked schedules the thread's paused turn when nothing it
// waits for is missing: no request is open and every Agent call has its
// report. It reports whether it did.
func (m *Manager) resumeIfReadyLocked(threadID string) bool {
	if m.recovering {
		return false
	}
	if _, scheduled := m.executions[threadID]; scheduled {
		return false
	}
	view, ok := m.threads.Read(threadID)
	if !ok {
		return false
	}
	turn, active := view.ActiveTurn()
	if !active {
		return false
	}
	if _, waiting := view.PendingRequest(); waiting || len(view.AgentCalls()) > 0 {
		return false
	}
	m.scheduleLocked(threadID, turn.ID)
	return true
}

// stopLocked interrupts a scheduled execution. A running one is cancelled
// and ends on its worker; a queued one never ran and ends here.
func (m *Manager) stopLocked(threadID string, ex *execution) {
	ex.cancel()
	if ex.running {
		return
	}
	m.runnable = slices.DeleteFunc(m.runnable, func(id string) bool { return id == threadID })
	m.finishLocked(threadID, ex, agent.Result{}, ex.ctx.Err(), true)
}

// finishLocked records how an execution left its turn: paused on a
// request, or ended.
func (m *Manager) finishLocked(threadID string, ex *execution, result agent.Result, runErr error, interrupted bool) {
	ex.cancel()
	delete(m.executions, threadID)
	view, _ := m.threads.Read(threadID)
	turn, ok := findTurn(view, ex.turnID)
	if !ok {
		panic(fmt.Sprintf("threadmanager: turn %s vanished from thread %s", ex.turnID, threadID))
	}
	// Messages sent too late for the Runner (the turn paused, failed, or was
	// interrupted first) are still recorded in the turn: nothing the user
	// sent is dropped, and the next run or turn sees them.
	rec := recorder{m: m, thread: view.Thread, turn: turn}
	for _, text := range ex.input.close() {
		if _, err := rec.Record(thread.NewUserMessage(uuid.NewString(), ex.turnID, text, time.Now().UTC())); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("threadmanager: record a message sent during the turn: %w", err))
		}
	}
	switch {
	case runErr != nil && m.ctx.Err() != nil:
		// Shutdown, not the user, stopped the turn: it stays in progress
		// and runs again after the next start.
		log.Printf("threadmanager: thread %s turn %s left in progress at shutdown: %v", threadID, ex.turnID, runErr)
		return
	case runErr == nil && (result.Pending != nil || result.Waiting):
		m.launchSubagentsLocked(threadID)
		view, _ = m.threads.Read(threadID)
		m.emitRequest(view)
		m.emitStatus(threadID)
		// If no subagent could be launched, nothing is left to wait for.
		m.resumeIfReadyLocked(threadID)
		return
	case runErr == nil:
		turn.Status = thread.TurnCompleted
	case interrupted:
		turn.Status = thread.TurnInterrupted
		turn.Error = runErr.Error()
	default:
		turn.Status = thread.TurnFailed
		turn.Error = runErr.Error()
	}
	if runErr != nil {
		log.Printf("threadmanager: thread %s turn %s %s: %v", threadID, ex.turnID, turn.Status, runErr)
	}
	m.endLocked(turn)
}
