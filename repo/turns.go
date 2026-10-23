package repo

import (
	"fmt"
	"time"

	"github.com/0xfakeSpike/ollupi/pkg/llm"
)

type Usage struct {
	InputTokens       int64 `gorm:"not null" json:"inputTokens"`
	CachedInputTokens int64 `gorm:"not null" json:"cachedInputTokens"`
	OutputTokens      int64 `gorm:"not null" json:"outputTokens"`
}

func (u *Usage) Add(v Usage) {
	u.InputTokens += v.InputTokens
	u.CachedInputTokens += v.CachedInputTokens
	u.OutputTokens += v.OutputTokens
}

type TurnRow struct {
	ID             string     `gorm:"primaryKey" json:"id"`
	ThreadID       string     `gorm:"not null;uniqueIndex:turns_thread_seq,priority:1" json:"threadId"`
	Thread         *ThreadRow `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	Seq            int        `gorm:"not null;uniqueIndex:turns_thread_seq,priority:2" json:"seq"`
	Status         string     `gorm:"not null" json:"status"`
	Model          llm.Model  `gorm:"not null;serializer:json;check:turns_model,json_valid(model)" json:"model"`
	ApprovalPolicy string     `gorm:"not null" json:"approvalPolicy"`
	Usage          Usage      `gorm:"embedded" json:"usage"`
	Error          string     `gorm:"not null;default:''" json:"error,omitempty"`
	StartedAt      time.Time  `gorm:"not null" json:"startedAt"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
}

func (TurnRow) TableName() string { return "turns" }

const (
	TurnInProgress  = "inProgress"
	TurnCompleted   = "completed"
	TurnInterrupted = "interrupted"
	TurnFailed      = "failed"
)

func (d *DB) ActiveThreadIDs() ([]string, error) {
	var ids []string
	err := d.gorm.Model(&TurnRow{}).Where("status = 'inProgress'").Order("started_at, id").Pluck("thread_id", &ids).Error
	return ids, err
}

func (d *DB) CreateTurn(row TurnRow) error {
	row.StartedAt, row.CompletedAt = row.StartedAt.UTC(), nullTime(row.CompletedAt)
	return d.gorm.Create(&row).Error
}

func (d *DB) UpdateTurn(row TurnRow) error {
	res := d.gorm.Model(&TurnRow{}).Where("id = ? AND thread_id = ?", row.ID, row.ThreadID).Updates(map[string]any{
		"status": row.Status, "input_tokens": row.Usage.InputTokens, "cached_input_tokens": row.Usage.CachedInputTokens,
		"output_tokens": row.Usage.OutputTokens, "error": row.Error, "completed_at": nullTime(row.CompletedAt),
	})
	if err := checkOne(res); err != nil {
		return fmt.Errorf("turn %s: %w", row.ID, err)
	}
	return nil
}

func (d *DB) ActiveTurn(threadID string) (TurnRow, error) {
	var row TurnRow
	return row, notFound(d.gorm.Where("thread_id = ? AND status = 'inProgress'", threadID).Take(&row).Error)
}

func (d *DB) EndedTurn(id string) (TurnRow, error) {
	var row TurnRow
	return row, notFound(d.gorm.Where("id = ? AND status <> 'inProgress'", id).Take(&row).Error)
}
