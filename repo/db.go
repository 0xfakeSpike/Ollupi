package repo

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type DB struct {
	gorm *gorm.DB
}

func Open(homeDir string) (*DB, error) {
	path := filepath.Join(homeDir, "data", "ollupi.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("storage: create directory of %s: %w", path, err)
	}

	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: "_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on",
	}).String()
	g, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),

		NowFunc:                func() time.Time { return time.Now().UTC() },
		SkipDefaultTransaction: true,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}
	conn, err := g.DB()
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}
	conn.SetMaxOpenConns(1)
	db := &DB{gorm: g}
	if err := db.applySchema(); err != nil {
		err = fmt.Errorf("storage: %s: %w", path, err)
		if cerr := conn.Close(); cerr != nil {
			return nil, fmt.Errorf("%w (close: %v)", err, cerr)
		}
		return nil, err
	}
	return db, nil
}

func (d *DB) Close() error {
	conn, err := d.gorm.DB()
	if err != nil {
		return err
	}
	return conn.Close()
}

func (d *DB) applySchema() error {
	return d.gorm.Transaction(func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(rowTypes...); err != nil {
			return fmt.Errorf("create tables: %w", err)
		}
		var triggers []string
		if err := tx.Raw(`SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'memory_%'`).Scan(&triggers).Error; err != nil {
			return fmt.Errorf("read memory triggers: %w", err)
		}
		for _, name := range triggers {
			if err := tx.Exec(`DROP TRIGGER "` + name + `"`).Error; err != nil {
				return fmt.Errorf("drop trigger %s: %w", name, err)
			}
		}
		if err := tx.Exec(memoryTriggersSQL()).Error; err != nil {
			return fmt.Errorf("create memory triggers: %w", err)
		}
		if err := tx.FirstOrCreate(&MemoryStateRow{}, MemoryStateRow{ID: 1}).Error; err != nil {
			return fmt.Errorf("create memory state: %w", err)
		}
		return nil
	})
}

func (d *DB) Tx(fn func(*DB) error) error {
	return d.gorm.Transaction(func(tx *gorm.DB) error { return fn(&DB{gorm: tx}) })
}

func nullTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func checkOne(res *gorm.DB) error {
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("storage: wrote %d rows, want 1 (an immutable column changed?)", res.RowsAffected)
	}
	return nil
}

var rowTypes = []any{
	&WorkspaceRow{}, &ThreadRow{}, &TurnRow{}, &ItemRow{}, &CronJobRow{},
	&MemoryRow{}, &MemoryEventRow{}, &MemoryStateRow{},
}

var ErrNotFound = errors.New("storage: not found")

func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

func notFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

func where(sql string) clause.Where {
	return clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: sql}}}
}
