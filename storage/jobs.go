package storage

import (
	"database/sql"
	"fmt"

	"github.com/0xfakeSpike/ollupi/cron"
)

// JobsSelectAll returns every cron job in creation order.
func (d *DB) JobsSelectAll() ([]cron.Job, error) {
	rows, err := d.conn.Query(`SELECT id, spec, prompt, created_at, workspace_id FROM cron_jobs ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cron.Job
	for rows.Next() {
		var job cron.Job
		var created string
		var workspaceID sql.NullString
		if err := rows.Scan(&job.ID, &job.Spec, &job.Prompt, &created, &workspaceID); err != nil {
			return nil, err
		}
		job.WorkspaceID = workspaceID.String
		if job.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("storage: cron job %s created_at: %w", job.ID, err)
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// JobInsert stores a new job; it fails if the id already exists.
func (d *DB) JobInsert(job cron.Job) error {
	_, err := d.conn.Exec(`INSERT INTO cron_jobs (id, spec, prompt, created_at, workspace_id) VALUES (?, ?, ?, ?, ?)`,
		job.ID, job.Spec, job.Prompt, formatTime(job.CreatedAt), nullable(job.WorkspaceID))
	return err
}

// JobDelete removes a job and reports whether it existed.
func (d *DB) JobDelete(id string) (bool, error) {
	res, err := d.conn.Exec(`DELETE FROM cron_jobs WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}
