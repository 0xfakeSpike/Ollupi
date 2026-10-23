package memory

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

// commit applies one batch's extraction to the memories of its scope inside a
// single transaction. Current rows are re-read at apply time so user edits
// made while the model was running take precedence (last write wins); there is
// no revision comparison.
func commit(db *repo.DB, b *Batch, ops []Operation, report repo.MemoryReport) error {
	if b == nil || len(b.Events) == 0 {
		return errors.New("memory batch has no events")
	}
	return db.Tx(func(tx *repo.DB) error {
		s, deleted, err := loadState(tx, b)
		if err != nil {
			return err
		}
		if err := s.apply(ops); err != nil {
			return err
		}
		if err := s.retireDeletedSources(deleted); err != nil {
			return err
		}
		if err := ackEvents(tx, b.Events); err != nil {
			return err
		}
		return tx.SetMemorySuccess(s.now, report)
	})
}

type applyState struct {
	tx      *repo.DB
	scope   string
	now     time.Time
	byID    map[string]repo.MemoryRow
	byTopic map[string]string
	allowed map[string]repo.MemorySource
	blocked map[string]bool
}

func loadState(tx *repo.DB, b *Batch) (*applyState, map[string]bool, error) {
	deleted := map[string]bool{}
	for _, ev := range b.Events {
		if ev.Operation == "delete" {
			deleted[ev.SourceTable+"/"+ev.SourceID] = true
		}
	}
	current, err := tx.Memories(b.WorkspaceID, false)
	if err != nil {
		return nil, nil, err
	}
	s := &applyState{
		tx: tx, scope: b.WorkspaceID, now: time.Now().UTC(),
		byID:    map[string]repo.MemoryRow{},
		byTopic: map[string]string{},
		allowed: map[string]repo.MemorySource{},
		blocked: map[string]bool{},
	}
	for _, e := range current {
		s.byID[e.ID] = e
		s.byTopic[e.Topic] = e.ID
		if e.Status == "forgotten" {
			for _, src := range e.Sources {
				s.blocked[src.Table+"/"+src.ID] = true
			}
		}
		for _, src := range e.Sources {
			s.allowed[src.Table+"/"+src.ID] = src
		}
	}
	for _, f := range b.Facts {
		s.allowed[f.Source.Table+"/"+f.Source.ID] = f.Source
	}
	return s, deleted, nil
}

func (s *applyState) apply(ops []Operation) error {
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
		if err := s.tx.SaveMemory(e); err != nil {
			return err
		}
		s.byID[e.ID] = e
		s.byTopic[e.Topic] = e.ID
	}
	return nil
}

// prepare validates an operation's target and returns the row it will write.
// skip reports an operation that must be dropped (already covered, forgotten,
// or user-edited) rather than applied.
func (s *applyState) prepare(op Operation) (repo.MemoryRow, bool, error) {
	switch op.Action {
	case "add":
		topic := normalizeTopic(op.Topic)
		if topic == "" || len([]rune(topic)) > 200 {
			return repo.MemoryRow{}, false, errors.New("memory topic must contain 1 to 200 characters")
		}
		if _, exists := s.byTopic[topic]; exists {
			return repo.MemoryRow{}, true, nil
		}
		return repo.MemoryRow{ID: uuid.NewString(), WorkspaceID: s.scope, Topic: topic, Kind: op.Kind, Status: "active", CreatedAt: s.now, UpdatedAt: s.now}, false, nil
	case "update", "retire":
		e, ok := s.byID[op.ID]
		if !ok {
			return repo.MemoryRow{}, false, errors.New("memory operation targeted an unavailable record")
		}
		if e.Status == "forgotten" || e.UserEdited {
			return repo.MemoryRow{}, true, nil
		}
		if op.Topic != "" && normalizeTopic(op.Topic) != e.Topic {
			return repo.MemoryRow{}, false, errors.New("memory topic is immutable; update the existing stable topic")
		}
		e.UpdatedAt = s.now
		return e, false, nil
	default:
		return repo.MemoryRow{}, false, fmt.Errorf("invalid memory action %q", op.Action)
	}
}

// evidenceFor resolves the operation's citations to canonical sources. blocked
// reports evidence a forgotten memory owns or, for active writes, sources that
// no longer exist.
func (s *applyState) evidenceFor(op Operation) ([]repo.MemorySource, bool, error) {
	sources := []repo.MemorySource{}
	seen := map[string]bool{}
	blocked := false
	for _, src := range op.Sources {
		key := src.Table + "/" + src.ID
		canonical, ok := s.allowed[key]
		if !ok {
			return nil, false, fmt.Errorf("memory source %s was not provided to extractor", key)
		}
		if s.blocked[key] {
			blocked = true
		}
		if op.Action != "retire" {
			if canonical.Operation == "delete" {
				blocked = true
			} else {
				exists, err := s.tx.RowExists(canonical.Table, canonical.ID)
				if err != nil {
					return nil, false, err
				}
				if !exists {
					blocked = true
				}
			}
		}
		if !seen[key] {
			sources = append(sources, canonical)
			seen[key] = true
		}
	}
	return sources, blocked, nil
}

func (s *applyState) assign(e *repo.MemoryRow, op Operation, sources []repo.MemorySource) error {
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
func (s *applyState) retireDeletedSources(deleted map[string]bool) error {
	for _, e := range s.byID {
		if e.Status != "active" || e.UserEdited {
			continue
		}
		for _, src := range e.Sources {
			if deleted[src.Table+"/"+src.ID] {
				e.Status = "retired"
				e.UpdatedAt = s.now
				if err := s.tx.SaveMemory(e); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

func ackEvents(tx *repo.DB, events []repo.MemoryEventRow) error {
	for _, ev := range events {
		if err := tx.AckEvent(ev.ID); err != nil {
			return err
		}
	}
	return nil
}

func normalizeTopic(topic string) string {
	return strings.ToLower(strings.Join(strings.Fields(topic), " "))
}

func deferBatch(db *repo.DB, b *Batch, message string) error {
	if b == nil {
		return errors.New("memory batch is required")
	}
	message = clipMemory(message, 1000)
	return db.Tx(func(tx *repo.DB) error {
		changed := false
		now := time.Now().UnixMilli()
		for _, ev := range b.Events {
			ok, err := tx.DeferEvent(ev.ID, message, now)
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
