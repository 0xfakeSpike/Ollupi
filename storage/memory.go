package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/memory"
	"github.com/google/uuid"
)

var _ memory.Store = (*DB)(nil)
var _ memory.AdminStore = (*DB)(nil)
var _ memory.RetryStore = (*DB)(nil)

const entryColumns = `id,workspace_id,topic,kind,text,status,revision,sources,created_at,updated_at,user_edited`

type memoryQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type memoryReceipt struct {
	memory.Change
	table, id, scope, thread, turn, operation, metadata string
}

// MemoryNextBatch uses a short, consistent read transaction. It never waits for
// a running turn and never holds a connection while the extractor calls a model.
func (d *DB) MemoryNextBatch(ctx context.Context, limit int) (*memory.Batch, error) {
	if limit < 1 {
		limit = 16
	}
	if limit > 100 {
		limit = 100
	}
	tx, err := d.conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	const eligible = `c.next_attempt <= ? AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.status='inProgress' AND (t.id=c.turn_id OR (c.entity_table='threads' AND t.thread_id=c.thread_id)))`
	var scope string
	err = tx.QueryRowContext(ctx, `SELECT c.workspace_id FROM memory_changes c WHERE `+eligible+` ORDER BY c.id LIMIT 1`, time.Now().UnixMilli()).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.version,c.entity_table,c.entity_id,c.workspace_id,c.thread_id,c.turn_id,c.operation,c.metadata FROM memory_changes c WHERE c.workspace_id=? AND `+eligible+` ORDER BY c.id LIMIT ?`, scope, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	var receipts []memoryReceipt
	for rows.Next() {
		var c memoryReceipt
		if err = rows.Scan(&c.ID, &c.Version, &c.table, &c.id, &c.scope, &c.thread, &c.turn, &c.operation, &c.metadata); err != nil {
			rows.Close()
			return nil, err
		}
		receipts = append(receipts, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	batch := &memory.Batch{WorkspaceID: scope}
	facts := map[string]bool{}
	add := func(table, id, operation string, meta string) error {
		if id == "" {
			return nil
		}
		key := table + "/" + id
		if facts[key] {
			return nil
		}
		fact, err := memoryFact(ctx, tx, table, id, operation, meta)
		if err != nil {
			return err
		}
		if fact == nil {
			return nil
		}
		facts[key] = true
		batch.Facts = append(batch.Facts, *fact)
		return nil
	}
	for _, c := range receipts {
		batch.Changes = append(batch.Changes, c.Change)
		if err = add(c.table, c.id, c.operation, c.metadata); err != nil {
			return nil, err
		}
		if c.operation == "delete" {
			continue
		}
		if err = add("workspaces", scope, "upsert", ""); err != nil {
			return nil, err
		}
		if err = add("threads", c.thread, "upsert", ""); err != nil {
			return nil, err
		}
		if err = add("turns", c.turn, "upsert", ""); err != nil {
			return nil, err
		}
		if c.turn != "" {
			// User requests supply the purpose behind an item. Bound this relation;
			// remaining user messages retain their own durable queue receipts.
			rs, e := tx.QueryContext(ctx, `SELECT id FROM items WHERE turn_id=? AND type='userMessage' ORDER BY seq LIMIT 2`, c.turn)
			if e != nil {
				return nil, e
			}
			var ids []string
			for rs.Next() {
				var id string
				if e = rs.Scan(&id); e != nil {
					rs.Close()
					return nil, e
				}
				ids = append(ids, id)
			}
			e = rs.Err()
			rs.Close()
			if e != nil {
				return nil, e
			}
			for _, id := range ids {
				if err = add("items", id, "upsert", ""); err != nil {
					return nil, err
				}
			}
		}
	}
	batch.Existing, err = memoryList(ctx, tx, scope, false)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return batch, nil
}

// memoryFact deliberately omits credentials, model settings, instruction
// snapshots, model reasoning, and telemetry. Tool output is evidence, never an
// instruction to the extractor. Every clipped field declares the truncation.
func memoryFact(ctx context.Context, q memoryQuery, table, id, operation, metadata string) (*memory.Fact, error) {
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
			var name, path string
			var archived sql.NullString
			err = q.QueryRowContext(ctx, `SELECT name,root_path,archived_at,updated_at FROM workspaces WHERE id=?`, id).Scan(&name, &path, &archived, &occurredAt)
			data = map[string]any{"name": clipMemory(name, 500), "rootPath": clipMemory(path, 1000), "archived": archived.Valid}
		case "threads":
			var scope, source, parent, job string
			err = q.QueryRowContext(ctx, `SELECT COALESCE(workspace_id,''),source,COALESCE(parent_thread_id,''),COALESCE(cron_job_id,''),created_at FROM threads WHERE id=?`, id).Scan(&scope, &source, &parent, &job, &occurredAt)
			data = map[string]any{"workspaceId": scope, "source": source, "parentThreadId": parent, "cronJobId": job}
		case "turns":
			var threadID, status, agent, model, errText string
			var seq int
			err = q.QueryRowContext(ctx, `SELECT thread_id,seq,status,agent,model,COALESCE(error,''),started_at FROM turns WHERE id=? AND status<>'inProgress'`, id).Scan(&threadID, &seq, &status, &agent, &model, &errText, &occurredAt)
			data = map[string]any{"threadId": threadID, "seq": seq, "status": status, "agent": agent, "model": model, "error": clipMemory(errText, 1000)}
		case "items":
			var threadID, turnID, kind, status, body string
			var seq int
			err = q.QueryRowContext(ctx, `SELECT i.thread_id,i.turn_id,i.seq,i.type,i.status,i.body,i.started_at FROM items i JOIN turns t ON t.id=i.turn_id WHERE i.id=? AND t.status<>'inProgress' AND i.type NOT IN ('reasoning','contextCompaction','memoryContext')`, id).Scan(&threadID, &turnID, &seq, &kind, &status, &body, &occurredAt)
			data = map[string]any{"threadId": threadID, "turnId": turnID, "seq": seq, "type": kind, "status": status}
			var fields map[string]json.RawMessage
			if err == nil {
				if err = json.Unmarshal([]byte(body), &fields); err != nil {
					return nil, fmt.Errorf("memory evidence item %s: %w", id, err)
				}
				remaining := 4000
				for _, key := range []string{"text", "tool", "server", "arguments", "result", "error", "questions", "answers", "agent", "receiverThreadIds"} {
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
						if kind == "userMessage" {
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
			}
		case "cron_jobs":
			var scope, spec, prompt string
			err = q.QueryRowContext(ctx, `SELECT COALESCE(workspace_id,''),spec,prompt,created_at FROM cron_jobs WHERE id=?`, id).Scan(&scope, &spec, &prompt, &occurredAt)
			data = map[string]any{"workspaceId": scope, "schedule": clipMemory(spec, 250), "prompt": clipMemory(prompt, 3000)}
		default:
			return nil, fmt.Errorf("unsupported memory source %q", table)
		}
		if errors.Is(err, sql.ErrNoRows) {
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
	return &memory.Fact{Source: memory.Source{Table: table, ID: id, Digest: hex.EncodeToString(hash[:]), Operation: operation, OccurredAt: occurredAt}, Data: raw}, nil
}

func clipMemory(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n[truncated]"
}

func (d *DB) MemoryList(ctx context.Context, scope string, includeGlobal bool) ([]memory.Entry, error) {
	return memoryList(ctx, d.conn, scope, includeGlobal)
}
func memoryList(ctx context.Context, q memoryQuery, scope string, includeGlobal bool) ([]memory.Entry, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+entryColumns+` FROM memories WHERE workspace_id=? OR (? AND workspace_id='') ORDER BY updated_at DESC,id`, scope, includeGlobal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []memory.Entry{}
	for rows.Next() {
		e, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}
func scanMemory(row interface{ Scan(...any) error }) (memory.Entry, error) {
	var e memory.Entry
	var sources, created, updated string
	err := row.Scan(&e.ID, &e.WorkspaceID, &e.Topic, &e.Kind, &e.Text, &e.Status, &e.Revision, &sources, &created, &updated, &e.UserEdited)
	if err != nil {
		return e, err
	}
	if err = json.Unmarshal([]byte(sources), &e.Sources); err != nil {
		return e, err
	}
	e.CreatedAt, err = parseTime(created)
	if err != nil {
		return e, err
	}
	e.UpdatedAt, err = parseTime(updated)
	return e, err
}

func (d *DB) MemoryStatus(ctx context.Context) (memory.Status, error) {
	var s memory.Status
	var last, report sql.NullString
	err := d.conn.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM memory_changes),(SELECT count(*) FROM memories WHERE status='active'),(SELECT count(*) FROM memory_changes WHERE attempts>0),last_success,COALESCE((SELECT last_error FROM memory_changes WHERE attempts>0 ORDER BY id LIMIT 1),last_error),last_report FROM memory_state WHERE id=1`).Scan(&s.Pending, &s.Active, &s.Failed, &last, &s.LastError, &report)
	if err != nil {
		return s, err
	}
	s.LastSuccess, err = parseNullableTime(last)
	if err != nil {
		return s, err
	}
	if report.Valid {
		var r memory.Report
		if err = json.Unmarshal([]byte(report.String), &r); err != nil {
			return s, err
		}
		s.LastReport = &r
	}
	return s, nil
}
func (d *DB) MemoryFailure(ctx context.Context, message string) error {
	_, err := d.conn.ExecContext(ctx, `UPDATE memory_state SET last_error=? WHERE id=1`, clipMemory(message, 1000))
	return err
}

func writeMemory(ctx context.Context, tx *sql.Tx, e memory.Entry, operation string) error {
	sources, err := json.Marshal(e.Sources)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memories(`+entryColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET topic=excluded.topic,kind=excluded.kind,text=excluded.text,status=excluded.status,revision=excluded.revision,sources=excluded.sources,updated_at=excluded.updated_at,user_edited=excluded.user_edited`, e.ID, e.WorkspaceID, e.Topic, e.Kind, e.Text, e.Status, e.Revision, string(sources), formatTime(e.CreatedAt), formatTime(e.UpdatedAt), e.UserEdited)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_revisions(memory_id,revision,operation,snapshot,created_at) VALUES(?,?,?,?,?)`, e.ID, e.Revision, operation, string(raw), formatTime(e.UpdatedAt))
	return err
}

func (d *DB) MemoryEdit(ctx context.Context, id string, revision int, value string) (memory.Entry, error) {
	var zero memory.Entry
	value = strings.TrimSpace(value)
	if value == "" || len([]rune(value)) > 8000 {
		return zero, errors.New("memory text must contain 1 to 8000 characters")
	}
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	e, err := scanMemory(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM memories WHERE id=?`, id))
	if err != nil {
		return zero, err
	}
	if e.Revision != revision || e.Status == "forgotten" {
		return zero, memory.ErrConflict
	}
	e.Text = value
	e.UserEdited = true
	e.Status = "active"
	e.Revision++
	e.UpdatedAt = time.Now().UTC()
	if err = writeMemory(ctx, tx, e, "user_edit"); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	return e, nil
}
func (d *DB) MemoryForget(ctx context.Context, id string, revision int) error {
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	e, err := scanMemory(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM memories WHERE id=?`, id))
	if err != nil {
		return err
	}
	if e.Revision != revision {
		return memory.ErrConflict
	}
	if e.Status == "forgotten" {
		return nil
	}
	e.Status = "forgotten"
	e.Text = ""
	e.Revision++
	e.UpdatedAt = time.Now().UTC()
	// Forget removes retained prose while keeping only identity/source tombstones.
	if _, err = tx.ExecContext(ctx, `DELETE FROM memory_revisions WHERE memory_id=?`, id); err != nil {
		return err
	}
	if err = writeMemory(ctx, tx, e, "user_forget"); err != nil {
		return err
	}
	return tx.Commit()
}

// MemoryCommit acknowledges exactly the snapshots processed by the model. The
// transaction contains no model work; a changed source or user edit causes a
// retry without advancing any receipt.
func (d *DB) MemoryCommit(ctx context.Context, b *memory.Batch, ops []memory.Operation, report memory.Report) error {
	if b == nil || len(b.Changes) == 0 {
		return errors.New("memory batch has no receipts")
	}
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	deleted := map[string]bool{}
	for _, change := range b.Changes {
		var version int64
		var scope, table, id, operation string
		err = tx.QueryRowContext(ctx, `SELECT version,workspace_id,entity_table,entity_id,operation FROM memory_changes WHERE id=?`, change.ID).Scan(&version, &scope, &table, &id, &operation)
		if errors.Is(err, sql.ErrNoRows) {
			return memory.ErrConflict
		}
		if err != nil {
			return err
		}
		if version != change.Version || scope != b.WorkspaceID {
			return memory.ErrConflict
		}
		if operation == "delete" {
			deleted[table+"/"+id] = true
		}
	}
	current, err := memoryList(ctx, tx, b.WorkspaceID, false)
	if err != nil {
		return err
	}
	// Checking the scoped generation also catches an explicit forget racing with
	// an add under a different topic, before old evidence can recreate it.
	baseline := map[string]int{}
	for _, e := range b.Existing {
		baseline[e.ID] = e.Revision
	}
	if len(current) != len(baseline) {
		return memory.ErrConflict
	}
	byID := map[string]memory.Entry{}
	byTopic := map[string]string{}
	forgottenSources := map[string]bool{}
	for _, e := range current {
		if baseline[e.ID] != e.Revision {
			return memory.ErrConflict
		}
		byID[e.ID] = e
		byTopic[e.Topic] = e.ID
		if e.Status == "forgotten" {
			for _, s := range mostSpecificMemorySources(e.Sources) {
				forgottenSources[memorySourceIdentity(s)] = true
			}
		}
	}
	available := map[string]memory.Source{}
	for _, f := range b.Facts {
		key := f.Source.Table + "/" + f.Source.ID
		if f.Source.Operation != "delete" {
			fresh, e := memoryFact(ctx, tx, f.Source.Table, f.Source.ID, "upsert", "")
			if e != nil {
				return e
			}
			if fresh == nil || fresh.Source.Digest != f.Source.Digest {
				return memory.ErrConflict
			}
		}
		available[key] = f.Source
	}
	now := time.Now().UTC()
	touched := map[string]bool{}
	for _, op := range ops {
		if op.Action != "add" && op.Action != "update" && op.Action != "retire" {
			return fmt.Errorf("invalid memory action %q", op.Action)
		}
		var e memory.Entry
		if op.Action == "add" {
			topic := strings.ToLower(strings.Join(strings.Fields(op.Topic), " "))
			if topic == "" || len([]rune(topic)) > 200 {
				return errors.New("memory topic must contain 1 to 200 characters")
			}
			if _, exists := byTopic[topic]; exists {
				continue
			}
			e = memory.Entry{ID: uuid.NewString(), WorkspaceID: b.WorkspaceID, Topic: topic, Kind: op.Kind, Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
		} else {
			var ok bool
			e, ok = byID[op.ID]
			if !ok || e.Revision != op.Revision || touched[e.ID] {
				return memory.ErrConflict
			}
			if e.Status == "forgotten" || e.UserEdited {
				continue
			}
			if op.Topic != "" && strings.ToLower(strings.Join(strings.Fields(op.Topic), " ")) != e.Topic {
				return errors.New("memory topic is immutable; update the existing stable topic")
			}
			e.Revision++
			e.UpdatedAt = now
		}
		allowed := map[string]memory.Source{}
		exact := map[string]memory.Source{}
		for k, v := range available {
			allowed[k] = v
			exact[memorySourceIdentity(v)] = v
		}
		for _, existing := range b.Existing {
			for _, s := range existing.Sources {
				exact[memorySourceIdentity(s)] = s
				if _, ok := allowed[s.Table+"/"+s.ID]; !ok {
					allowed[s.Table+"/"+s.ID] = s
				}
			}
		}
		sources := []memory.Source{}
		seen := map[string]bool{}
		blocked := false
		for _, s := range op.Sources {
			key := s.Table + "/" + s.ID
			canonical, ok := allowed[key]
			if s.Digest != "" {
				canonical, ok = exact[memorySourceIdentity(s)]
			}
			if !ok {
				return fmt.Errorf("memory source %s was not provided to extractor", key)
			}
			if forgottenSources[memorySourceIdentity(canonical)] {
				blocked = true
			}
			if op.Action != "retire" {
				exists, err := memorySourceExists(ctx, tx, canonical)
				if err != nil {
					return err
				}
				// A historical upsert digest remains valid provenance, but cannot
				// establish an active fact after its source has been deleted.
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
		if blocked {
			continue
		}
		if op.Action == "retire" {
			e.Status = "retired"
			if len(sources) > 0 {
				e.Sources = sources
			}
		} else {
			if len(sources) == 0 {
				return errors.New("memory requires evidence")
			}
			for _, s := range sources {
				if s.Operation == "delete" {
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
		}
		if err = writeMemory(ctx, tx, e, op.Action); err != nil {
			return err
		}
		byID[e.ID] = e
		byTopic[e.Topic] = e.ID
		touched[e.ID] = true
	}
	// A disappearing source cannot silently leave an asserted fact active. Keep
	// the retired entry and its provenance for inspection; retrieval excludes it.
	for _, e := range byID {
		if e.Status != "active" || e.UserEdited {
			continue
		}
		for _, s := range e.Sources {
			if deleted[s.Table+"/"+s.ID] {
				e.Status = "retired"
				e.Revision++
				e.UpdatedAt = now
				if err = writeMemory(ctx, tx, e, "source_deleted"); err != nil {
					return err
				}
				break
			}
		}
	}
	for _, c := range b.Changes {
		res, e := tx.ExecContext(ctx, `DELETE FROM memory_changes WHERE id=? AND version=?`, c.ID, c.Version)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return memory.ErrConflict
		}
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE memory_state SET last_success=?,last_error=CASE WHEN EXISTS(SELECT 1 FROM memory_changes WHERE attempts>0) THEN last_error ELSE '' END,last_report=? WHERE id=1`, formatTime(now), string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

// A workspace/thread source often supplies scope rather than the remembered
// fact. Forget the narrowest cited evidence so one forgotten decision does not
// prevent unrelated learning throughout the same project. A changed digest is
// fresh evidence; the forgotten topic itself remains suppressed.
func mostSpecificMemorySources(sources []memory.Source) []memory.Source {
	priorities := map[string]int{"items": 5, "cron_jobs": 4, "turns": 3, "threads": 2, "workspaces": 1}
	best := 0
	for _, s := range sources {
		if priorities[s.Table] > best {
			best = priorities[s.Table]
		}
	}
	out := []memory.Source{}
	for _, s := range sources {
		if priorities[s.Table] == best {
			out = append(out, s)
		}
	}
	return out
}
func memorySourceIdentity(s memory.Source) string { return s.Table + "/" + s.ID + "/" + s.Digest }

func memorySourceExists(ctx context.Context, q memoryQuery, source memory.Source) (bool, error) {
	switch source.Table {
	case "workspaces", "threads", "turns", "items", "cron_jobs":
	default:
		return false, errors.New("unsupported memory source table")
	}
	var exists bool
	// Table is selected from the fixed allowlist above; IDs remain parameters.
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+source.Table+` WHERE id=?)`, source.ID).Scan(&exists)
	return exists, err
}

func boundedMemoryValue(v any, raw []byte, bound int) any {
	if value, ok := v.(string); ok {
		return clipMemory(value, bound)
	}
	if len([]rune(string(raw))) > bound {
		return map[string]any{"truncated": true, "excerpt": clipMemory(string(raw), bound)}
	}
	return v
}

// MemoryDefer prevents a malformed or unavailable extraction from starving
// unrelated scopes. Only the receipts actually attempted receive backoff;
// later business writes reset their retry delay in the SQL triggers.
func (d *DB) MemoryDefer(ctx context.Context, b *memory.Batch, message string) error {
	if b == nil {
		return errors.New("memory batch is required")
	}
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed := int64(0)
	for _, c := range b.Changes {
		res, e := tx.ExecContext(ctx, `UPDATE memory_changes SET next_attempt=?+MIN(60,1 << MIN(attempts,6))*60000,attempts=attempts+1,last_error=? WHERE id=? AND version=? AND workspace_id=?`, time.Now().UnixMilli(), clipMemory(message, 1000), c.ID, c.Version, b.WorkspaceID)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		changed += n
	}
	if changed > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE memory_state SET last_error=? WHERE id=1`, clipMemory(message, 1000)); err != nil {
			return err
		}
	}
	return tx.Commit()
}
