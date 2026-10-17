// Package sqlite opens SQLite databases with the settings a long-running
// service needs, and versions their schema with PRAGMA user_version. It knows
// nothing about what is stored.
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

// Open opens, creating if needed, the SQLite file at path: WAL journal, a
// 5 s busy timeout, foreign keys on, and one connection, so writes are
// serialized in the process.
func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("sqlite: create directory of %s: %w", path, err)
	}
	// Build the DSN as a URL so '#', '?', and '%' in the path are escaped.
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: "_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on",
	}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, closeWith(db, fmt.Errorf("sqlite: open %s: %w", path, err))
	}
	return db, nil
}

// ApplySchema creates schema in an empty database and records version in
// PRAGMA user_version. A database that already has version is left as it
// is. Anything else (another version, or tables without a version) is an
// error: there are no migrations, so such a database must be replaced.
func ApplySchema(db *sql.DB, version int, schema string) error {
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("sqlite: read schema version: %w", err)
	}
	if current == version {
		return nil
	}
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
		return fmt.Errorf("sqlite: inspect schema: %w", err)
	}
	if current != 0 || tables != 0 {
		return fmt.Errorf("sqlite: the database has schema version %d, but version %d is required", current, version)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("sqlite: create schema: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		return fmt.Errorf("sqlite: set schema version: %w", err)
	}
	return tx.Commit()
}

func closeWith(db *sql.DB, cause error) error {
	if err := db.Close(); err != nil {
		return errors.Join(cause, fmt.Errorf("sqlite: close: %w", err))
	}
	return cause
}
