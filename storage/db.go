// Package storage stores the assistant's data (threads, turns, items, and
// cron jobs) in the workspace SQLite database. It implements
// thread.Persistence and cron.Store on top of pkg/sqlite.
package storage

import (
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/0xfakeSpike/ollupi/sqlite"
)

// DB is the workspace database.
type DB struct {
	conn *sql.DB
}

// Open opens workspaceRoot/data/ollupi.sqlite, creating the schema in a
// new file or migrating an existing supported version in place.
func Open(workspaceRoot string) (*DB, error) {
	path := filepath.Join(workspaceRoot, "data", "ollupi.sqlite")
	conn, err := sqlite.Open(path)
	if err != nil {
		return nil, err
	}
	if err := applySchema(conn); err != nil {
		err = fmt.Errorf("storage: %s: %w", path, err)
		if cerr := conn.Close(); cerr != nil {
			return nil, fmt.Errorf("%w (close: %v)", err, cerr)
		}
		return nil, err
	}
	return &DB{conn: conn}, nil
}

// Close closes the database.
func (d *DB) Close() error {
	return d.conn.Close()
}

// schemaVersion is the schema this code reads and writes. Versions 2 through 4
// upgrade without inventing execution history; unknown versions are refused.
const schemaVersion = 5

func applySchema(conn *sql.DB) error {
	var current int
	if err := conn.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if current != 2 && current != 3 && current != 4 {
		return sqlite.ApplySchema(conn, schemaVersion, schema)
	}
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if current == 2 {
		if _, err := tx.Exec(`ALTER TABLE turns ADD COLUMN model_profile_id TEXT NOT NULL DEFAULT 'primary';
            ALTER TABLE turns ADD COLUMN model_settings TEXT NOT NULL DEFAULT '{}';`); err != nil {
			return fmt.Errorf("migrate schema version 2 to 3: %w", err)
		}
	}
	if current <= 3 {
		if _, err := tx.Exec(workspaceSchema + `
        ALTER TABLE threads ADD COLUMN workspace_id TEXT REFERENCES workspaces(id) ON DELETE RESTRICT;
        CREATE INDEX threads_workspace_updated ON threads(workspace_id, updated_at DESC);
        ALTER TABLE cron_jobs ADD COLUMN workspace_id TEXT REFERENCES workspaces(id) ON DELETE RESTRICT;
        CREATE INDEX cron_jobs_workspace ON cron_jobs(workspace_id);
        ALTER TABLE turns ADD COLUMN execution_context TEXT CHECK(execution_context IS NULL OR json_valid(execution_context));
        PRAGMA user_version = 4;`); err != nil {
			return fmt.Errorf("migrate schema version 3 to 4: %w", err)
		}
	}
	if _, err := tx.Exec(memorySchemaSQL() + memoryBackfillSQL + `PRAGMA user_version = 5;`); err != nil {
		return fmt.Errorf("migrate schema version 4 to 5: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema migration: %w", err)
	}
	return nil
}

// workspaceSchema is shared by the fresh schema and the version 4 migration.
const workspaceSchema = `
CREATE TABLE workspaces (
 id TEXT PRIMARY KEY NOT NULL,
 name TEXT NOT NULL CHECK(length(trim(name)) > 0),
 root_path TEXT NOT NULL UNIQUE,
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 archived_at TEXT
);
CREATE INDEX workspaces_archived_updated ON workspaces(archived_at, updated_at DESC);
`

// schema creates every table. Times are RFC 3339 UTC text; item body holds
// type-specific JSON, including prepared tool executions and approvals.
var schema = workspaceSchema + `
CREATE TABLE threads (
	id              TEXT PRIMARY KEY,
	source          TEXT NOT NULL,
	workspace_id TEXT REFERENCES workspaces(id) ON DELETE RESTRICT,
	cron_job_id     TEXT,
	approval_policy TEXT,
	parent_thread_id TEXT REFERENCES threads (id) ON DELETE CASCADE,
	parent_item_id  TEXT,
	agent           TEXT,
	preview         TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
CREATE INDEX threads_cron_job_id ON threads (cron_job_id);
CREATE INDEX threads_parent_thread_id ON threads (parent_thread_id);
CREATE INDEX threads_updated_at ON threads (updated_at);
CREATE INDEX threads_workspace_updated ON threads(workspace_id, updated_at DESC);

CREATE TABLE turns (
	id                  TEXT PRIMARY KEY,
	thread_id           TEXT NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
	seq                 INTEGER NOT NULL,
	status              TEXT NOT NULL,
	agent               TEXT NOT NULL,
	model               TEXT NOT NULL,
	model_profile_id    TEXT NOT NULL DEFAULT 'primary',
	model_settings      TEXT NOT NULL DEFAULT '{}',
	execution_context TEXT CHECK(execution_context IS NULL OR json_valid(execution_context)),
	approval_policy     TEXT NOT NULL,
	input_tokens        INTEGER NOT NULL,
	cached_input_tokens INTEGER NOT NULL,
	output_tokens       INTEGER NOT NULL,
	error               TEXT,
	started_at          TEXT NOT NULL,
	completed_at        TEXT,
	UNIQUE (thread_id, seq)
);

CREATE TABLE items (
	id           TEXT PRIMARY KEY,
	thread_id    TEXT NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
	turn_id      TEXT NOT NULL REFERENCES turns (id) ON DELETE CASCADE,
	seq          INTEGER NOT NULL,
	type         TEXT NOT NULL,
	status       TEXT NOT NULL,
	body         TEXT NOT NULL,
	started_at   TEXT NOT NULL,
	completed_at TEXT,
	UNIQUE (thread_id, seq)
);
CREATE INDEX items_turn_id ON items (turn_id);

CREATE TABLE cron_jobs (
	id         TEXT PRIMARY KEY,
	spec       TEXT NOT NULL,
	workspace_id TEXT REFERENCES workspaces(id) ON DELETE RESTRICT,
	prompt     TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX cron_jobs_workspace ON cron_jobs(workspace_id);
` + memorySchemaSQL()
