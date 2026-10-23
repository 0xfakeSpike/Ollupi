package agent

import (
	"fmt"
	"reflect"
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

func (a *Agent) ReadThread(id string) (repo.ThreadView, bool) {
	v, err := a.DB.Thread(strings.TrimSpace(id))
	return v, err == nil
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
	v, ok := a.ReadThread(id)
	if !ok {
		return ""
	}
	status := v.Status()
	if status != repo.ThreadActive {
		return status
	}
	for _, call := range v.AgentCalls() {
		if child, ok := a.ReadThread(call.Await.ThreadID); ok {
			if cs := child.Status(); cs == repo.ThreadWaitingOnApproval || cs == repo.ThreadWaitingOnUserInput {
				return cs
			}
		}
	}
	return status
}

func (a *Agent) recoverItems(threadID string) ([]repo.ItemRow, error) {
	var closed []repo.ItemRow
	err := a.DB.Tx(func(tx *repo.DB) error {
		v, err := tx.Thread(threadID)
		if err != nil {
			return err
		}
		active, ok := v.ActiveTurn()
		if !ok {
			return nil
		}
		now := time.Now().UTC()

		handling := true
		for _, it := range v.Items {
			if it.TurnID != active.ID || it.Status != repo.ItemInProgress {
				continue
			}
			switch {
			case it.Type == repo.ItemReasoning || it.Type == repo.ItemAgentMessage || it.Type == repo.ItemContextCompaction:
				it.Error = "the app stopped before this finished"
			case it.IsCall() && handling:

				if it.Awaits() {
					continue
				}
				handling = false
				declined := it.Approval != nil && it.Approval.Decision == repo.DecisionDecline
				if it.IsPending() || declined {
					continue
				}
				it.Error = "the app stopped while this call was being handled; it may or may not have taken effect, so check before repeating it"
			default:
				continue
			}
			it.Status, it.CompletedAt = repo.ItemFailed, &now
			if err := saveItemIn(tx, threadID, it); err != nil {
				return err
			}
			closed = append(closed, it)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agent: recover thread %s: %w", threadID, err)
	}
	return closed, nil
}

func (a *Agent) insertTurn(threadID string, turn repo.TurnRow, input []repo.ItemRow) (repo.TurnRow, error) {
	err := a.DB.Tx(func(tx *repo.DB) error {
		v, err := tx.Thread(threadID)
		if err != nil {
			return err
		}
		if active, ok := v.ActiveTurn(); ok {
			return fmt.Errorf("thread %s already has turn %s in progress", threadID, active.ID)
		}
		if turn.ID == "" || turn.Status != repo.TurnInProgress {
			return fmt.Errorf("a new turn needs an id and status inProgress")
		}
		turn.ThreadID, turn.Seq = threadID, len(v.Turns)+1
		if err := tx.CreateTurn(turn); err != nil {
			return err
		}
		t := v.Thread
		for _, it := range input {
			if it.Type == repo.ItemUserMessage && t.Preview == "" {
				t.Preview = clip(firstLine(it.Text), 80)
			}
			if err := saveItemIn(tx, threadID, it); err != nil {
				return err
			}
		}
		t.UpdatedAt = time.Now().UTC()
		return tx.UpdateThread(t)
	})
	if err != nil {
		return repo.TurnRow{}, fmt.Errorf("agent: begin turn: %w", err)
	}
	return turn, nil
}

func (a *Agent) recordItem(threadID string, item repo.ItemRow) (repo.ItemRow, bool, error) {
	var stored repo.ItemRow
	var isNew bool
	err := a.DB.Tx(func(tx *repo.DB) error {
		active, err := tx.ActiveTurn(threadID)
		if repo.IsNotFound(err) || (err == nil && active.ID != item.TurnID) {
			return fmt.Errorf("item %s belongs to turn %s, which is not in progress", item.ID, item.TurnID)
		}
		if err != nil {
			return err
		}
		_, err = tx.Item(item.ID)
		isNew = repo.IsNotFound(err)
		if err != nil && !isNew {
			return err
		}
		if err := saveItemIn(tx, threadID, item); err != nil {
			return err
		}
		stored, err = tx.Item(item.ID)
		return err
	})
	if err != nil {
		return repo.ItemRow{}, false, fmt.Errorf("agent: record: %w", err)
	}
	return stored, isNew, nil
}

func (a *Agent) addUsage(threadID, turnID string, u repo.Usage) error {
	if u == (repo.Usage{}) {
		return nil
	}
	return a.DB.Tx(func(tx *repo.DB) error {
		row, err := tx.ActiveTurn(threadID)
		if repo.IsNotFound(err) || (err == nil && row.ID != turnID) {
			return fmt.Errorf("agent: turn %s is not in progress", turnID)
		}
		if err != nil {
			return err
		}
		row.Usage.Add(u)
		return tx.UpdateTurn(row)
	})
}

func (a *Agent) completeTurn(threadID string, turn repo.TurnRow) ([]repo.ItemRow, error) {
	var closed []repo.ItemRow
	err := a.DB.Tx(func(tx *repo.DB) error {
		var err error
		closed, err = completeIn(tx, threadID, turn)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("agent: complete turn: %w", err)
	}
	return closed, nil
}

func (a *Agent) completeSubagentTurn(threadID string, turn repo.TurnRow, report repo.ItemRow) (closed []repo.ItemRow, stored repo.ItemRow, err error) {
	err = a.DB.Tx(func(tx *repo.DB) error {
		child, err := tx.Thread(threadID)
		if err != nil {
			return err
		}
		parentID := child.Thread.ParentThreadID
		active, err := tx.ActiveTurn(parentID)
		if repo.IsNotFound(err) || (err == nil && (active.ID != report.TurnID || report.ID != child.Thread.ParentItemID)) {
			return fmt.Errorf("item %s is not the Agent call of thread %s's turn in progress", report.ID, parentID)
		}
		if err != nil {
			return err
		}
		if closed, err = completeIn(tx, threadID, turn); err != nil {
			return err
		}
		if err := saveItemIn(tx, parentID, report); err != nil {
			return err
		}
		stored, err = tx.Item(report.ID)
		return err
	})
	if err != nil {
		return nil, repo.ItemRow{}, fmt.Errorf("agent: complete subagent turn: %w", err)
	}
	return closed, stored, nil
}

func completeIn(tx *repo.DB, threadID string, turn repo.TurnRow) ([]repo.ItemRow, error) {
	v, err := tx.Thread(threadID)
	if err != nil {
		return nil, err
	}
	active, ok := v.ActiveTurn()
	if !ok || active.ID != turn.ID {
		return nil, fmt.Errorf("turn %s is not in progress", turn.ID)
	}
	if turn.Status == repo.TurnInProgress {
		return nil, fmt.Errorf("turn %s must end with a final status", turn.ID)
	}
	now := time.Now().UTC()
	turn.ThreadID, turn.Seq, turn.CompletedAt = threadID, active.Seq, &now
	if err := tx.UpdateTurn(turn); err != nil {
		return nil, err
	}
	var closed []repo.ItemRow
	for _, it := range v.Items {
		if it.TurnID != turn.ID || it.Status != repo.ItemInProgress {
			continue
		}
		it.Status = repo.ItemFailed
		it.Error = fmt.Sprintf("the turn ended (%s) before this item finished", turn.Status)
		it.CompletedAt = &now
		if err := saveItemIn(tx, threadID, it); err != nil {
			return nil, err
		}
		closed = append(closed, it)
	}
	t := v.Thread
	t.UpdatedAt = now
	return closed, tx.UpdateThread(t)
}

func saveItemIn(tx *repo.DB, threadID string, it repo.ItemRow) error {
	if it.ID == "" || it.TurnID == "" || it.Type == "" || it.Status == "" {
		return fmt.Errorf("item needs an id, turn, type, and status")
	}
	prev, err := tx.Item(it.ID)
	switch {
	case repo.IsNotFound(err):
		if it.Seq, err = tx.NextItemSeq(threadID); err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if prev.ThreadID != threadID || prev.Type != it.Type || prev.TurnID != it.TurnID {
			return fmt.Errorf("item %s cannot change its thread, type, or turn", it.ID)
		}
		if prev.PreparedExecution != nil && (!reflect.DeepEqual(prev.PreparedExecution, it.PreparedExecution) || prev.Arguments != it.Arguments || prev.Tool != it.Tool || prev.Server != it.Server) {
			return fmt.Errorf("item %s cannot change its prepared execution or tool arguments", it.ID)
		}
		it.Seq = prev.Seq
	}
	it.ThreadID = threadID
	return tx.SaveItem(it)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func clip(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}
