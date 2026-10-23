package agent

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/agent/tools/userinput"
	"github.com/0xfakeSpike/ollupi/repo"
)

func (a *Agent) ResolveApproval(threadID, approvalID string, decision repo.Decision, decidedBy string) error {
	return a.respond(threadID, func(it repo.ItemRow) bool { return it.WaitsForApproval() && it.Approval.ID == approvalID }, func(it repo.ItemRow) (repo.ItemRow, error) {
		if available := it.Approval.AvailableDecisions(); !slices.Contains(available, decision) {
			return repo.ItemRow{}, fmt.Errorf("agent: decision %q is not one of %q", decision, available)
		}
		now := time.Now().UTC()
		it.Approval.Decision, it.Approval.DecidedBy, it.Approval.DecidedAt = decision, decidedBy, &now
		return it, nil
	})
}

func (a *Agent) AnswerUserInput(threadID, itemID string, answers map[string]userinput.Answer) error {
	return a.respond(threadID, func(it repo.ItemRow) bool { return it.ID == itemID && !it.WaitsForApproval() }, func(it repo.ItemRow) (repo.ItemRow, error) {
		req, err := userinput.Decode(it.Await.Request)
		if err != nil {
			return repo.ItemRow{}, err
		}
		if err := req.Check(answers, maxInputRunes); err != nil {
			return repo.ItemRow{}, fmt.Errorf("agent: %w", err)
		}
		result, _, err := repo.ResultOf(tools.Result{Output: answers})
		if err != nil {
			return repo.ItemRow{}, err
		}
		now := time.Now().UTC()
		it.Result, it.Status, it.CompletedAt = result, repo.ItemCompleted, &now
		return it, nil
	})
}

func (a *Agent) respond(threadID string, match func(repo.ItemRow) bool, apply func(repo.ItemRow) (repo.ItemRow, error)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	view, ok := a.ReadThread(threadID)
	if !ok {
		return fmt.Errorf("agent: thread %s not found", threadID)
	}
	if _, scheduled := a.slots[threadID]; scheduled {
		return errNoSuchRequest
	}
	i := slices.IndexFunc(view.PendingRequests(), match)
	if i < 0 {
		return errNoSuchRequest
	}
	pending := view.PendingRequests()[i]
	activeTurn, _ := view.ActiveTurn()
	answered, err := apply(pending)
	if err != nil {
		return err
	}
	stored, _, err := a.recordItem(threadID, answered)
	if err != nil {
		return err
	}
	a.emitItem(view.Thread, activeTurn, stored, false)
	if !a.resumeIfReadyLocked(threadID) {
		a.emitStatus(threadID)
	}
	return nil
}

type Request struct {
	Thread repo.ThreadRow
	Item   repo.ItemRow
}

func (a *Agent) PendingRequests(threadID string) []Request {
	view, ok := a.ReadThread(threadID)
	if !ok {
		return nil
	}
	var out []Request
	for _, it := range view.PendingRequests() {
		out = append(out, Request{Thread: view.Thread, Item: it})
	}
	for _, call := range view.AgentCalls() {
		if child, ok := a.ReadThread(call.Await.ThreadID); ok {
			for _, it := range child.PendingRequests() {
				out = append(out, Request{Thread: child.Thread, Item: it})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return requestedAt(out[i].Item).Before(requestedAt(out[j].Item)) })
	return out
}

func requestedAt(it repo.ItemRow) time.Time {
	if it.Approval != nil {
		return it.Approval.RequestedAt
	}
	return it.StartedAt
}
