package repo

import (
	"time"
)

type MemoryRevisionRow struct {
	MemoryID  string     `gorm:"primaryKey"`
	Memory    *MemoryRow `gorm:"foreignKey:MemoryID"`
	Revision  int        `gorm:"primaryKey;autoIncrement:false"`
	Operation string     `gorm:"not null"`
	Snapshot  string     `gorm:"not null;check:memory_revisions_snapshot,json_valid(snapshot)"`
	CreatedAt time.Time  `gorm:"not null;autoCreateTime:false"`
}

func (MemoryRevisionRow) TableName() string { return "memory_revisions" }

func (d *DB) DeleteMemoryRevisions(id string) error {
	return d.gorm.Where("memory_id = ?", id).Delete(&MemoryRevisionRow{}).Error
}
