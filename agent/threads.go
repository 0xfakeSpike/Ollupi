package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

func (a *Agent) StartThread(workspaceID, approvalPolicy string) (repo.ThreadRow, error) {
	if err := a.checkWorkspace(workspaceID); err != nil {
		return repo.ThreadRow{}, err
	}
	if approvalPolicy != "" {
		if err := permission.Policy(approvalPolicy).Validate(); err != nil {
			return repo.ThreadRow{}, fmt.Errorf("agent: %w", err)
		}
	}
	return a.createThread(repo.ThreadRow{ApprovalPolicy: approvalPolicy, WorkspaceID: workspaceID})
}

func (a *Agent) createThread(t repo.ThreadRow) (repo.ThreadRow, error) {
	now := time.Now().UTC()
	t.ID = uuid.NewString()
	t.CreatedAt, t.UpdatedAt = now, now
	if err := a.DB.CreateThread(t); err != nil {
		return repo.ThreadRow{}, fmt.Errorf("agent: create: %w", err)
	}
	return t, nil
}

func (a *Agent) ReadThread(id string) (repo.ThreadView, error) {
	v, err := a.DB.Thread(strings.TrimSpace(id))
	if err != nil {
		return repo.ThreadView{}, fmt.Errorf("agent: thread %s: %w", id, err)
	}
	return v, nil
}

func (a *Agent) ListThreads(f repo.ThreadFilter) ([]repo.ThreadSummary, error) {
	rows, err := a.DB.ThreadRows(f)
	if err != nil {
		return nil, err
	}
	out := make([]repo.ThreadSummary, 0, len(rows))
	for _, r := range rows {
		status := a.threadStatus(r.ID)
		out = append(out, repo.ThreadSummary{ThreadRow: r, Status: status})
	}
	return out, nil
}

func (a *Agent) threadStatus(id string) repo.ThreadStatus {
	v, err := a.ReadThread(id)
	if err != nil {
		return ""
	}
	status := v.Status()
	if status != repo.ThreadActive {
		return status
	}
	for _, call := range v.AgentCalls() {
		if child, err := a.ReadThread(call.Await.ThreadID); err == nil {
			if cs := child.Status(); cs == repo.ThreadWaitingOnApproval || cs == repo.ThreadWaitingOnUserInput {
				return cs
			}
		}
	}
	return status
}
