package repo

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// MemoryEventRow is one pending memory job. Events are append-only: every
// relevant source change inserts a new row and the worker consumes the rows it
// read by ID. There is no version column or revision reconciliation.
type MemoryEventRow struct {
	ID          int64  `gorm:"primaryKey;autoIncrement"`
	WorkspaceID string `gorm:"not null;default:'';index:memory_events_scope"`
	SourceTable string `gorm:"not null"`
	SourceID    string `gorm:"not null"`
	Operation   string `gorm:"not null;default:'upsert'"`
	Metadata    string `gorm:"not null;default:'{}'"`
	Attempts    int    `gorm:"not null;default:0"`
	NextAttempt int64  `gorm:"not null;default:0"`
	LastError   string `gorm:"not null;default:''"`
}

func (MemoryEventRow) TableName() string { return "memory_events" }

func (d *DB) NextEventScope(nowMillis int64) (scope string, ok bool, err error) {
	var row MemoryEventRow
	err = d.gorm.Where("next_attempt <= ?", nowMillis).Order("id").Limit(1).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, nil
	}
	return row.WorkspaceID, err == nil, err
}

func (d *DB) QueuedEvents(scope string, nowMillis int64, limit int) ([]MemoryEventRow, error) {
	var rows []MemoryEventRow
	err := d.gorm.Where("workspace_id = ? AND next_attempt <= ?", scope, nowMillis).Order("id").Limit(limit).Find(&rows).Error
	return rows, err
}

func (d *DB) AckEvent(id int64) error {
	res := d.gorm.Delete(&MemoryEventRow{}, "id = ?", id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("memory: event %d no longer exists", id)
	}
	return nil
}

func (d *DB) DeferEvent(id int64, message string, nowMillis int64) (bool, error) {
	res := d.gorm.Model(&MemoryEventRow{}).Where("id = ?", id).Updates(map[string]any{
		"next_attempt": gorm.Expr("? + MIN(60, 1 << MIN(attempts, 6)) * 60000", nowMillis),
		"attempts":     gorm.Expr("attempts + 1"),
		"last_error":   message,
	})
	return res.RowsAffected == 1, res.Error
}

// memoryTriggersSQL emits memory events only for terminal state, not for every
// streaming item write: a turn becoming completed/failed/interrupted is the
// only in-conversation trigger, plus workspace, thread and cron job changes.
func memoryTriggersSQL() string {
	return `
CREATE TRIGGER memory_turn_ended AFTER UPDATE OF status ON turns
WHEN NEW.status IN ('completed','failed','interrupted') AND OLD.status IS NOT NEW.status BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  VALUES(COALESCE((SELECT workspace_id FROM threads WHERE id=NEW.thread_id),''),'turns',NEW.id,'upsert',json_object('status',NEW.status));
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  SELECT COALESCE((SELECT workspace_id FROM threads WHERE id=NEW.thread_id),''),'items',id,'upsert',json_object('type',type)
  FROM items WHERE turn_id=NEW.id AND type IN ('userMessage','agentMessage','toolCall','error')
    AND status IN ('completed','failed','declined');
END;

CREATE TRIGGER memory_workspace_insert AFTER INSERT ON workspaces BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  VALUES(NEW.id,'workspaces',NEW.id,'upsert',json_object('archived',NEW.archived_at IS NOT NULL));
END;

CREATE TRIGGER memory_workspace_update AFTER UPDATE ON workspaces
WHEN NEW.name IS NOT OLD.name OR NEW.root_path IS NOT OLD.root_path OR NEW.archived_at IS NOT OLD.archived_at BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  VALUES(NEW.id,'workspaces',NEW.id,'upsert',json_object('archived',NEW.archived_at IS NOT NULL));
END;

CREATE TRIGGER memory_workspace_delete BEFORE DELETE ON workspaces BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  VALUES(OLD.id,'workspaces',OLD.id,'delete',json_object('identity',json_object('name',OLD.name)));
END;

CREATE TRIGGER memory_thread_insert AFTER INSERT ON threads BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  VALUES(COALESCE(NEW.workspace_id,''),'threads',NEW.id,'upsert','{}');
END;

CREATE TRIGGER memory_thread_update AFTER UPDATE ON threads
WHEN NEW.workspace_id IS NOT OLD.workspace_id OR NEW.parent_thread_id IS NOT OLD.parent_thread_id BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  VALUES(COALESCE(NEW.workspace_id,''),'threads',NEW.id,'upsert','{}');
END;

CREATE TRIGGER memory_thread_delete BEFORE DELETE ON threads BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  VALUES(COALESCE(OLD.workspace_id,''),'threads',OLD.id,'delete','{}');
END;

CREATE TRIGGER memory_thread_children BEFORE DELETE ON threads BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  SELECT COALESCE(OLD.workspace_id,''),'items',id,'delete',json_object('type',type)
  FROM items WHERE thread_id=OLD.id AND type NOT IN ('reasoning','contextCompaction','memoryContext');
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  SELECT COALESCE(OLD.workspace_id,''),'turns',id,'delete','{}' FROM turns WHERE thread_id=OLD.id;
END;

CREATE TRIGGER memory_cron_insert AFTER INSERT ON cron_jobs BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  VALUES(COALESCE((SELECT workspace_id FROM threads WHERE id=NEW.thread_id),''),'cron_jobs',NEW.id,'upsert','{}');
END;

CREATE TRIGGER memory_cron_update AFTER UPDATE ON cron_jobs
WHEN NEW.spec IS NOT OLD.spec OR NEW.prompt IS NOT OLD.prompt BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  VALUES(COALESCE((SELECT workspace_id FROM threads WHERE id=NEW.thread_id),''),'cron_jobs',NEW.id,'upsert','{}');
END;

CREATE TRIGGER memory_cron_delete BEFORE DELETE ON cron_jobs BEGIN
  INSERT INTO memory_events(workspace_id,source_table,source_id,operation,metadata)
  VALUES(COALESCE((SELECT workspace_id FROM threads WHERE id=OLD.thread_id),''),'cron_jobs',OLD.id,'delete',json_object('identity',json_object('prompt',OLD.prompt)));
END;
`
}
