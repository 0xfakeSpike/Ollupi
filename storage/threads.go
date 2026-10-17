package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/0xfakeSpike/ollupi/execution"
	"reflect"
	"time"

	"github.com/0xfakeSpike/ollupi/thread"
)

// Save writes thread changes in one transaction. A turn or item rewritten
// under another thread or seq is an error, not a silent no-op.
func (d *DB) Save(changes ...thread.Change) error {
	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range changes {
		if err := saveChange(tx, c); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func saveChange(tx *sql.Tx, c thread.Change) error {
	t := c.Thread
	res, err := tx.Exec(`INSERT INTO threads (id, source, cron_job_id, approval_policy, parent_thread_id, parent_item_id, agent, preview, created_at, updated_at, workspace_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT (id) DO UPDATE SET approval_policy = excluded.approval_policy, preview = excluded.preview, updated_at = excluded.updated_at
        WHERE workspace_id IS excluded.workspace_id AND source=excluded.source AND cron_job_id IS excluded.cron_job_id
        AND parent_thread_id IS excluded.parent_thread_id AND parent_item_id IS excluded.parent_item_id AND agent IS excluded.agent AND created_at=excluded.created_at`,
		t.ID, string(t.Source), nullable(t.CronJobID), nullable(t.ApprovalPolicy), nullable(t.ParentThreadID), nullable(t.ParentItemID), nullable(t.Agent),
		t.Preview, formatTime(t.CreatedAt), formatTime(t.UpdatedAt), nullable(t.WorkspaceID))
	if err := checkOne(res, err); err != nil {
		return fmt.Errorf("thread %s: %w", t.ID, err)
	}
	for _, turn := range c.Turns {
		settings, err := json.Marshal(turn.ModelSettings)
		if err != nil {
			return fmt.Errorf("turn %s: encode model settings: %w", turn.ID, err)
		}
		var contextJSON any
		if turn.ExecutionContext != nil {
			encoded, err := json.Marshal(turn.ExecutionContext)
			if err != nil {
				return fmt.Errorf("turn %s: execution context: %w", turn.ID, err)
			}
			contextJSON = string(encoded)
		}
		profileID := turn.ModelProfileID
		if profileID == "" {
			profileID = "primary"
		}
		res, err := tx.Exec(`INSERT INTO turns (id, thread_id, seq, status, agent, model, model_profile_id, model_settings, approval_policy,
				input_tokens, cached_input_tokens, output_tokens, error, started_at, completed_at, execution_context)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET status = excluded.status, input_tokens = excluded.input_tokens,
				cached_input_tokens = excluded.cached_input_tokens, output_tokens = excluded.output_tokens,
				error = excluded.error, completed_at = excluded.completed_at
			WHERE thread_id = excluded.thread_id AND seq = excluded.seq AND execution_context IS excluded.execution_context
            AND agent=excluded.agent AND model=excluded.model AND model_profile_id=excluded.model_profile_id
            AND model_settings=excluded.model_settings AND approval_policy=excluded.approval_policy AND started_at=excluded.started_at`,
			turn.ID, t.ID, turn.Seq, string(turn.Status), turn.Agent, turn.Model, profileID, string(settings), turn.ApprovalPolicy,
			turn.Usage.InputTokens, turn.Usage.CachedInputTokens, turn.Usage.OutputTokens,
			nullable(turn.Error), formatTime(turn.StartedAt), nullableTime(turn.CompletedAt), contextJSON)
		if err := checkOne(res, err); err != nil {
			return fmt.Errorf("turn %s: %w", turn.ID, err)
		}
	}
	for _, it := range c.Items {
		var previousBody string
		err := tx.QueryRow(`SELECT body FROM items WHERE id=?`, it.ID).Scan(&previousBody)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			var previous thread.Item
			if err := json.Unmarshal([]byte(previousBody), &previous); err != nil {
				return err
			}
			if previous.PreparedExecution != nil && (!reflect.DeepEqual(previous.PreparedExecution, it.PreparedExecution) || previous.Arguments != it.Arguments || previous.Tool != it.Tool || previous.Server != it.Server) {
				return fmt.Errorf("item %s: cannot change prepared execution or tool arguments", it.ID)
			}
		}
		body, err := itemBody(it)
		if err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO items (id, thread_id, turn_id, seq, type, status, body, started_at, completed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET status = excluded.status, body = excluded.body, completed_at = excluded.completed_at
			WHERE thread_id = excluded.thread_id AND turn_id = excluded.turn_id AND seq = excluded.seq AND type = excluded.type`,
			it.ID, t.ID, it.TurnID, it.Seq, string(it.Type), string(it.Status), body, formatTime(it.StartedAt), nullableTime(it.CompletedAt))
		if err := checkOne(res, err); err != nil {
			return fmt.Errorf("item %s: %w", it.ID, err)
		}
	}
	return nil
}

// checkOne requires a write to have changed exactly one row: an upsert whose
// WHERE rejected the rewrite changes none.
func checkOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("already stored with a different immutable identity or execution context")
	}
	return nil
}

// Load returns every thread, oldest first, with its turns and items in seq
// order.
func (d *DB) Load() ([]thread.View, error) {
	var views []thread.View
	index := map[string]int{}
	rows, err := d.conn.Query(`SELECT id, source, cron_job_id, approval_policy, parent_thread_id, parent_item_id, agent, preview, created_at, updated_at, workspace_id
		FROM threads ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t thread.Thread
		var source, created, updated string
		var cronJobID, policy, parentThread, parentItem, agentName, workspaceID sql.NullString
		if err := rows.Scan(&t.ID, &source, &cronJobID, &policy, &parentThread, &parentItem, &agentName, &t.Preview, &created, &updated, &workspaceID); err != nil {
			return nil, err
		}
		t.WorkspaceID = workspaceID.String
		t.Source, t.CronJobID, t.ApprovalPolicy = thread.Source(source), cronJobID.String, policy.String
		t.ParentThreadID, t.ParentItemID, t.Agent = parentThread.String, parentItem.String, agentName.String
		if t.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("storage: thread %s created_at: %w", t.ID, err)
		}
		if t.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, fmt.Errorf("storage: thread %s updated_at: %w", t.ID, err)
		}
		index[t.ID] = len(views)
		views = append(views, thread.View{Thread: t, Turns: []thread.Turn{}, Items: []thread.Item{}})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := d.loadTurns(views, index); err != nil {
		return nil, err
	}
	if err := d.loadItems(views, index); err != nil {
		return nil, err
	}
	return views, nil
}

func (d *DB) loadTurns(views []thread.View, index map[string]int) error {
	rows, err := d.conn.Query(`SELECT id, thread_id, seq, status, agent, model, model_profile_id, model_settings, approval_policy,
		input_tokens, cached_input_tokens, output_tokens, error, started_at, completed_at, execution_context
		FROM turns ORDER BY thread_id, seq`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var turn thread.Turn
		var status, started, settings string
		var errText, completed, contextJSON sql.NullString
		if err := rows.Scan(&turn.ID, &turn.ThreadID, &turn.Seq, &status, &turn.Agent, &turn.Model, &turn.ModelProfileID, &settings, &turn.ApprovalPolicy,
			&turn.Usage.InputTokens, &turn.Usage.CachedInputTokens, &turn.Usage.OutputTokens, &errText, &started, &completed, &contextJSON); err != nil {
			return err
		}
		turn.Status, turn.Error = thread.TurnStatus(status), errText.String
		if contextJSON.Valid {
			turn.ExecutionContext = new(execution.Context)
			if err := json.Unmarshal([]byte(contextJSON.String), turn.ExecutionContext); err != nil {
				return fmt.Errorf("storage: turn %s execution_context: %w", turn.ID, err)
			}
		}
		if err := json.Unmarshal([]byte(settings), &turn.ModelSettings); err != nil {
			return fmt.Errorf("storage: turn %s model_settings: %w", turn.ID, err)
		}
		if turn.StartedAt, err = parseTime(started); err != nil {
			return fmt.Errorf("storage: turn %s started_at: %w", turn.ID, err)
		}
		if turn.CompletedAt, err = parseNullableTime(completed); err != nil {
			return fmt.Errorf("storage: turn %s completed_at: %w", turn.ID, err)
		}
		i, ok := index[turn.ThreadID]
		if !ok {
			return fmt.Errorf("storage: turn %s belongs to unknown thread %s", turn.ID, turn.ThreadID)
		}
		v := &views[i]
		if turn.Seq != len(v.Turns)+1 {
			return fmt.Errorf("storage: thread %s turns have a gap at seq %d", turn.ThreadID, len(v.Turns)+1)
		}
		v.Turns = append(v.Turns, turn)
	}
	return rows.Err()
}

func (d *DB) loadItems(views []thread.View, index map[string]int) error {
	rows, err := d.conn.Query(`SELECT id, thread_id, turn_id, seq, type, status, body, started_at, completed_at FROM items ORDER BY thread_id, seq`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, threadID, turnID, kind, status, body, started string
		var seq int
		var completed sql.NullString
		if err := rows.Scan(&id, &threadID, &turnID, &seq, &kind, &status, &body, &started, &completed); err != nil {
			return err
		}
		var it thread.Item
		if err := json.Unmarshal([]byte(body), &it); err != nil {
			return fmt.Errorf("storage: item %s body: %w", id, err)
		}
		it.ID, it.TurnID, it.Seq, it.Type, it.Status = id, turnID, seq, thread.ItemType(kind), thread.ItemStatus(status)
		if it.StartedAt, err = parseTime(started); err != nil {
			return fmt.Errorf("storage: item %s started_at: %w", id, err)
		}
		if it.CompletedAt, err = parseNullableTime(completed); err != nil {
			return fmt.Errorf("storage: item %s completed_at: %w", id, err)
		}
		i, ok := index[threadID]
		if !ok {
			return fmt.Errorf("storage: item %s belongs to unknown thread %s", id, threadID)
		}
		v := &views[i]
		if seq != len(v.Items)+1 {
			return fmt.Errorf("storage: thread %s items have a gap at seq %d", threadID, len(v.Items)+1)
		}
		v.Items = append(v.Items, it)
	}
	return rows.Err()
}

// itemColumns are the item fields stored in columns rather than in body.
var itemColumns = []string{"id", "turnId", "seq", "type", "status", "startedAt", "completedAt"}

// itemBody encodes the fields of an item that have no column.
func itemBody(it thread.Item) (string, error) {
	raw, err := json.Marshal(it)
	if err != nil {
		return "", fmt.Errorf("item %s: encode: %w", it.ID, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", fmt.Errorf("item %s: encode: %w", it.ID, err)
	}
	for _, column := range itemColumns {
		delete(fields, column)
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("item %s: encode: %w", it.ID, err)
	}
	return string(body), nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

func nullableTime(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(*t), Valid: true}
}

func parseNullableTime(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// nullable stores an empty string as NULL.
func nullable(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
