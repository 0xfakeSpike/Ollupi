package storage

import (
	"database/sql"
	"fmt"
	"github.com/0xfakeSpike/ollupi/workspace"
)

func (d *DB) WorkspacesSelectAll() ([]workspace.Workspace, error) {
	rows, err := d.conn.Query(`SELECT id,name,root_path,created_at,updated_at,archived_at FROM workspaces ORDER BY updated_at DESC,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]workspace.Workspace, 0)
	for rows.Next() {
		var w workspace.Workspace
		var created, updated string
		var archived sql.NullString
		if err := rows.Scan(&w.ID, &w.Name, &w.RootPath, &created, &updated, &archived); err != nil {
			return nil, err
		}
		if w.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if w.UpdatedAt, err = parseTime(updated); err != nil {
			return nil, err
		}
		if w.ArchivedAt, err = parseNullableTime(archived); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (d *DB) WorkspaceSave(w workspace.Workspace) error {
	res, err := d.conn.Exec(`INSERT INTO workspaces(id,name,root_path,created_at,updated_at,archived_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(id) DO UPDATE SET name=excluded.name,updated_at=excluded.updated_at,archived_at=excluded.archived_at
 WHERE root_path=excluded.root_path AND created_at=excluded.created_at`, w.ID, w.Name, w.RootPath, formatTime(w.CreatedAt), formatTime(w.UpdatedAt), nullableTime(w.ArchivedAt))
	if err := checkOne(res, err); err != nil {
		return fmt.Errorf("workspace %s: immutable identity or write failure: %w", w.ID, err)
	}
	return nil
}
