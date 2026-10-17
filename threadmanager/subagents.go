package threadmanager

import (
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/0xfakeSpike/ollupi/agent"
	"github.com/0xfakeSpike/ollupi/thread"
)

// Subagents (Claude Code's foreground subagents). The parent's Agent call is
// a collabAgentToolCall item that the Runner accepts and leaves in progress;
// the parent turn then pauses. The manager launches one subagent thread per
// accepted call, whose one turn does the task. When that turn ends, the same
// write completes the call with the subagent's final report (endLocked), and
// the parent turn is scheduled again once every call has its report. Only
// the parent writes its own items, except that one report; a subagent never
// learns of its parent.

// parentTurn is the thread and turn in progress of an Agent call.
type parentTurn struct {
	Thread thread.Thread
	Turn   thread.Turn
}

// launchSubagentsLocked launches a subagent thread for every accepted Agent
// call of the thread's turn that has none yet. A call whose subagent cannot
// be launched fails.
func (m *Manager) launchSubagentsLocked(threadID string) {
	view, _ := m.threads.Read(threadID)
	turn, _ := view.ActiveTurn()
	for _, call := range view.AgentCalls() {
		if len(call.ReceiverThreadIDs) > 0 {
			continue
		}
		if child, err := m.launchSubagentLocked(view.Thread, call); err != nil {
			now := time.Now().UTC()
			call.Status, call.Error, call.CompletedAt = thread.ItemFailed, fmt.Sprintf("the subagent could not be launched: %v", err), &now
		} else {
			call.ReceiverThreadIDs = []string{child.ID}
		}
		stored, _, err := m.threads.Record(threadID, call)
		if err != nil {
			log.Printf("threadmanager: thread %s: record Agent call %s: %v", threadID, call.ID, err)
			continue
		}
		m.emitItem(view.Thread, turn, stored, false)
	}
}

// launchSubagentLocked starts the subagent thread for one Agent call, with
// the call's prompt as the user message of its turn.
func (m *Manager) launchSubagentLocked(parent thread.Thread, call thread.Item) (thread.Thread, error) {
	args, err := agent.ParseAgentCall(call.Arguments, m.agent.Subagents)
	if err != nil {
		return thread.Thread{}, err
	}
	child, err := m.threads.Create(thread.Thread{
		Source: thread.SourceSubagent, ParentThreadID: parent.ID, ParentItemID: call.ID, Agent: call.Agent,
		ApprovalPolicy: parent.ApprovalPolicy, WorkspaceID: parent.WorkspaceID,
	})
	if err != nil {
		return thread.Thread{}, err
	}
	m.emit(Event{Kind: EventThreadStarted, Thread: child})
	view, _ := m.threads.Read(child.ID)
	if _, err := m.beginTurnLocked(view, args.Prompt); err != nil {
		return thread.Thread{}, err
	}
	return child, nil
}

// reportLocked is the completed Agent call that the end of a subagent's
// turn makes, when the parent turn still waits for it.
func (m *Manager) reportLocked(child thread.View, ended thread.Turn) (thread.Item, parentTurn, bool) {
	if child.Thread.ParentThreadID == "" {
		return thread.Item{}, parentTurn{}, false
	}
	parent, ok := m.threads.Read(child.Thread.ParentThreadID)
	if !ok {
		return thread.Item{}, parentTurn{}, false
	}
	turn, active := parent.ActiveTurn()
	if !active {
		return thread.Item{}, parentTurn{}, false
	}
	for _, call := range parent.AgentCalls() {
		if call.ID == child.Thread.ParentItemID {
			return report(call, child, ended), parentTurn{Thread: parent.Thread, Turn: turn}, true
		}
	}
	return thread.Item{}, parentTurn{}, false
}

// report completes an Agent call with how its subagent's turn ended.
func report(call thread.Item, child thread.View, ended thread.Turn) thread.Item {
	now := time.Now().UTC()
	call.CompletedAt = &now
	switch ended.Status {
	case thread.TurnCompleted:
		if answer, ok := child.FinalAnswer(ended.ID); ok {
			call.Status, call.Text = thread.ItemCompleted, answer
			return call
		}
		call.Status, call.Error = thread.ItemFailed, "the subagent finished without a report"
	case thread.TurnInterrupted:
		call.Status, call.Error = thread.ItemFailed, "the subagent was stopped: "+ended.Error
	default:
		call.Status, call.Error = thread.ItemFailed, "the subagent failed: "+ended.Error
	}
	return call
}

// recoverLocked runs again every turn that was in progress when the
// process stopped, subagents' as well as their parents': it closes what each
// lost (thread.Store.Recover), launches the subagents of Agent calls that
// were accepted but had none yet, fails calls whose subagent thread is gone,
// and schedules, oldest first, every turn that waits for nothing. Turns
// waiting for the user keep waiting; turns waiting for subagents go on when
// their subagents report.
func (m *Manager) recoverLocked() error {
	active := m.threads.ActiveThreads()
	for _, id := range active {
		closed, err := m.threads.Recover(id)
		if err != nil {
			return fmt.Errorf("threadmanager: recover thread %s: %w", id, err)
		}
		if len(closed) > 0 {
			log.Printf("threadmanager: thread %s: closed %d items cut off by the restart", id, len(closed))
		}
	}
	for _, id := range active {
		view, _ := m.threads.Read(id)
		if turn, running := view.ActiveTurn(); running {
			_, err := m.agentForTurn(view.Thread, turn)
			failureStatus := thread.TurnFailed
			if err == nil {
				err = m.validateExecutionContext(turn.ExecutionContext)
				failureStatus = thread.TurnInterrupted
			}
			if err != nil {
				// A removed connection must visibly fail even if this turn
				// was waiting for approval. Never silently use the default.
				turn.Status, turn.Error = failureStatus, err.Error()
				m.endLocked(turn)
				for _, call := range view.AgentCalls() {
					for _, child := range call.ReceiverThreadIDs {
						m.stopThreadLocked(child)
					}
				}
				continue
			}
		}
		for _, call := range view.AgentCalls() {
			if settled, ok := m.settledCall(call); ok {
				if _, _, err := m.threads.Record(id, settled); err != nil {
					return fmt.Errorf("threadmanager: thread %s: settle Agent call %s: %w", id, call.ID, err)
				}
			}
		}
		m.launchSubagentsLocked(id)
	}
	for _, id := range active {
		m.resumeIfReadyLocked(id)
	}
	return nil
}

// settledCall is an Agent call completed from its subagent thread when that
// thread can no longer report by itself: it is gone, or its turn ended
// without the report reaching the call. Calls not launched yet and
// subagents still in progress are left as they are.
func (m *Manager) settledCall(call thread.Item) (thread.Item, bool) {
	if len(call.ReceiverThreadIDs) == 0 {
		return thread.Item{}, false
	}
	child, ok := m.threads.Read(call.ReceiverThreadIDs[0])
	if !ok {
		now := time.Now().UTC()
		call.Status, call.Error, call.CompletedAt = thread.ItemFailed, "the subagent's thread is missing", &now
		return call, true
	}
	if _, active := child.ActiveTurn(); active || len(child.Turns) == 0 {
		return thread.Item{}, false
	}
	return report(call, child, child.Turns[len(child.Turns)-1]), true
}

// Request is a request waiting for the user: an approval or questions, in
// the thread or in a subagent thread its turn waits for.
type Request struct {
	Thread thread.Thread
	Item   thread.Item
}

// PendingRequests returns the requests waiting for the user in a
// conversation: the thread's own and its subagents', oldest first. The user
// answers them one at a time, in any order.
func (m *Manager) PendingRequests(threadID string) []Request {
	view, ok := m.threads.Read(threadID)
	if !ok {
		return nil
	}
	var out []Request
	if it, ok := view.PendingRequest(); ok {
		out = append(out, Request{Thread: view.Thread, Item: it})
	}
	for _, call := range view.AgentCalls() {
		for _, id := range call.ReceiverThreadIDs {
			if child, ok := m.threads.Read(id); ok {
				if it, ok := child.PendingRequest(); ok {
					out = append(out, Request{Thread: child.Thread, Item: it})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return requestedAt(out[i].Item).Before(requestedAt(out[j].Item)) })
	return out
}

func requestedAt(it thread.Item) time.Time {
	if it.Approval != nil {
		return it.Approval.RequestedAt
	}
	return it.StartedAt
}
