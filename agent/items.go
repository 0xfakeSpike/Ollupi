package agent

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/repo"
)

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
