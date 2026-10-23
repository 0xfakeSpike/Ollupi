package agent

import (
	"sort"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/tools/userinput"
	"github.com/0xfakeSpike/ollupi/repo"
)

func (a *Agent) ResolveApproval(threadID, approvalID string, decision repo.Decision, decidedBy string) error {
	return a.run.resolveApproval(threadID, approvalID, decision, decidedBy)
}

func (a *Agent) AnswerUserInput(threadID, itemID string, answers map[string]userinput.Answer) error {
	return a.run.answerUserInput(threadID, itemID, answers)
}

type Request struct {
	Thread repo.ThreadRow
	Item   repo.ItemRow
}

func (a *Agent) PendingRequests(threadID string) []Request {
	view, err := a.ReadThread(threadID)
	if err != nil {
		return nil
	}
	var out []Request
	for _, it := range view.PendingRequests() {
		out = append(out, Request{Thread: view.Thread, Item: it})
	}
	for _, call := range view.AgentCalls() {
		if child, err := a.ReadThread(call.Await.ThreadID); err == nil {
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
