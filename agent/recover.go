package agent

import (
	"fmt"
	"log"
	"time"

	"github.com/0xfakeSpike/ollupi/repo"
)

func (a *Agent) recoverLocked() error {
	active, err := a.DB.ActiveThreadIDs()
	if err != nil {
		return fmt.Errorf("agent: active threads: %w", err)
	}
	for _, id := range active {
		closed, err := a.recoverItems(id)
		if err != nil {
			return fmt.Errorf("agent: recover thread %s: %w", id, err)
		}
		if len(closed) > 0 {
			log.Printf("agent: thread %s: closed %d items cut off by the restart", id, len(closed))
		}
	}
	for _, id := range active {
		view, _ := a.ReadThread(id)
		if turn, running := view.ActiveTurn(); running {
			_, err := a.modelOn(turn.Model.Connection)
			if err != nil {
				turn.Status, turn.Error = repo.TurnFailed, err.Error()
				a.endLocked(turn)
				for _, call := range view.AgentCalls() {
					a.stopThreadLocked(call.Await.ThreadID)
				}
				continue
			}
		}
		for _, call := range view.AgentCalls() {
			if settled, ok := a.settledCall(call); ok {
				if _, _, err := a.recordItem(id, settled); err != nil {
					return fmt.Errorf("agent: thread %s: settle Agent call %s: %w", id, call.ID, err)
				}
			}
		}
		a.launchSubagentsLocked(id)
	}
	for _, id := range active {
		a.resumeIfReadyLocked(id)
	}
	return nil
}

func (a *Agent) settledCall(call repo.ItemRow) (repo.ItemRow, bool) {
	if call.Await.ThreadID == "" {
		return repo.ItemRow{}, false
	}
	child, ok := a.ReadThread(call.Await.ThreadID)
	if !ok {
		now := time.Now().UTC()
		call.Status, call.Error, call.CompletedAt = repo.ItemFailed, "the subagent's thread is missing", &now
		return call, true
	}
	if _, active := child.ActiveTurn(); active || len(child.Turns) == 0 {
		return repo.ItemRow{}, false
	}
	return report(call, child, child.Turns[len(child.Turns)-1]), true
}
