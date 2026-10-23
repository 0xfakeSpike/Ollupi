package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

func (a *Agent) StartTurn(threadID, text, connection string) (repo.TurnRow, error) {
	text, err := checkMessage(text)
	if err != nil {
		return repo.TurnRow{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	view, ok := a.ReadThread(threadID)
	if !ok {
		return repo.TurnRow{}, fmt.Errorf("agent: thread %s not found", threadID)
	}
	if view.Thread.ParentThreadID != "" {
		return repo.TurnRow{}, errSubagentThread
	}
	model, err := a.conversationModel(view, connection)
	if err != nil {
		return repo.TurnRow{}, err
	}
	return a.beginTurnLocked(view, repo.NewUserMessage(uuid.NewString(), "", text, time.Now().UTC()), model, a.threadPolicy(view.Thread))
}

func (a *Agent) beginTurnLocked(view repo.ThreadView, message repo.ItemRow, model llm.Model, policy permission.Policy) (repo.TurnRow, error) {
	threadID := view.Thread.ID
	if _, active := view.ActiveTurn(); active {
		return repo.TurnRow{}, errTurnActive
	}
	if view.Thread.ParentThreadID == "" {
		if err := a.checkWorkspace(view.Thread.WorkspaceID); err != nil {
			return repo.TurnRow{}, err
		}
	}
	turn := repo.TurnRow{
		ID: uuid.NewString(), ThreadID: threadID, Status: repo.TurnInProgress,
		Model: model.Clone(), ApprovalPolicy: string(policy), StartedAt: time.Now().UTC(),
	}
	message.TurnID = turn.ID
	turn, err := a.insertTurn(threadID, turn, []repo.ItemRow{message})
	if err != nil {
		return repo.TurnRow{}, err
	}
	a.emit(Event{Kind: EventTurnStarted, Thread: view.Thread, Turn: turn})
	a.emitItem(view.Thread, turn, message, true)
	a.enqueueLocked(threadID)
	a.emitStatus(threadID)
	return turn, nil
}

func (a *Agent) SteerTurn(threadID, expectedTurnID, text string) (repo.TurnRow, error) {
	text, err := checkMessage(text)
	if err != nil {
		return repo.TurnRow{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	view, ok := a.ReadThread(threadID)
	if !ok {
		return repo.TurnRow{}, fmt.Errorf("agent: thread %s not found", threadID)
	}
	if view.Thread.ParentThreadID != "" {
		return repo.TurnRow{}, errSubagentThread
	}
	turn, active := view.ActiveTurn()
	if !active {
		return repo.TurnRow{}, ErrNoActiveTurn
	}
	if turn.ID != expectedTurnID {
		return repo.TurnRow{}, fmt.Errorf("%w (expected %s, in progress %s)", ErrTurnMismatch, expectedTurnID, turn.ID)
	}
	stored, isNew, err := a.recordItem(threadID, repo.NewUserMessage(uuid.NewString(), turn.ID, text, time.Now().UTC()))
	if err != nil {
		return repo.TurnRow{}, err
	}
	a.emitItem(view.Thread, turn, stored, isNew)
	if sl, ok := a.slots[threadID]; ok && !a.queuedLocked(threadID) {
		sl.steered = true
	}
	return turn, nil
}

func (a *Agent) InterruptTurn(threadID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if sl, ok := a.slots[threadID]; ok {
		a.stopLocked(threadID, sl)
		return nil
	}
	view, ok := a.ReadThread(threadID)
	if !ok {
		return fmt.Errorf("agent: thread %s not found", threadID)
	}
	turn, active := view.ActiveTurn()
	if !active {
		return ErrNoActiveTurn
	}
	calls := view.AgentCalls()
	turn.Status = repo.TurnInterrupted
	turn.Error = "interrupted while waiting for the user"
	if len(calls) > 0 {
		turn.Error = "interrupted while waiting for subagents"
	}
	a.endLocked(turn)
	for _, call := range calls {
		a.stopThreadLocked(call.Await.ThreadID)
	}
	return nil
}

func (a *Agent) stopThreadLocked(threadID string) {
	if sl, ok := a.slots[threadID]; ok {
		a.stopLocked(threadID, sl)
		return
	}
	view, ok := a.ReadThread(threadID)
	if !ok {
		return
	}
	if turn, active := view.ActiveTurn(); active {
		turn.Status = repo.TurnInterrupted
		turn.Error = "stopped with the turn that launched it"
		a.endLocked(turn)
	}
}

func checkMessage(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("agent: empty message")
	}
	if n := len([]rune(text)); n > maxInputRunes {
		return "", fmt.Errorf("agent: message has %d characters; at most %d are accepted", n, maxInputRunes)
	}
	return text, nil
}
