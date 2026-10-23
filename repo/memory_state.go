package repo

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"gorm.io/gorm"
)

type MemoryReport struct {
	Model                llm.Model `json:"model"`
	EstimatedInputTokens int       `json:"estimatedInputTokens"`
	InputTokens          int64     `json:"inputTokens"`
	OutputTokens         int64     `json:"outputTokens"`
	UsageReported        bool      `json:"usageReported"`
}

type MemoryStateRow struct {
	ID          int           `gorm:"primaryKey;autoIncrement:false;check:memory_state_one,id = 1" json:"-"`
	LastSuccess *time.Time    `json:"lastSuccess,omitempty"`
	LastError   string        `gorm:"not null;default:''" json:"lastError,omitempty"`
	LastReport  *MemoryReport `gorm:"serializer:json;check:memory_state_report,last_report IS NULL OR json_valid(last_report)" json:"lastReport,omitempty"`
}

func (MemoryStateRow) TableName() string { return "memory_state" }

type MemoryStatus struct {
	GenerationEnabled bool  `json:"generationEnabled"`
	UseEnabled        bool  `json:"useEnabled"`
	Pending           int64 `json:"pending"`
	Failed            int64 `json:"failed"`
	Active            int64 `json:"active"`
	MemoryStateRow
}

func (d *DB) MemoryStatus() (MemoryStatus, error) {
	var s MemoryStatus
	g := d.gorm
	if err := g.Model(&MemoryChangeRow{}).Count(&s.Pending).Error; err != nil {
		return s, err
	}
	if err := g.Model(&MemoryRow{}).Where("status = 'active'").Count(&s.Active).Error; err != nil {
		return s, err
	}
	if err := g.Model(&MemoryChangeRow{}).Where("attempts > 0").Count(&s.Failed).Error; err != nil {
		return s, err
	}
	if err := g.Where("id = 1").Take(&s.MemoryStateRow).Error; err != nil {
		return s, err
	}
	var failed MemoryChangeRow
	err := g.Where("attempts > 0").Order("id").Limit(1).Take(&failed).Error
	if err == nil && failed.LastError != "" {
		s.LastError = failed.LastError
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return s, err
	}
	return s, nil
}

func (d *DB) SetMemoryError(message string) error {
	return d.gorm.Model(&MemoryStateRow{}).Where("id = 1").Update("last_error", message).Error
}

func (d *DB) SetMemorySuccess(now time.Time, report MemoryReport) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return d.gorm.Model(&MemoryStateRow{}).Where("id = 1").Updates(map[string]any{
		"last_success": now.UTC(),
		"last_error":   gorm.Expr("CASE WHEN EXISTS (SELECT 1 FROM memory_changes WHERE attempts > 0) THEN last_error ELSE '' END"),
		"last_report":  string(raw),
	}).Error
}
