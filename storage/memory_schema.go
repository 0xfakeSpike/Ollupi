package storage

import (
	"fmt"
	"strings"
)

// The queue coalesces repeated writes to a row while a turn streams. Its receipt
// version changes on every meaningful update, so acknowledging an old snapshot
// cannot discard a newer change. Evidence stays in the business tables.
func memorySchemaSQL() string {
	var b strings.Builder
	b.WriteString(`
CREATE TABLE memories (
 id TEXT PRIMARY KEY NOT NULL,
 workspace_id TEXT NOT NULL DEFAULT '',
 topic TEXT NOT NULL,
 kind TEXT NOT NULL,
 text TEXT NOT NULL,
 status TEXT NOT NULL CHECK(status IN ('active','retired','forgotten')),
 revision INTEGER NOT NULL CHECK(revision > 0),
 user_edited INTEGER NOT NULL DEFAULT 0 CHECK(user_edited IN (0,1)),
 sources TEXT NOT NULL CHECK(json_valid(sources)),
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE(workspace_id,topic)
);
CREATE INDEX memories_scope_status ON memories(workspace_id,status);
CREATE TABLE memory_revisions (
 memory_id TEXT NOT NULL REFERENCES memories(id),
 revision INTEGER NOT NULL,
 operation TEXT NOT NULL,
 snapshot TEXT NOT NULL CHECK(json_valid(snapshot)),
 created_at TEXT NOT NULL,
 PRIMARY KEY(memory_id,revision)
);
CREATE TABLE memory_changes (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 entity_table TEXT NOT NULL,
 entity_id TEXT NOT NULL,
 workspace_id TEXT NOT NULL DEFAULT '',
 thread_id TEXT NOT NULL DEFAULT '',
 turn_id TEXT NOT NULL DEFAULT '',
 operation TEXT NOT NULL DEFAULT 'upsert',
 version INTEGER NOT NULL DEFAULT 1,
 metadata TEXT NOT NULL DEFAULT '{}',
 attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '',
 UNIQUE(entity_table,entity_id)
);
CREATE INDEX memory_changes_scope ON memory_changes(workspace_id,id);
CREATE TABLE memory_state (
 id INTEGER PRIMARY KEY CHECK(id=1),
 last_success TEXT,
 last_error TEXT NOT NULL DEFAULT '',
 last_report TEXT CHECK(last_report IS NULL OR json_valid(last_report))
);
INSERT INTO memory_state(id) VALUES(1);
`)
	type tracked struct{ table, scope, thread, turn, meta, update string }
	tables := []tracked{
		{"workspaces", "r.id", "''", "''", "'{}'", "NEW.name IS NOT OLD.name OR NEW.root_path IS NOT OLD.root_path OR NEW.archived_at IS NOT OLD.archived_at"},
		{"threads", "r.workspace_id", "r.id", "''", "'{}'", "NEW.source IS NOT OLD.source OR NEW.workspace_id IS NOT OLD.workspace_id OR NEW.parent_thread_id IS NOT OLD.parent_thread_id OR NEW.cron_job_id IS NOT OLD.cron_job_id"},
		{"turns", "(SELECT workspace_id FROM threads WHERE id=r.thread_id)", "r.thread_id", "r.id", "'{}'", "NEW.status IS NOT OLD.status OR NEW.error IS NOT OLD.error OR NEW.completed_at IS NOT OLD.completed_at"},
		{"items", "(SELECT workspace_id FROM threads WHERE id=r.thread_id)", "r.thread_id", "r.turn_id", "json_object('type',r.type)", "NEW.type IS NOT OLD.type OR NEW.status IS NOT OLD.status OR json_remove(NEW.body,'$.contextStats','$.usage','$.responseId','$.preparedExecution') IS NOT json_remove(OLD.body,'$.contextStats','$.usage','$.responseId','$.preparedExecution')"},
		{"cron_jobs", "r.workspace_id", "''", "''", "'{}'", "NEW.spec IS NOT OLD.spec OR NEW.prompt IS NOT OLD.prompt OR NEW.workspace_id IS NOT OLD.workspace_id"},
	}
	for _, t := range tables {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			row, timing := "NEW", "AFTER"
			if op == "DELETE" {
				row, timing = "OLD", "BEFORE"
			}
			replace := func(s string) string { return strings.ReplaceAll(s, "r.", row+".") }
			condition := "1"
			if t.table == "items" {
				condition = row + ".type NOT IN ('reasoning','contextCompaction','memoryContext')"
			}
			if op == "UPDATE" {
				condition += " AND (" + t.update + ")"
			}
			// A parent deletion may already have queued its children. Preserve that
			// scope when a subsequent cascading delete cannot find the parent row.
			scope := fmt.Sprintf("COALESCE(%s,(SELECT workspace_id FROM memory_changes WHERE entity_table='%s' AND entity_id=%s.id),'')", replace(t.scope), t.table, row)
			operation := "upsert"
			if op == "DELETE" {
				operation = "delete"
			}
			fmt.Fprintf(&b, `CREATE TRIGGER memory_%s_%s %s %s ON %s WHEN %s BEGIN
INSERT INTO memory_changes(entity_table,entity_id,workspace_id,thread_id,turn_id,operation,metadata)
VALUES('%s',%s.id,%s,%s,%s,'%s',%s)
ON CONFLICT(entity_table,entity_id) DO UPDATE SET workspace_id=excluded.workspace_id,thread_id=excluded.thread_id,turn_id=excluded.turn_id,operation=excluded.operation,metadata=excluded.metadata,version=memory_changes.version+1,attempts=0,next_attempt=0,last_error='';
END;
`, t.table, strings.ToLower(op), timing, op, t.table, condition, t.table, row, scope, replace(t.thread), replace(t.turn), operation, replace(t.meta))
		}
	}
	// Capture scope before SQLite removes a parent and cascades into its children.
	b.WriteString(`
CREATE TRIGGER memory_thread_children BEFORE DELETE ON threads BEGIN
INSERT INTO memory_changes(entity_table,entity_id,workspace_id,thread_id,turn_id,operation,metadata)
SELECT 'turns',id,COALESCE(OLD.workspace_id,''),thread_id,id,'delete','{}' FROM turns WHERE thread_id=OLD.id
ON CONFLICT(entity_table,entity_id) DO UPDATE SET workspace_id=excluded.workspace_id,operation='delete',version=memory_changes.version+1,attempts=0,next_attempt=0,last_error='';
INSERT INTO memory_changes(entity_table,entity_id,workspace_id,thread_id,turn_id,operation,metadata)
SELECT 'items',id,COALESCE(OLD.workspace_id,''),thread_id,turn_id,'delete',json_object('type',type) FROM items WHERE thread_id=OLD.id AND type NOT IN ('reasoning','contextCompaction','memoryContext')
ON CONFLICT(entity_table,entity_id) DO UPDATE SET workspace_id=excluded.workspace_id,operation='delete',version=memory_changes.version+1,attempts=0,next_attempt=0,last_error='';
END;
`)
	return b.String()
}

const memoryBackfillSQL = `
INSERT INTO memory_changes(entity_table,entity_id,workspace_id) SELECT 'workspaces',id,id FROM workspaces;
INSERT INTO memory_changes(entity_table,entity_id,workspace_id,thread_id) SELECT 'threads',id,COALESCE(workspace_id,''),id FROM threads;
INSERT INTO memory_changes(entity_table,entity_id,workspace_id,thread_id,turn_id) SELECT 'turns',v.id,COALESCE(t.workspace_id,''),v.thread_id,v.id FROM turns v JOIN threads t ON t.id=v.thread_id;
INSERT INTO memory_changes(entity_table,entity_id,workspace_id,thread_id,turn_id,metadata) SELECT 'items',i.id,COALESCE(t.workspace_id,''),i.thread_id,i.turn_id,json_object('type',i.type) FROM items i JOIN threads t ON t.id=i.thread_id WHERE i.type NOT IN ('reasoning','contextCompaction','memoryContext');
INSERT INTO memory_changes(entity_table,entity_id,workspace_id) SELECT 'cron_jobs',id,COALESCE(workspace_id,'') FROM cron_jobs;
`
