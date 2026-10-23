package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
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
		scope, ok, err := tx.QueuedScope(now)
		if err != nil || !ok {
			return err
		}
		changes, err := tx.QueuedChanges(scope, now, limit)
		if err != nil {
			return err
		}
		b := &Batch{WorkspaceID: scope}
		facts := map[string]bool{}
		add := func(table, id, operation, meta string) error {
			if id == "" || facts[table+"/"+id] {
				return nil
			}
			fact, err := memoryFact(tx, table, id, operation, meta)
			if err != nil || fact == nil {
				return err
			}
			facts[table+"/"+id] = true
			b.Facts = append(b.Facts, *fact)
			return nil
		}
		for _, c := range changes {
			b.Changes = append(b.Changes, c)
			if err := add(c.EntityTable, c.EntityID, c.Operation, c.Metadata); err != nil {
				return err
			}
			if c.Operation == "delete" {
				continue
			}
			for _, rel := range [][2]string{{"workspaces", scope}, {"threads", c.ThreadID}, {"turns", c.TurnID}} {
				if err := add(rel[0], rel[1], "upsert", ""); err != nil {
					return err
				}
			}
			if c.TurnID != "" {
				ids, err := tx.UserMessageIDs(c.TurnID, 2)
				if err != nil {
					return err
				}
				for _, id := range ids {
					if err := add("items", id, "upsert", ""); err != nil {
						return err
					}
				}
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
		var err error
		switch table {
		case "workspaces":
			var w repo.WorkspaceRow
			if w, err = tx.Workspace(id); err == nil {
				occurredAt = stamp(w.UpdatedAt)
				data = map[string]any{"name": clipMemory(w.Name, 500), "rootPath": clipMemory(w.RootPath, 1000), "archived": w.ArchivedAt != nil}
			}
		case "threads":
			var t repo.ThreadRow
			if t, err = tx.ThreadRow(id); err == nil {
				occurredAt = stamp(t.CreatedAt)
				data = map[string]any{"workspaceId": t.WorkspaceID, "parentThreadId": t.ParentThreadID}
			}
		case "turns":
			var t repo.TurnRow
			if t, err = tx.EndedTurn(id); err == nil {
				occurredAt = stamp(t.StartedAt)
				data = map[string]any{"threadId": t.ThreadID, "seq": t.Seq, "status": t.Status, "model": t.Model, "error": clipMemory(t.Error, 1000)}
			}
		case "items":
			var it repo.ItemRow
			if it, err = tx.EndedItem(id); err == nil {
				occurredAt = stamp(it.StartedAt)
				data = map[string]any{"threadId": it.ThreadID, "turnId": it.TurnID, "seq": it.Seq, "type": it.Type, "status": it.Status}
				if err := itemEvidence(data, it); err != nil {
					return nil, err
				}
			}
		case "cron_jobs":
			var j repo.CronJobRow
			var scope string
			if j, scope, err = tx.CronJobWithScope(id); err == nil {
				occurredAt = stamp(j.CreatedAt)
				data = map[string]any{"workspaceId": scope, "threadId": j.ThreadID, "schedule": clipMemory(j.Spec, 250), "prompt": clipMemory(j.Prompt, 3000)}
			}
		default:
			return nil, fmt.Errorf("unsupported memory source %q", table)
		}
		if errors.Is(err, repo.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	return &Fact{Source: repo.MemorySource{Table: table, ID: id, Digest: hex.EncodeToString(hash[:]), Operation: operation, OccurredAt: occurredAt}, Data: raw}, nil
}

func itemEvidence(data map[string]any, it repo.ItemRow) error {
	encoded, err := json.Marshal(it)
	if err != nil {
		return fmt.Errorf("memory evidence item %s: %w", it.ID, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return fmt.Errorf("memory evidence item %s: %w", it.ID, err)
	}
	remaining := 4000
	for _, key := range []string{"text", "tool", "server", "arguments", "await", "result", "error"} {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		bound := 1500
		if key == "text" {
			bound = 3000
			if it.Type == "userMessage" {
				bound = 1500
			}
		}
		if key == "result" {
			bound = 3000
		}
		if remaining < 100 {
			data[key] = "[omitted: evidence budget reached]"
			continue
		}
		if bound > remaining {
			bound = remaining
		}
		for {
			data[key] = boundedMemoryValue(v, raw, bound)
			encoded, _ := json.Marshal(data[key])
			size := len([]rune(string(encoded)))
			if size <= remaining || bound < 32 {
				remaining -= size
				break
			}
			bound /= 2
		}
	}
	return nil
}

func clipMemory(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n[truncated]"
}

// commit applies one batch's extraction to the memories of its scope inside a
// single transaction. It re-reads every receipt and source and fails with
// ErrConflict if anything changed underneath it.
func commit(db *repo.DB, b *Batch, ops []Operation, report repo.MemoryReport) error {
	if b == nil || len(b.Changes) == 0 {
		return errors.New("memory batch has no receipts")
	}
	return db.Tx(func(tx *repo.DB) error {
		s, deleted, err := loadCommitState(tx, b)
		if err != nil {
			return err
		}
		if err := s.apply(ops); err != nil {
			return err
		}
		if err := s.retireDeletedSources(deleted); err != nil {
			return err
		}
		if err := ackChanges(tx, b.Changes); err != nil {
			return err
		}
		return tx.SetMemorySuccess(s.now, report)
	})
}

// commitState is the working set of one commit. Every map is rebuilt from the
// database inside the transaction, so nothing survives between batches.
type commitState struct {
	tx               *repo.DB
	scope            string
	now              time.Time
	byID             map[string]repo.MemoryRow
	byTopic          map[string]string
	forgottenSources map[string]bool              // source identity blocked by a forgotten memory
	allowed          map[string]repo.MemorySource // table/id -> source the extractor may cite
	exact            map[string]repo.MemorySource // source identity -> source
	touched          map[string]bool
}

func loadCommitState(tx *repo.DB, b *Batch) (*commitState, map[string]bool, error) {
	deleted, err := checkReceipts(tx, b.Changes, b.WorkspaceID)
	if err != nil {
		return nil, nil, err
	}
	current, err := tx.Memories(b.WorkspaceID, false)
	if err != nil {
		return nil, nil, err
	}
	s := &commitState{
		tx: tx, scope: b.WorkspaceID, now: time.Now().UTC(),
		byID: map[string]repo.MemoryRow{}, byTopic: map[string]string{},
		forgottenSources: map[string]bool{}, allowed: map[string]repo.MemorySource{},
		exact: map[string]repo.MemorySource{}, touched: map[string]bool{},
	}
	if err := s.loadCurrent(current, b.Existing); err != nil {
		return nil, nil, err
	}
	if err := s.loadAllowed(tx, b); err != nil {
		return nil, nil, err
	}
	return s, deleted, nil
}

// checkReceipts verifies every change is still queued with the same version
// and scope, and returns the entities this batch deletes.
func checkReceipts(tx *repo.DB, changes []repo.MemoryChangeRow, scope string) (map[string]bool, error) {
	deleted := map[string]bool{}
	for _, change := range changes {
		c, err := tx.QueuedChange(change.ID)
		if errors.Is(err, repo.ErrNotFound) {
			return nil, ErrConflict
		}
		if err != nil {
			return nil, err
		}
		if c.Version != change.Version || c.WorkspaceID != scope {
			return nil, ErrConflict
		}
		if c.Operation == "delete" {
			deleted[c.EntityTable+"/"+c.EntityID] = true
		}
	}
	return deleted, nil
}

// loadCurrent indexes the memories currently in this scope and verifies none
// changed since the batch was read.
func (s *commitState) loadCurrent(current, existing []repo.MemoryRow) error {
	baseline := map[string]int{}
	for _, e := range existing {
		baseline[e.ID] = e.Revision
	}
	if len(current) != len(baseline) {
		return ErrConflict
	}
	for _, e := range current {
		if baseline[e.ID] != e.Revision {
			return ErrConflict
		}
		s.byID[e.ID] = e
		s.byTopic[e.Topic] = e.ID
		if e.Status == "forgotten" {
			for _, src := range mostSpecificMemorySources(e.Sources) {
				s.forgottenSources[memorySourceIdentity(src)] = true
			}
		}
	}
	return nil
}

// loadAllowed re-reads each fact to confirm it is still current, then builds
// the allowed sources: the batch's facts plus every source an existing memory
// already carries, with the batch's facts taking precedence.
func (s *commitState) loadAllowed(tx *repo.DB, b *Batch) error {
	for _, f := range b.Facts {
		key := f.Source.Table + "/" + f.Source.ID
		if f.Source.Operation != "delete" {
			fresh, err := memoryFact(tx, f.Source.Table, f.Source.ID, "upsert", "")
			if err != nil {
				return err
			}
			if fresh == nil || fresh.Source.Digest != f.Source.Digest {
				return ErrConflict
			}
		}
		s.allowed[key] = f.Source
	}
	for _, src := range s.allowed {
		s.exact[memorySourceIdentity(src)] = src
	}
	for _, e := range b.Existing {
		for _, src := range e.Sources {
			s.exact[memorySourceIdentity(src)] = src
			if _, ok := s.allowed[src.Table+"/"+src.ID]; !ok {
				s.allowed[src.Table+"/"+src.ID] = src
			}
		}
	}
	return nil
}

func (s *commitState) apply(ops []Operation) error {
	for _, op := range ops {
		e, skip, err := s.prepare(op)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		sources, blocked, err := s.evidenceFor(op)
		if err != nil {
			return err
		}
		if blocked {
			continue
		}
		if err := s.assign(&e, op, sources); err != nil {
			return err
		}
		if err := s.tx.SaveMemory(e, op.Action); err != nil {
			return err
		}
		s.byID[e.ID] = e
		s.byTopic[e.Topic] = e.ID
		s.touched[e.ID] = true
	}
	return nil
}

// prepare validates an operation's target and returns the row it will write.
// skip reports an operation that must be dropped rather than applied (already
// covered, forgotten, or user-edited).
func (s *commitState) prepare(op Operation) (repo.MemoryRow, bool, error) {
	switch op.Action {
	case "add":
		topic := normalizeTopic(op.Topic)
		if topic == "" || len([]rune(topic)) > 200 {
			return repo.MemoryRow{}, false, errors.New("memory topic must contain 1 to 200 characters")
		}
		if _, exists := s.byTopic[topic]; exists {
			return repo.MemoryRow{}, true, nil
		}
		return repo.MemoryRow{ID: uuid.NewString(), WorkspaceID: s.scope, Topic: topic, Kind: op.Kind, Status: "active", Revision: 1, CreatedAt: s.now, UpdatedAt: s.now}, false, nil
	case "update", "retire":
		e, ok := s.byID[op.ID]
		if !ok || e.Revision != op.Revision || s.touched[e.ID] {
			return repo.MemoryRow{}, false, ErrConflict
		}
		if e.Status == "forgotten" || e.UserEdited {
			return repo.MemoryRow{}, true, nil
		}
		if op.Topic != "" && normalizeTopic(op.Topic) != e.Topic {
			return repo.MemoryRow{}, false, errors.New("memory topic is immutable; update the existing stable topic")
		}
		e.Revision++
		e.UpdatedAt = s.now
		return e, false, nil
	default:
		return repo.MemoryRow{}, false, fmt.Errorf("invalid memory action %q", op.Action)
	}
}

// evidenceFor resolves the operation's citations to canonical sources. blocked
// reports evidence a forgotten memory owns, or sources that no longer exist.
func (s *commitState) evidenceFor(op Operation) ([]repo.MemorySource, bool, error) {
	sources := []repo.MemorySource{}
	seen := map[string]bool{}
	blocked := false
	for _, src := range op.Sources {
		key := src.Table + "/" + src.ID
		canonical, ok := s.allowed[key]
		if src.Digest != "" {
			canonical, ok = s.exact[memorySourceIdentity(src)]
		}
		if !ok {
			return nil, false, fmt.Errorf("memory source %s was not provided to extractor", key)
		}
		if s.forgottenSources[memorySourceIdentity(canonical)] {
			blocked = true
		}
		if op.Action != "retire" {
			exists, err := s.tx.RowExists(canonical.Table, canonical.ID)
			if err != nil {
				return nil, false, err
			}
			if !exists {
				blocked = true
			}
		}
		identity := memorySourceIdentity(canonical)
		if !seen[identity] {
			sources = append(sources, canonical)
			seen[identity] = true
		}
	}
	return sources, blocked, nil
}

// assign writes the operation's outcome into the row.
func (s *commitState) assign(e *repo.MemoryRow, op Operation, sources []repo.MemorySource) error {
	if op.Action == "retire" {
		e.Status = "retired"
		if len(sources) > 0 {
			e.Sources = sources
		}
		return nil
	}
	if len(sources) == 0 {
		return errors.New("memory requires evidence")
	}
	for _, src := range sources {
		if src.Operation == "delete" {
			return errors.New("deleted evidence cannot establish active memory")
		}
	}
	e.Text = strings.TrimSpace(op.Text)
	if e.Text == "" || len([]rune(e.Text)) > 8000 {
		return errors.New("memory text must contain 1 to 8000 characters")
	}
	if op.Kind != "" {
		e.Kind = op.Kind
	}
	if e.Kind == "" || len(e.Kind) > 100 {
		return errors.New("memory kind is required")
	}
	e.Status = "active"
	e.Sources = sources
	return nil
}

// retireDeletedSources retires active memories that cite an entity deleted in
// this batch.
func (s *commitState) retireDeletedSources(deleted map[string]bool) error {
	for _, e := range s.byID {
		if e.Status != "active" || e.UserEdited {
			continue
		}
		for _, src := range e.Sources {
			if deleted[src.Table+"/"+src.ID] {
				e.Status = "retired"
				e.Revision++
				e.UpdatedAt = s.now
				if err := s.tx.SaveMemory(e, "source_deleted"); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func ackChanges(tx *repo.DB, changes []repo.MemoryChangeRow) error {
	for _, c := range changes {
		ok, err := tx.AckChange(c.ID, c.Version)
		if err != nil {
			return err
		}
		if !ok {
			return ErrConflict
		}
	}
	return nil
}

func normalizeTopic(topic string) string {
	return strings.ToLower(strings.Join(strings.Fields(topic), " "))
}

func mostSpecificMemorySources(sources []repo.MemorySource) []repo.MemorySource {
	priorities := map[string]int{"items": 5, "cron_jobs": 4, "turns": 3, "threads": 2, "workspaces": 1}
	best := 0
	for _, s := range sources {
		if priorities[s.Table] > best {
			best = priorities[s.Table]
		}
	}
	out := []repo.MemorySource{}
	for _, s := range sources {
		if priorities[s.Table] == best {
			out = append(out, s)
		}
	}
	return out
}
func memorySourceIdentity(s repo.MemorySource) string { return s.Table + "/" + s.ID + "/" + s.Digest }

func boundedMemoryValue(v any, raw []byte, bound int) any {
	if value, ok := v.(string); ok {
		return clipMemory(value, bound)
	}
	if len([]rune(string(raw))) > bound {
		return map[string]any{"truncated": true, "excerpt": clipMemory(string(raw), bound)}
	}
	return v
}

func deferBatch(db *repo.DB, b *Batch, message string) error {
	if b == nil {
		return errors.New("memory batch is required")
	}
	message = clipMemory(message, 1000)
	return db.Tx(func(tx *repo.DB) error {
		changed := false
		now := time.Now().UnixMilli()
		for _, c := range b.Changes {
			ok, err := tx.DeferChange(c.ID, c.Version, b.WorkspaceID, message, now)
			if err != nil {
				return err
			}
			changed = changed || ok
		}
		if !changed {
			return nil
		}
		return tx.SetMemoryError(message)
	})
}
