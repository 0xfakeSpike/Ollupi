package repo

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

type MemoryChangeRow struct {
	ID          int64  `gorm:"primaryKey;autoIncrement"`
	EntityTable string `gorm:"not null;uniqueIndex:memory_changes_entity,priority:1"`
	EntityID    string `gorm:"not null;uniqueIndex:memory_changes_entity,priority:2"`
	WorkspaceID string `gorm:"not null;default:'';index:memory_changes_scope,priority:1"`
	ThreadID    string `gorm:"not null;default:''"`
	TurnID      string `gorm:"not null;default:''"`
	Operation   string `gorm:"not null;default:'upsert'"`
	Version     int64  `gorm:"not null;default:1"`
	Metadata    string `gorm:"not null;default:'{}'"`
	Attempts    int    `gorm:"not null;default:0"`
	NextAttempt int64  `gorm:"not null;default:0"`
	LastError   string `gorm:"not null;default:''"`
}

func (MemoryChangeRow) TableName() string { return "memory_changes" }

func (d *DB) QueuedScope(nowMillis int64) (scope string, ok bool, err error) {
	var row MemoryChangeRow
	err = d.ready(nowMillis).Order("c.id").Limit(1).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	return row.WorkspaceID, err == nil, err
}

func (d *DB) QueuedChanges(scope string, nowMillis int64, limit int) ([]MemoryChangeRow, error) {
	var rows []MemoryChangeRow
	err := d.ready(nowMillis).Where("c.workspace_id = ?", scope).Order("c.id").Limit(limit).Find(&rows).Error
	return rows, err
}

func (d *DB) ready(nowMillis int64) *gorm.DB {
	return d.gorm.Table("memory_changes AS c").
		Where("c.next_attempt <= ?", nowMillis).
		Where("NOT EXISTS (SELECT 1 FROM turns t WHERE t.status = 'inProgress' AND (t.id = c.turn_id OR (c.entity_table = 'threads' AND t.thread_id = c.thread_id)))")
}

func (d *DB) QueuedChange(id int64) (MemoryChangeRow, error) {
	var row MemoryChangeRow
	return row, notFound(d.gorm.Where("id = ?", id).Take(&row).Error)
}

func (d *DB) AckChange(id, version int64) (ok bool, err error) {
	res := d.gorm.Where("id = ? AND version = ?", id, version).Delete(&MemoryChangeRow{})
	return res.RowsAffected == 1, res.Error
}

func (d *DB) DeferChange(id, version int64, scope, message string, nowMillis int64) (bool, error) {
	res := d.gorm.Model(&MemoryChangeRow{}).Where("id = ? AND version = ? AND workspace_id = ?", id, version, scope).Updates(map[string]any{
		"next_attempt": gorm.Expr("? + MIN(60, 1 << MIN(attempts, 6)) * 60000", nowMillis),
		"attempts":     gorm.Expr("attempts + 1"),
		"last_error":   message,
	})
	return res.RowsAffected == 1, res.Error
}

func memoryTriggersSQL() string {
	var b strings.Builder
	type tracked struct{ table, scope, thread, turn, meta, update string }
	tables := []tracked{
		{"workspaces", "r.id", "''", "''", "'{}'", "NEW.name IS NOT OLD.name OR NEW.root_path IS NOT OLD.root_path OR NEW.archived_at IS NOT OLD.archived_at"},
		{"threads", "r.workspace_id", "r.id", "''", "'{}'", "NEW.workspace_id IS NOT OLD.workspace_id OR NEW.parent_thread_id IS NOT OLD.parent_thread_id"},
		{"turns", "(SELECT workspace_id FROM threads WHERE id=r.thread_id)", "r.thread_id", "r.id", "'{}'", "NEW.status IS NOT OLD.status OR NEW.error IS NOT OLD.error OR NEW.completed_at IS NOT OLD.completed_at"},
		{"items", "(SELECT workspace_id FROM threads WHERE id=r.thread_id)", "r.thread_id", "r.turn_id", "json_object('type',r.type)", "NEW.type IS NOT OLD.type OR NEW.status IS NOT OLD.status OR " + changed("text", "cron_job_id", "memory", "call_id", "tool", "arguments", "server", "annotations", "approval", "await", "result", "error")},
		{"cron_jobs", "(SELECT workspace_id FROM threads WHERE id=r.thread_id)", "r.thread_id", "''", "'{}'", "NEW.spec IS NOT OLD.spec OR NEW.prompt IS NOT OLD.prompt"},
	}
	for _, t := range tables {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			row, timing := "NEW", "AFTER"
			if op == "DELETE" {
				row, timing = "OLD", "BEFORE"
			}
			replace := func(s string) string { return strings.ReplaceAll(s, "r.", row+".") }
			condition := "1"
			if t.table == "items" {
				condition = row + ".type NOT IN ('reasoning','contextCompaction','memoryContext')"
			}
			if op == "UPDATE" {
				condition += " AND (" + t.update + ")"
			}

			scope := fmt.Sprintf("COALESCE(%s,(SELECT workspace_id FROM memory_changes WHERE entity_table='%s' AND entity_id=%s.id),'')", replace(t.scope), t.table, row)
			operation := "upsert"
			if op == "DELETE" {
				operation = "delete"
			}
			fmt.Fprintf(&b, `CREATE TRIGGER memory_%s_%s %s %s ON %s WHEN %s BEGIN
INSERT INTO memory_changes(entity_table,entity_id,workspace_id,thread_id,turn_id,operation,metadata)
VALUES('%s',%s.id,%s,%s,%s,'%s',%s)
ON CONFLICT(entity_table,entity_id) DO UPDATE SET workspace_id=excluded.workspace_id,thread_id=excluded.thread_id,turn_id=excluded.turn_id,operation=excluded.operation,metadata=excluded.metadata,version=memory_changes.version+1,attempts=0,next_attempt=0,last_error='';
END;
`, t.table, strings.ToLower(op), timing, op, t.table, condition, t.table, row, scope, replace(t.thread), replace(t.turn), operation, replace(t.meta))
		}
	}

	b.WriteString(`
CREATE TRIGGER memory_thread_children BEFORE DELETE ON threads BEGIN
INSERT INTO memory_changes(entity_table,entity_id,workspace_id,thread_id,turn_id,operation,metadata)
SELECT 'turns',id,COALESCE(OLD.workspace_id,''),thread_id,id,'delete','{}' FROM turns WHERE thread_id=OLD.id
ON CONFLICT(entity_table,entity_id) DO UPDATE SET workspace_id=excluded.workspace_id,operation='delete',version=memory_changes.version+1,attempts=0,next_attempt=0,last_error='';
INSERT INTO memory_changes(entity_table,entity_id,workspace_id,thread_id,turn_id,operation,metadata)
SELECT 'items',id,COALESCE(OLD.workspace_id,''),thread_id,turn_id,'delete',json_object('type',type) FROM items WHERE thread_id=OLD.id AND type NOT IN ('reasoning','contextCompaction','memoryContext')
ON CONFLICT(entity_table,entity_id) DO UPDATE SET workspace_id=excluded.workspace_id,operation='delete',version=memory_changes.version+1,attempts=0,next_attempt=0,last_error='';
INSERT INTO memory_changes(entity_table,entity_id,workspace_id,thread_id,turn_id,operation,metadata)
SELECT 'cron_jobs',id,COALESCE(OLD.workspace_id,''),thread_id,'','delete','{}' FROM cron_jobs WHERE thread_id=OLD.id
ON CONFLICT(entity_table,entity_id) DO UPDATE SET workspace_id=excluded.workspace_id,operation='delete',version=memory_changes.version+1,attempts=0,next_attempt=0,last_error='';
END;
`)
	return b.String()
}

func changed(columns ...string) string {
	parts := make([]string, len(columns))
	for i, c := range columns {
		parts[i] = "NEW." + c + " IS NOT OLD." + c
	}
	return strings.Join(parts, " OR ")
}
