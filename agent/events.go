package agent

import (
	"fmt"

	"github.com/0xfakeSpike/ollupi/repo"
)

type EventKind string

const (
	EventThreadStarted       EventKind = "thread_started"
	EventTurnStarted         EventKind = "turn_started"
	EventItemStarted         EventKind = "item_started"
	EventItemCompleted       EventKind = "item_completed"
	EventItemDelta           EventKind = "item_delta"
	EventApprovalRequested   EventKind = "approval_requested"
	EventUserInputRequested  EventKind = "user_input_requested"
	EventTurnCompleted       EventKind = "turn_completed"
	EventThreadStatusChanged EventKind = "thread_status_changed"
)

type Event struct {
	Kind   EventKind
	Thread repo.ThreadRow
	Turn   repo.TurnRow
	Item   *repo.ItemRow
	Delta  string
	Status repo.ThreadStatus
}

func (a *Agent) emit(e Event) {
	if a.OnEvent != nil {
		a.OnEvent(e)
	}
}

func (a *Agent) emitStatus(threadID string) {
	t, err := a.DB.ThreadRow(threadID)
	if err != nil {
		panic(fmt.Sprintf("agent: thread %s vanished: %v", threadID, err))
	}
	if t.ParentThreadID != "" {
		a.emitStatus(t.ParentThreadID)
		return
	}
	status := a.threadStatus(threadID)
	a.emit(Event{Kind: EventThreadStatusChanged, Thread: t, Status: status})
}

func (a *Agent) emitRequests(view repo.ThreadView) {
	turn, _ := view.ActiveTurn()
	for _, it := range view.PendingRequests() {
		kind := EventApprovalRequested
		if !it.WaitsForApproval() {
			kind = EventUserInputRequested
		}
		a.emit(Event{Kind: kind, Thread: view.Thread, Turn: turn, Item: &it})
	}
}

func (a *Agent) emitItem(t repo.ThreadRow, turn repo.TurnRow, it repo.ItemRow, isNew bool) {
	if isNew {
		started := it
		a.emit(Event{Kind: EventItemStarted, Thread: t, Turn: turn, Item: &started})
	}
	if it.Status != repo.ItemInProgress {
		a.emit(Event{Kind: EventItemCompleted, Thread: t, Turn: turn, Item: &it})
	}
}
