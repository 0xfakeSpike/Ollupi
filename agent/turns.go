package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/repo"
)

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
