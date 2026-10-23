package agent

import (
	"context"
	"log"
	"slices"

	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/agent/runtime"
	"github.com/0xfakeSpike/ollupi/agent/systemprompt"
	"github.com/0xfakeSpike/ollupi/repo"
)

func (a *Agent) enqueueLocked(threadID string) {
	a.slots[threadID] = &slot{}
	a.queue = append(a.queue, threadID)
	a.dispatchLocked()
}

func (a *Agent) queuedLocked(threadID string) bool {
	return slices.Contains(a.queue, threadID)
}

func (a *Agent) dispatchLocked() {
	if a.recovering {
		return
	}
	for a.busy < workers && len(a.queue) > 0 && a.ctx.Err() == nil {
		threadID := a.queue[0]
		a.queue = a.queue[1:]
		sl := a.slots[threadID]
		ctx, cancel := context.WithCancel(a.ctx)
		sl.cancel = cancel
		a.busy++
		a.background.Add(1)
		go func() {
			defer a.background.Done()
			a.work(ctx, threadID, sl)
		}()
	}
}

func (a *Agent) work(ctx context.Context, threadID string, sl *slot) {
	view, _ := a.ReadThread(threadID)
	turn, active := view.ActiveTurn()
	var outcome runtime.Outcome
	var err error
	if active {
		var usage repo.Usage
		outcome, usage, err = a.step(ctx, view, turn)
		if err := a.addUsage(threadID, turn.ID, usage); err != nil {
			log.Printf("agent: thread %s: record usage of turn %s: %v", threadID, turn.ID, err)
		}
	}
	interrupted := ctx.Err() != nil
	a.mu.Lock()
	defer a.mu.Unlock()
	a.busy--
	a.afterStepLocked(threadID, sl, outcome, err, interrupted)
	a.dispatchLocked()
}

func (a *Agent) step(ctx context.Context, view repo.ThreadView, turn repo.TurnRow) (runtime.Outcome, repo.Usage, error) {
	if _, err := a.modelOn(turn.Model.Connection); err != nil {
		return runtime.Done, repo.Usage{}, err
	}
	contexts, err := a.contextsFor(turn.Model)
	if err != nil {
		return runtime.Done, repo.Usage{}, err
	}
	descriptors, checker, err := a.toolsFor(a.Tools, permission.Policy(turn.ApprovalPolicy), a.rulesFor(view.Thread))
	if err != nil {
		return runtime.Done, repo.Usage{}, err
	}
	var history, items []repo.ItemRow
	for _, it := range view.Items {
		if it.TurnID == turn.ID {
			items = append(items, it)
		} else {
			history = append(history, it)
		}
	}
	rt := &runtime.Runtime{
		LLM:         a.Connections[turn.Model.Connection],
		Registry:    a.registry,
		Skills:      a.skills,
		Contexts:    contexts,
		Memory:      a.Memory,
		Turn:        turn,
		Caller:      a.callerOf(view.Thread),
		Prompt:      systemprompt.AgentPrompt(a.Name, a.Prompt),
		Tools:       descriptors,
		Permissions: checker,
		History:     history,
		Items:       items,
		Record:      a.recorder(view.Thread, turn),
		Delta: func(it repo.ItemRow, text string) {
			a.emit(Event{Kind: EventItemDelta, Thread: view.Thread, Turn: turn, Item: &it, Delta: text})
		},
	}
	return rt.Step(ctx)
}

func (a *Agent) afterStepLocked(threadID string, sl *slot, outcome runtime.Outcome, err error, interrupted bool) {
	sl.cancel()
	delete(a.slots, threadID)
	view, _ := a.ReadThread(threadID)
	turn, ok := view.ActiveTurn()
	if !ok {
		return
	}
	switch {
	case a.ctx.Err() != nil && (err != nil || interrupted):
		log.Printf("agent: thread %s turn %s left in progress at shutdown: %v", threadID, turn.ID, err)
		return
	case interrupted:
		turn.Status, turn.Error = repo.TurnInterrupted, "interrupted by the user"
		a.endLocked(turn)
	case err != nil:
		turn.Status, turn.Error = repo.TurnFailed, err.Error()
		log.Printf("agent: thread %s turn %s failed: %v", threadID, turn.ID, err)
		a.endLocked(turn)
	case outcome == runtime.Continue || (outcome == runtime.Done && sl.steered):
		a.enqueueLocked(threadID)
	case outcome == runtime.Wait:
		a.launchSubagentsLocked(threadID)
		view, _ = a.ReadThread(threadID)
		a.emitRequests(view)
		a.emitStatus(threadID)
		a.resumeIfReadyLocked(threadID)
	default:
		turn.Status = repo.TurnCompleted
		a.endLocked(turn)
	}
}

func (a *Agent) resumeIfReadyLocked(threadID string) bool {
	if _, queued := a.slots[threadID]; queued {
		return false
	}
	view, ok := a.ReadThread(threadID)
	if !ok {
		return false
	}
	if _, active := view.ActiveTurn(); !active {
		return false
	}
	if _, waiting := view.PendingRequest(); waiting || len(view.AgentCalls()) > 0 {
		return false
	}
	a.enqueueLocked(threadID)
	a.emitStatus(threadID)
	return true
}

func (a *Agent) stopLocked(threadID string, sl *slot) {
	if !a.queuedLocked(threadID) {
		sl.cancel()
		return
	}
	a.queue = slices.DeleteFunc(a.queue, func(id string) bool { return id == threadID })
	delete(a.slots, threadID)
	view, _ := a.ReadThread(threadID)
	if turn, ok := view.ActiveTurn(); ok {
		turn.Status, turn.Error = repo.TurnInterrupted, "interrupted before it ran again"
		a.endLocked(turn)
	}
}

func (a *Agent) endLocked(turn repo.TurnRow) {
	view, _ := a.ReadThread(turn.ThreadID)
	var closed []repo.ItemRow
	var err error
	report, parent, reports := a.reportLocked(view, turn)
	if reports {
		var stored repo.ItemRow
		closed, stored, err = a.completeSubagentTurn(turn.ThreadID, turn, report)
		if err == nil {
			a.emitItem(parent.Thread, parent.Turn, stored, false)
		}
	} else {
		closed, err = a.completeTurn(turn.ThreadID, turn)
	}
	if err != nil {
		log.Printf("agent: thread %s: record end of turn %s: %v", turn.ThreadID, turn.ID, err)
	}
	view, _ = a.ReadThread(turn.ThreadID)
	for _, it := range closed {
		a.emitItem(view.Thread, turn, it, false)
	}
	ended, _ := findTurn(view, turn.ID)
	a.emit(Event{Kind: EventTurnCompleted, Thread: view.Thread, Turn: ended})
	a.emitStatus(turn.ThreadID)
	if reports && err == nil {
		a.resumeIfReadyLocked(parent.Thread.ID)
	}
	a.sendDueLocked(turn.ThreadID)
}

func (a *Agent) recorder(t repo.ThreadRow, turn repo.TurnRow) func(repo.ItemRow) (repo.ItemRow, error) {
	return func(it repo.ItemRow) (repo.ItemRow, error) {
		stored, isNew, err := a.recordItem(t.ID, it)
		if err != nil {
			return repo.ItemRow{}, err
		}
		a.emitItem(t, turn, stored, isNew)
		return stored, nil
	}
}

func findTurn(view repo.ThreadView, turnID string) (repo.TurnRow, bool) {
	for _, t := range view.Turns {
		if t.ID == turnID {
			return t, true
		}
	}
	return repo.TurnRow{}, false
}
