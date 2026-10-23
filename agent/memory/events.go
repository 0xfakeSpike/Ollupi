package memory

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/0xfakeSpike/ollupi/repo"
)

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nextBatch(db *repo.DB, limit int) (*Batch, error) {
	if limit < 1 {
		limit = 16
	}
	if limit > 100 {
		limit = 100
	}
	var batch *Batch
	err := db.Tx(func(tx *repo.DB) error {
		now := time.Now().UnixMilli()
		scope, ok, err := tx.NextEventScope(now)
		if err != nil || !ok {
			return err
		}
		events, err := tx.QueuedEvents(scope, now, limit)
		if err != nil || len(events) == 0 {
			return err
		}
		b := &Batch{WorkspaceID: scope, Events: events}
		seen := map[string]bool{}
		for i := len(events) - 1; i >= 0; i-- {
			ev := events[i]
			if seen[ev.SourceTable+"/"+ev.SourceID] {
				continue
			}
			seen[ev.SourceTable+"/"+ev.SourceID] = true
			if err := addEvidence(tx, b, ev); err != nil {
				return err
			}
		}
		if b.Existing, err = tx.Memories(scope, false); err != nil {
			return err
		}
		batch = b
		return nil
	})
	return batch, err
}

func addEvidence(tx *repo.DB, b *Batch, ev repo.MemoryEventRow) error {
	fact, err := memoryFact(tx, ev.SourceTable, ev.SourceID, ev.Operation, ev.Metadata)
	if err != nil || fact == nil {
		return err
	}
	b.Facts = append(b.Facts, *fact)
	return nil
}

func memoryFact(tx *repo.DB, table, id, operation, metadata string) (*Fact, error) {
	data := map[string]any{}
	var occurredAt string
	if operation == "delete" {
		data["deleted"] = true
		if metadata != "" {
			var m map[string]any
			if json.Unmarshal([]byte(metadata), &m) == nil {
				data["identity"] = m
			}
		}
	} else {
		switch table {
		case "workspaces":
			var w repo.WorkspaceRow
			w, err := tx.Workspace(id)
			if err != nil {
				if err == repo.ErrNotFound {
					return nil, nil
				}
				return nil, err
			}
			occurredAt = stamp(w.UpdatedAt)
			data = map[string]any{"name": clipMemory(w.Name, 500), "rootPath": clipMemory(w.RootPath, 1000), "archived": w.ArchivedAt != nil}
		case "threads":
			var t repo.ThreadRow
			t, err := tx.ThreadRow(id)
			if err != nil {
				if err == repo.ErrNotFound {
					return nil, nil
				}
				return nil, err
			}
			occurredAt = stamp(t.UpdatedAt)
			data = map[string]any{"workspaceId": t.WorkspaceID, "parentThreadId": t.ParentThreadID}
		case "turns":
			var t repo.TurnRow
			t, err := tx.EndedTurn(id)
			if err != nil {
				if err == repo.ErrNotFound {
					return nil, nil
				}
				return nil, err
			}
			occurredAt = stamp(t.StartedAt)
			data = map[string]any{"threadId": t.ThreadID, "status": t.Status, "error": clipMemory(t.Error, 1000)}
		case "items":
			var it repo.ItemRow
			it, err := tx.EndedItem(id)
			if err != nil {
				if err == repo.ErrNotFound {
					return nil, nil
				}
				return nil, err
			}
			fact, err := itemFact(it)
			if err != nil {
				return nil, err
			}
			return &fact, nil
		case "cron_jobs":
			var j repo.CronJobRow
			var scope string
			j, scope, err := tx.CronJobWithScope(id)
			if err != nil {
				if err == repo.ErrNotFound {
					return nil, nil
				}
				return nil, err
			}
			occurredAt = stamp(j.CreatedAt)
			data = map[string]any{"workspaceId": scope, "threadId": j.ThreadID, "schedule": clipMemory(j.Spec, 250), "prompt": clipMemory(j.Prompt, 3000)}
		default:
			return nil, fmt.Errorf("unsupported memory source %q", table)
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &Fact{Source: repo.MemorySource{Table: table, ID: id, Operation: operation, OccurredAt: occurredAt}, Data: raw}, nil
}

func itemFact(it repo.ItemRow) (Fact, error) {
	data := map[string]any{"threadId": it.ThreadID, "turnId": it.TurnID, "type": it.Type, "status": it.Status}
	remaining := 4000
	for key, value := range map[string]any{
		"text": it.Text, "tool": it.Tool, "arguments": it.Arguments, "result": resultText(it), "error": it.Error,
	} {
		if value == "" || value == nil {
			continue
		}
		if remaining < 100 {
			data[key] = "[omitted: evidence budget reached]"
			continue
		}
		bound := 1500
		if key == "text" {
			bound = 3000
			if it.Type == repo.ItemUserMessage {
				bound = 1500
			}
		}
		if key == "result" {
			bound = 3000
		}
		if bound > remaining {
			bound = remaining
		}
		text := value.(string)
		if runeLen(text) > bound {
			text = clipMemory(text, bound)
		}
		data[key] = text
		remaining -= runeLen(text)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Fact{}, err
	}
	return Fact{Source: repo.MemorySource{Table: "items", ID: it.ID, Operation: "upsert", OccurredAt: stamp(it.StartedAt)}, Data: raw}, nil
}

func resultText(it repo.ItemRow) string {
	if it.Result == nil {
		return ""
	}
	if it.Error != "" {
		return clipMemory(it.Error, 1000)
	}
	return string(it.Result.Output)
}

// visibleEvidence drops facts a forgotten memory owns, so extraction cannot
// recreate what the user deliberately removed.
func visibleEvidence(batch *Batch) *Batch {
	blocked := map[string]bool{}
	for _, e := range batch.Existing {
		if e.Status != "forgotten" {
			continue
		}
		for _, s := range e.Sources {
			blocked[s.Table+"/"+s.ID] = true
		}
	}
	out := *batch
	out.Facts = make([]Fact, 0, len(batch.Facts))
	for _, f := range batch.Facts {
		if !blocked[f.Source.Table+"/"+f.Source.ID] {
			out.Facts = append(out.Facts, f)
		}
	}
	return &out
}

func clipMemory(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n[truncated]"
}

func runeLen(s string) int { return len([]rune(s)) }
