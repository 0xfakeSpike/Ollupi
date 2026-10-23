package repo

import (
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm/clause"
)

type MemorySource struct {
	Table      string `json:"table"`
	ID         string `json:"id"`
	Digest     string `json:"digest,omitempty"`
	Operation  string `json:"operation,omitempty"`
	OccurredAt string `json:"occurredAt,omitempty"`
}

type MemoryRow struct {
	ID          string         `gorm:"primaryKey" json:"id"`
	WorkspaceID string         `gorm:"not null;default:'';uniqueIndex:memories_scope_topic,priority:1;index:memories_scope_status,priority:1" json:"workspaceId,omitempty"`
	Topic       string         `gorm:"not null;uniqueIndex:memories_scope_topic,priority:2" json:"topic"`
	Kind        string         `gorm:"not null" json:"kind"`
	Text        string         `gorm:"not null" json:"text"`
	Status      string         `gorm:"not null;check:memories_status,status IN ('active','retired','forgotten');index:memories_scope_status,priority:2" json:"status"`
	Revision    int            `gorm:"not null;check:memories_revision,revision > 0" json:"revision"`
	UserEdited  bool           `gorm:"not null" json:"userEdited,omitempty"`
	Sources     []MemorySource `gorm:"not null;serializer:json;check:memories_sources,json_valid(sources)" json:"sources"`
	CreatedAt   time.Time      `gorm:"not null;autoCreateTime:false" json:"createdAt"`
	UpdatedAt   time.Time      `gorm:"not null;autoUpdateTime:false" json:"updatedAt"`
}

func (MemoryRow) TableName() string { return "memories" }

func (d *DB) Memories(scope string, includeGlobal bool) ([]MemoryRow, error) {
	var rows []MemoryRow
	err := d.gorm.Where("workspace_id = ? OR (? AND workspace_id = '')", scope, includeGlobal).Order("updated_at DESC, id").Find(&rows).Error
	return rows, err
}

func (d *DB) Memory(id string) (MemoryRow, error) {
	var row MemoryRow
	return row, notFound(d.gorm.Where("id = ?", id).Take(&row).Error)
}

func (d *DB) SaveMemory(m MemoryRow, operation string) error {
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	if m.Sources == nil {
		m.Sources = []MemorySource{}
	}
	snapshot, err := json.Marshal(m)
	if err != nil {
		return err
	}
	err = d.gorm.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"topic", "kind", "text", "status", "revision", "sources", "updated_at", "user_edited"}),
	}).Create(&m).Error
	if err != nil {
		return err
	}
	return d.gorm.Create(&MemoryRevisionRow{MemoryID: m.ID, Revision: m.Revision, Operation: operation, Snapshot: string(snapshot), CreatedAt: m.UpdatedAt}).Error
}

func (d *DB) RowExists(table, id string) (bool, error) {
	switch table {
	case "workspaces", "threads", "turns", "items", "cron_jobs":
	default:
		return false, fmt.Errorf("unsupported memory source table %q", table)
	}
	var n int64
	err := d.gorm.Table(table).Where("id = ?", id).Count(&n).Error
	return n > 0, err
}
