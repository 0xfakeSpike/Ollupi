package agent

import (
	"fmt"
	"log"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/agent/tools/subagent"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

type parentTurn struct {
	Thread repo.ThreadRow
	Turn   repo.TurnRow
}

func (a *Agent) launchSubagentsLocked(threadID string) {
	view, _ := a.ReadThread(threadID)
	turn, _ := view.ActiveTurn()
	for _, call := range view.AgentCalls() {
		if call.Await.ThreadID != "" {
			continue
		}
		if child, err := a.launchSubagentLocked(view.Thread, turn, call); err != nil {
			now := time.Now().UTC()
			call.Status, call.Error, call.CompletedAt = repo.ItemFailed, fmt.Sprintf("the subagent could not be launched: %v", err), &now
		} else {
			call.Await.ThreadID = child.ID
		}
		stored, _, err := a.recordItem(threadID, call)
		if err != nil {
			log.Printf("agent: thread %s: record Agent call %s: %v", threadID, call.ID, err)
			continue
		}
		a.emitItem(view.Thread, turn, stored, false)
	}
}

func (a *Agent) launchSubagentLocked(parent repo.ThreadRow, parentTurn repo.TurnRow, call repo.ItemRow) (repo.ThreadRow, error) {
	if parent.ParentThreadID != "" {
		return repo.ThreadRow{}, fmt.Errorf("a subagent cannot launch subagents; do the task yourself with your tools")
	}
	req, err := subagent.Decode(call.Await.Request)
	if err != nil {
		return repo.ThreadRow{}, err
	}
	child, err := a.createThread(repo.ThreadRow{
		ParentThreadID: parent.ID, ParentItemID: call.ID,
		ApprovalPolicy: parent.ApprovalPolicy, WorkspaceID: parent.WorkspaceID,
	})
	if err != nil {
		return repo.ThreadRow{}, err
	}
	a.emit(Event{Kind: EventThreadStarted, Thread: child})
	view, _ := a.ReadThread(child.ID)
	if _, err := a.beginTurnLocked(view, repo.NewUserMessage(uuid.NewString(), "", req.Prompt, time.Now().UTC()), parentTurn.Model, permission.Policy(parentTurn.ApprovalPolicy)); err != nil {
		return repo.ThreadRow{}, err
	}
	return child, nil
}

func (a *Agent) reportLocked(child repo.ThreadView, ended repo.TurnRow) (repo.ItemRow, parentTurn, bool) {
	if child.Thread.ParentThreadID == "" {
		return repo.ItemRow{}, parentTurn{}, false
	}
	parent, ok := a.ReadThread(child.Thread.ParentThreadID)
	if !ok {
		return repo.ItemRow{}, parentTurn{}, false
	}
	turn, active := parent.ActiveTurn()
	if !active {
		return repo.ItemRow{}, parentTurn{}, false
	}
	for _, call := range parent.AgentCalls() {
		if call.ID == child.Thread.ParentItemID {
			return report(call, child, ended), parentTurn{Thread: parent.Thread, Turn: turn}, true
		}
	}
	return repo.ItemRow{}, parentTurn{}, false
}

func report(call repo.ItemRow, child repo.ThreadView, ended repo.TurnRow) repo.ItemRow {
	now := time.Now().UTC()
	call.CompletedAt = &now
	switch ended.Status {
	case repo.TurnCompleted:
		if answer, ok := child.FinalAnswer(ended.ID); ok {
			result, _, err := repo.ResultOf(tools.Result{Output: answer})
			if err == nil {
				call.Status, call.Result = repo.ItemCompleted, result
				return call
			}
		}
		call.Status, call.Error = repo.ItemFailed, "the subagent finished without a report"
	case repo.TurnInterrupted:
		call.Status, call.Error = repo.ItemFailed, "the subagent was stopped: "+ended.Error
	default:
		call.Status, call.Error = repo.ItemFailed, "the subagent failed: "+ended.Error
	}
	return call
}
