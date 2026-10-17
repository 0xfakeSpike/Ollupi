package threadmanager

import (
	"errors"

	"github.com/0xfakeSpike/ollupi/thread"
)

var (
	// ErrTurnActive is returned when a turn is started on a thread whose
	// turn is still in progress (running or waiting for the user).
	ErrTurnActive = errors.New("the thread already has a turn in progress")
	// ErrNoActiveTurn is returned when there is no turn to interrupt or
	// steer.
	ErrNoActiveTurn = errors.New("the thread has no turn in progress")
	// ErrTurnMismatch is returned when a steer names a turn other than the
	// one in progress.
	ErrTurnMismatch = errors.New("the expected turn is not the turn in progress")
	// ErrTurnNotSteerable is returned when a steer reaches a turn that waits
	// for the user or is ending; the message must start the next turn.
	ErrTurnNotSteerable = errors.New("the turn in progress does not take messages now; start a new turn when it ends")
	// ErrNoSuchRequest is returned for a response to a request the thread
	// does not wait on.
	ErrNoSuchRequest = errors.New("the thread does not wait on this request")
	// ErrSubagentThread is returned for input sent to a subagent thread,
	// which takes its one task from the parent's Agent call.
	ErrSubagentThread = errors.New("a subagent thread takes no input; send it to its parent conversation")
)

// StartThreadParams configures a new thread.
type StartThreadParams struct {
	Source      thread.Source
	WorkspaceID string
	// ApprovalPolicy overrides the agent's approval policy; empty keeps it.
	ApprovalPolicy string
}

type EventKind string

const (
	// EventThreadStarted: Thread, a subagent thread, was started by its
	// parent's Agent call. Threads started by clients are not announced.
	EventThreadStarted EventKind = "thread_started"
	// EventTurnStarted: Turn started.
	EventTurnStarted EventKind = "turn_started"
	// EventItemStarted and EventItemCompleted: Item was appended to the
	// turn, or reached a final status. An item recorded already final gets
	// both.
	EventItemStarted   EventKind = "item_started"
	EventItemCompleted EventKind = "item_completed"
	// EventItemDelta: Delta is text streamed into Item, an in-progress
	// agentMessage or reasoning item.
	EventItemDelta EventKind = "item_delta"
	// EventApprovalRequested: Item is a tool call waiting for approval.
	EventApprovalRequested EventKind = "approval_requested"
	// EventUserInputRequested: Item holds questions waiting for answers.
	EventUserInputRequested EventKind = "user_input_requested"
	// EventTurnCompleted: Turn ended.
	EventTurnCompleted EventKind = "turn_completed"
	// EventThreadStatusChanged: Status is the thread's new status. It is
	// sent for every thread, so lists stay current.
	EventThreadStatusChanged EventKind = "thread_status_changed"
)

// Event is a notice about a thread, delivered to subscribers.
type Event struct {
	Kind   EventKind
	Thread thread.Thread
	Turn   thread.Turn
	Item   *thread.Item
	Delta  string
	Status thread.Status
}
