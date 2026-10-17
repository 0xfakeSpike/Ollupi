package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0xfakeSpike/ollupi/memory"
	"github.com/0xfakeSpike/ollupi/sqlite"
)

const memoryTestTime = "2026-10-11T00:00:00Z"

func memoryTestDB(t *testing.T) *DB {
	t.Helper()
	db, e := Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
func memoryExec(t *testing.T, d *DB, s string, args ...any) {
	t.Helper()
	if _, e := d.conn.Exec(s, args...); e != nil {
		t.Fatal(e)
	}
}
func memorySeed(t *testing.T, d *DB, scope, id, status string) {
	t.Helper()
	if scope != "" {
		memoryExec(t, d, `INSERT OR IGNORE INTO workspaces(id,name,root_path,created_at,updated_at) VALUES(?,?,?,?,?)`, scope, scope, "/"+scope, memoryTestTime, memoryTestTime)
	}
	memoryExec(t, d, `INSERT INTO threads(id,workspace_id,source,preview,created_at,updated_at) VALUES(?,?,'user','User request',?,?)`, id, nullable(scope), memoryTestTime, memoryTestTime)
	memoryExec(t, d, `INSERT INTO turns(id,thread_id,seq,status,agent,model,approval_policy,input_tokens,cached_input_tokens,output_tokens,started_at) VALUES(?,?,1,?,'agent','model','ask',0,0,0,?)`, id+"-turn", id, status, memoryTestTime)
	memoryExec(t, d, `INSERT INTO items(id,thread_id,turn_id,seq,type,status,body,started_at) VALUES(?,?,?,1,'userMessage','completed','{"text":"Only build macOS"}',?)`, id+"-user", id, id+"-turn", memoryTestTime)
}
func memoryBatch(t *testing.T, d *DB) *memory.Batch {
	t.Helper()
	b, e := d.MemoryNextBatch(context.Background(), 100)
	if e != nil {
		t.Fatal(e)
	}
	if b == nil {
		t.Fatal("missing batch")
	}
	return b
}
func memorySource(t *testing.T, b *memory.Batch, table, id string) memory.Source {
	t.Helper()
	for _, f := range b.Facts {
		if f.Source.Table == table && f.Source.ID == id {
			return f.Source
		}
	}
	t.Fatalf("missing fact %s/%s", table, id)
	return memory.Source{}
}
func memoryAdd(t *testing.T, d *DB, b *memory.Batch, topic string, s memory.Source) memory.Entry {
	t.Helper()
	e := d.MemoryCommit(context.Background(), b, []memory.Operation{{Action: "add", Topic: topic, Kind: "decision", Text: "macOS only", Sources: []memory.Source{s}}}, memory.Report{})
	if e != nil {
		t.Fatal(e)
	}
	entries, e := d.MemoryList(context.Background(), b.WorkspaceID, false)
	if e != nil || len(entries) != 1 {
		t.Fatalf("entries=%+v error=%v", entries, e)
	}
	return entries[0]
}

func TestMemoryMigrationAndBackfill(t *testing.T) {
	for _, version := range []int{2, 3, 4} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			conn, e := sqlite.Open(filepath.Join(t.TempDir(), "old.sqlite"))
			if e != nil {
				t.Fatal(e)
			}
			defer conn.Close()
			old := schema[:strings.Index(schema, "CREATE TABLE memories")]
			if version < 4 {
				old = strings.TrimPrefix(old, workspaceSchema)
				var lines []string
				for _, line := range strings.Split(old, "\n") {
					if strings.Contains(line, "workspace_id") || strings.Contains(line, "execution_context") {
						continue
					}
					if version == 2 && (strings.Contains(line, "model_profile_id") || strings.Contains(line, "model_settings")) {
						continue
					}
					lines = append(lines, line)
				}
				old = strings.Join(lines, "\n")
			}
			if _, e = conn.Exec(old + fmt.Sprintf("PRAGMA user_version=%d;", version)); e != nil {
				t.Fatal(e)
			}
			if _, e = conn.Exec(`INSERT INTO threads(id,source,preview,created_at,updated_at) VALUES('legacy','user','old',?,?)`, memoryTestTime, memoryTestTime); e != nil {
				t.Fatal(e)
			}
			if e = applySchema(conn); e != nil {
				t.Fatal(e)
			}
			var v, n int
			conn.QueryRow(`PRAGMA user_version`).Scan(&v)
			conn.QueryRow(`SELECT count(*) FROM memory_changes WHERE entity_id='legacy'`).Scan(&n)
			if v != 5 || n != 1 {
				t.Fatalf("version=%d backfill=%d", v, n)
			}
			if e = applySchema(conn); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestMemoryTracksAllTablesAndCoalesces(t *testing.T) {
	d := memoryTestDB(t)
	memorySeed(t, d, "w", "t", "completed")
	memoryExec(t, d, `INSERT INTO cron_jobs(id,workspace_id,spec,prompt,created_at) VALUES('j','w','0 * * * *','check project',?)`, memoryTestTime)
	var count int
	d.conn.QueryRow(`SELECT count(*) FROM memory_changes`).Scan(&count)
	if count != 5 {
		t.Fatalf("want all 5 tables, got %d", count)
	}
	for _, sql := range []string{`UPDATE workspaces SET name='Renamed' WHERE id='w'`, `UPDATE threads SET source='cron' WHERE id='t'`, `UPDATE turns SET status='failed',error='failure' WHERE id='t-turn'`, `UPDATE items SET body='{"text":"Windows too"}' WHERE id='t-user'`, `UPDATE cron_jobs SET prompt='new task' WHERE id='j'`} {
		memoryExec(t, d, sql)
	}
	var minVersion int
	d.conn.QueryRow(`SELECT count(*),min(version) FROM memory_changes`).Scan(&count, &minVersion)
	if count != 5 || minVersion != 2 {
		t.Fatalf("count=%d minVersion=%d", count, minVersion)
	}
	b := memoryBatch(t, d)
	if len(b.Changes) != 5 {
		t.Fatalf("receipts=%d", len(b.Changes))
	}
	if e := d.MemoryCommit(context.Background(), b, nil, memory.Report{Model: "test"}); e != nil {
		t.Fatal(e)
	}
	memoryExec(t, d, `DELETE FROM cron_jobs WHERE id='j'`)
	memoryExec(t, d, `DELETE FROM threads WHERE id='t'`)
	memoryExec(t, d, `DELETE FROM workspaces WHERE id='w'`)
	b = memoryBatch(t, d)
	if len(b.Changes) != 5 {
		t.Fatalf("delete receipts=%d facts=%+v", len(b.Changes), b.Facts)
	}
	for _, f := range b.Facts {
		if f.Source.Operation != "delete" || strings.Contains(string(f.Data), "macOS") {
			t.Fatalf("invented deletion evidence: %+v", f)
		}
	}
	if b.WorkspaceID != "w" {
		t.Fatalf("lost cascade scope %q", b.WorkspaceID)
	}
}

func TestMemorySkipsActiveTurnsAndDerivedItems(t *testing.T) {
	d := memoryTestDB(t)
	memorySeed(t, d, "", "active", "inProgress")
	memorySeed(t, d, "w", "done", "completed")
	memoryExec(t, d, `INSERT INTO items(id,thread_id,turn_id,seq,type,status,body,started_at) VALUES('derived','done','done-turn',2,'memoryContext','completed','{"text":"self reinforcement"}',?)`, memoryTestTime)
	for _, kind := range []string{"contextCompaction", "reasoning"} {
		memoryExec(t, d, `UPDATE items SET type=?,body='{"text":"derived"}' WHERE id='derived'`, kind)
	}
	memoryExec(t, d, `UPDATE threads SET updated_at='later' WHERE id='done'`)
	b := memoryBatch(t, d)
	if b.WorkspaceID != "w" {
		t.Fatalf("active scope blocked completed scope: %+v", b)
	}
	for _, f := range b.Facts {
		if strings.Contains(string(f.Data), "self reinforcement") || f.Source.ID == "derived" || f.Source.ID == "active-user" {
			t.Fatalf("unexpected fact %+v", f)
		}
	}
	if e := d.MemoryCommit(context.Background(), b, nil, memory.Report{}); e != nil {
		t.Fatal(e)
	}
	next, e := d.MemoryNextBatch(context.Background(), 100)
	if e != nil || next != nil {
		t.Fatalf("active source should wait: %v %v", next, e)
	}
	memoryExec(t, d, `UPDATE turns SET status='interrupted' WHERE id='active-turn'`)
	if b = memoryBatch(t, d); b.WorkspaceID != "" {
		t.Fatal("wrong general scope")
	}
}

func TestMemoryCommitRetriesChangedEvidenceAtomically(t *testing.T) {
	d := memoryTestDB(t)
	memorySeed(t, d, "w", "t", "completed")
	b := memoryBatch(t, d)
	source := memorySource(t, b, "items", "t-user")
	memoryExec(t, d, `UPDATE items SET body='{"text":"Windows now"}' WHERE id='t-user'`)
	op := memory.Operation{Action: "add", Topic: "platform", Kind: "decision", Text: "macOS", Sources: []memory.Source{source}}
	if e := d.MemoryCommit(context.Background(), b, []memory.Operation{op}, memory.Report{}); !errors.Is(e, memory.ErrConflict) {
		t.Fatalf("want conflict, got %v", e)
	}
	s, _ := d.MemoryStatus(context.Background())
	if s.Pending != 4 || s.Active != 0 || s.LastSuccess != nil {
		t.Fatalf("partial commit %+v", s)
	}
	b = memoryBatch(t, d)
	op.Sources = []memory.Source{memorySource(t, b, "items", "t-user")}
	bad := op
	bad.Topic = "invalid"
	bad.Sources = []memory.Source{{Table: "items", ID: "outside-batch"}}
	if e := d.MemoryCommit(context.Background(), b, []memory.Operation{op, bad}, memory.Report{}); e == nil {
		t.Fatal("accepted unprovided evidence")
	}
	entries, _ := d.MemoryList(context.Background(), "w", false)
	if len(entries) != 0 {
		t.Fatal("invalid operation partially committed")
	}
	if e := d.MemoryCommit(context.Background(), b, []memory.Operation{op}, memory.Report{Model: "extractor"}); e != nil {
		t.Fatal(e)
	}
	s, _ = d.MemoryStatus(context.Background())
	if s.Pending != 0 || s.Active != 1 || s.LastReport.Model != "extractor" {
		t.Fatalf("commit missing %+v", s)
	}
	if e := d.MemoryCommit(context.Background(), b, nil, memory.Report{}); !errors.Is(e, memory.ErrConflict) {
		t.Fatalf("replayed acknowledgement %v", e)
	}
}

func TestMemoryEditForgetRaceAndSuppression(t *testing.T) {
	ctx := context.Background()
	d := memoryTestDB(t)
	memorySeed(t, d, "w", "t", "completed")
	b := memoryBatch(t, d)
	entry := memoryAdd(t, d, b, "platform", memorySource(t, b, "items", "t-user"))
	memoryExec(t, d, `UPDATE items SET body='{"text":"Changed request"}' WHERE id='t-user'`)
	b = memoryBatch(t, d)
	edited, e := d.MemoryEdit(ctx, entry.ID, entry.Revision, "User corrected this")
	if e != nil {
		t.Fatal(e)
	}
	op := memory.Operation{Action: "update", ID: entry.ID, Revision: entry.Revision, Text: "stale rewrite", Sources: entry.Sources}
	if e = d.MemoryCommit(ctx, b, []memory.Operation{op}, memory.Report{}); !errors.Is(e, memory.ErrConflict) {
		t.Fatalf("edit raced %v", e)
	}
	b = memoryBatch(t, d)
	if e = d.MemoryForget(ctx, edited.ID, edited.Revision); e != nil {
		t.Fatal(e)
	}
	if e = d.MemoryCommit(ctx, b, nil, memory.Report{}); !errors.Is(e, memory.ErrConflict) {
		t.Fatalf("forget raced %v", e)
	}
	memoryExec(t, d, `UPDATE items SET body='{"text":"Only build macOS"}' WHERE id='t-user'`)
	b = memoryBatch(t, d)
	s := memorySource(t, b, "items", "t-user")
	if e = d.MemoryCommit(ctx, b, []memory.Operation{{Action: "add", Topic: "platform", Kind: "decision", Text: "revive", Sources: []memory.Source{s}}, {Action: "add", Topic: "different-topic", Kind: "decision", Text: "revive", Sources: []memory.Source{s}}}, memory.Report{}); e != nil {
		t.Fatal(e)
	}
	entries, e := d.MemoryList(ctx, "w", false)
	if e != nil || len(entries) != 1 || entries[0].Status != "forgotten" || entries[0].Text != "" {
		t.Fatalf("forgotten memory revived %+v %v", entries, e)
	}
	var oldText int
	d.conn.QueryRow(`SELECT count(*) FROM memory_revisions WHERE snapshot LIKE '%macOS%' OR snapshot LIKE '%User corrected%'`).Scan(&oldText)
	if oldText != 0 {
		t.Fatal("forget retained memory prose")
	}
}

func TestMemoryScopeAndSourceDeletion(t *testing.T) {
	ctx := context.Background()
	d := memoryTestDB(t)
	memorySeed(t, d, "", "global", "completed")
	b := memoryBatch(t, d)
	memoryAdd(t, d, b, "preference", memorySource(t, b, "items", "global-user"))
	memorySeed(t, d, "w", "t", "completed")
	b = memoryBatch(t, d)
	e := memoryAdd(t, d, b, "platform", memorySource(t, b, "items", "t-user"))
	own, _ := d.MemoryList(ctx, "w", false)
	both, _ := d.MemoryList(ctx, "w", true)
	other, _ := d.MemoryList(ctx, "other", false)
	if len(own) != 1 || len(both) != 2 || len(other) != 0 {
		t.Fatalf("scope leak %d/%d/%d", len(own), len(both), len(other))
	}
	memoryExec(t, d, `DELETE FROM items WHERE id='t-user'`)
	b = memoryBatch(t, d)
	if err := d.MemoryCommit(ctx, b, nil, memory.Report{}); err != nil {
		t.Fatal(err)
	}
	own, _ = d.MemoryList(ctx, "w", false)
	if own[0].ID != e.ID || own[0].Status != "retired" {
		t.Fatal("deleted evidence remained active")
	}
}

func TestMemoryEvidenceNormalization(t *testing.T) {
	d := memoryTestDB(t)
	memorySeed(t, d, "w", "t", "failed")
	body, _ := json.Marshal(map[string]any{"text": strings.Repeat("文", 4000), "contextStats": map[string]any{"systemTokens": 22}, "usage": map[string]any{"inputTokens": 4}, "preparedExecution": map[string]any{"secret": "instruction"}, "responseId": "provider-id"})
	memoryExec(t, d, `UPDATE items SET body=? WHERE id='t-user'`, string(body))
	b := memoryBatch(t, d)
	for _, f := range b.Facts {
		if f.Source.OccurredAt != memoryTestTime {
			t.Fatalf("missing source timestamp %+v", f.Source)
		}
		data := string(f.Data)
		for _, forbidden := range []string{"contextStats", "inputTokens", "preparedExecution", "provider-id"} {
			if strings.Contains(data, forbidden) {
				t.Fatalf("leaked %s", forbidden)
			}
		}
		if f.Source.ID == "t-user" && !strings.Contains(data, "truncated") {
			t.Fatal("silent truncation")
		}
	}
	// Telemetry-only writes must not cause another extractor request.
	if e := d.MemoryCommit(context.Background(), b, nil, memory.Report{}); e != nil {
		t.Fatal(e)
	}
	memoryExec(t, d, `UPDATE items SET body=json_set(body,'$.usage.inputTokens',99) WHERE id='t-user'`)
	next, e := d.MemoryNextBatch(context.Background(), 10)
	if e != nil || next != nil {
		t.Fatalf("telemetry caused extraction %v %v", next, e)
	}
}

func TestMemoryForgetKeepsUnrelatedWorkspaceEvidenceUsable(t *testing.T) {
	ctx := context.Background()
	d := memoryTestDB(t)
	memorySeed(t, d, "w", "t", "completed")
	b := memoryBatch(t, d)
	sources := []memory.Source{memorySource(t, b, "items", "t-user"), memorySource(t, b, "threads", "t"), memorySource(t, b, "workspaces", "w")}
	if err := d.MemoryCommit(ctx, b, []memory.Operation{{Action: "add", Topic: "platform", Kind: "decision", Text: "macOS", Sources: sources}}, memory.Report{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := d.MemoryList(ctx, "w", false)
	if err := d.MemoryForget(ctx, entries[0].ID, entries[0].Revision); err != nil {
		t.Fatal(err)
	}
	memoryExec(t, d, `UPDATE workspaces SET name='Renamed' WHERE id='w'`)
	b = memoryBatch(t, d)
	if err := d.MemoryCommit(ctx, b, []memory.Operation{{Action: "add", Topic: "project-name", Kind: "fact", Text: "Renamed", Sources: []memory.Source{memorySource(t, b, "workspaces", "w")}}}, memory.Report{}); err != nil {
		t.Fatal(err)
	}
	entries, _ = d.MemoryList(ctx, "w", false)
	if len(entries) != 2 {
		t.Fatalf("forget blocked unrelated project evidence %+v", entries)
	}
	// An actual revision of the forgotten source is new evidence, while a replay
	// of the old digest remains suppressed.
	memoryExec(t, d, `UPDATE items SET body='{"text":"The current supported platform is Windows"}' WHERE id='t-user'`)
	b = memoryBatch(t, d)
	if err := d.MemoryCommit(ctx, b, []memory.Operation{{Action: "add", Topic: "new-platform-decision", Kind: "decision", Text: "Windows", Sources: []memory.Source{memorySource(t, b, "items", "t-user")}}}, memory.Report{}); err != nil {
		t.Fatal(err)
	}
	entries, _ = d.MemoryList(ctx, "w", false)
	if len(entries) != 3 {
		t.Fatalf("fresh evidence was suppressed %+v", entries)
	}
}

func TestMemoryItemFactHasTotalBudget(t *testing.T) {
	d := memoryTestDB(t)
	memorySeed(t, d, "w", "t", "completed")
	huge := strings.Repeat("<中文>\\\"", 3000)
	body, _ := json.Marshal(map[string]any{"text": huge, "arguments": huge, "result": map[string]any{"content": huge}, "questions": []string{huge}, "answers": map[string]any{"answer": huge}})
	memoryExec(t, d, `INSERT INTO items(id,thread_id,turn_id,seq,type,status,body,started_at) VALUES('tool','t','t-turn',2,'mcpToolCall','completed',?,?)`, string(body), memoryTestTime)
	fact, err := memoryFact(context.Background(), d.conn, "items", "tool", "upsert", "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(string(fact.Data))); n > 4500 {
		t.Fatalf("unbounded fact %d runes", n)
	}
	if !strings.Contains(string(fact.Data), "truncated") {
		t.Fatal("missing truncation marker")
	}
}

func TestMemoryManualEditSurvivesLaterExtractionAndSourceDeletion(t *testing.T) {
	ctx := context.Background()
	d := memoryTestDB(t)
	memorySeed(t, d, "w", "t", "completed")
	b := memoryBatch(t, d)
	entry := memoryAdd(t, d, b, "platform", memorySource(t, b, "items", "t-user"))
	entry, err := d.MemoryEdit(ctx, entry.ID, entry.Revision, "The user-maintained truth")
	if err != nil {
		t.Fatal(err)
	}
	memoryExec(t, d, `UPDATE items SET body='{"text":"old model belief"}' WHERE id='t-user'`)
	b = memoryBatch(t, d)
	if err = d.MemoryCommit(ctx, b, []memory.Operation{{Action: "update", ID: entry.ID, Revision: entry.Revision, Text: "automatic overwrite", Sources: []memory.Source{memorySource(t, b, "items", "t-user")}}}, memory.Report{}); err != nil {
		t.Fatal(err)
	}
	memoryExec(t, d, `DELETE FROM items WHERE id='t-user'`)
	b = memoryBatch(t, d)
	if err = d.MemoryCommit(ctx, b, nil, memory.Report{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := d.MemoryList(ctx, "w", false)
	if len(entries) != 1 || !entries[0].UserEdited || entries[0].Status != "active" || entries[0].Text != entry.Text || entries[0].Revision != entry.Revision {
		t.Fatalf("manual correction lost %+v", entries)
	}
}

func TestMemoryDefersOnlyFailedSnapshotAndMakesOtherScopesAvailable(t *testing.T) {
	ctx := context.Background()
	d := memoryTestDB(t)
	memorySeed(t, d, "w1", "t1", "completed")
	memorySeed(t, d, "w2", "t2", "completed")
	failed := memoryBatch(t, d)
	if failed.WorkspaceID != "w1" {
		t.Fatal("wrong first scope")
	}
	if err := d.MemoryDefer(ctx, failed, "extractor unavailable"); err != nil {
		t.Fatal(err)
	}
	next := memoryBatch(t, d)
	if next.WorkspaceID != "w2" {
		t.Fatalf("failed scope starved another scope %+v", next)
	}
	if err := d.MemoryCommit(ctx, next, nil, memory.Report{}); err != nil {
		t.Fatal(err)
	}
	status, _ := d.MemoryStatus(ctx)
	if status.Failed != 4 || status.LastError != "extractor unavailable" {
		t.Fatalf("unrelated success hid failure %+v", status)
	}
	// New evidence clears its own delay immediately, without clearing delays on
	// unrelated receipts in the same scope.
	memoryExec(t, d, `UPDATE items SET body='{"text":"fresh evidence"}' WHERE id='t1-user'`)
	next = memoryBatch(t, d)
	if len(next.Changes) != 1 {
		t.Fatalf("wrong reset receipts %+v", next.Changes)
	}
	if err := d.MemoryDefer(ctx, failed, "stale failure"); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var last string
	d.conn.QueryRow(`SELECT attempts,last_error FROM memory_changes WHERE entity_id='t1-user'`).Scan(&attempts, &last)
	if attempts != 0 || last != "" {
		t.Fatal("old extraction deferred a newer item version")
	}
	if err := d.MemoryCommit(ctx, next, nil, memory.Report{}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryMergeCanKeepOtherExistingEvidence(t *testing.T) {
	ctx := context.Background()
	d := memoryTestDB(t)
	memorySeed(t, d, "w", "t", "completed")
	b := memoryBatch(t, d)
	user := memorySource(t, b, "items", "t-user")
	workspace := memorySource(t, b, "workspaces", "w")
	if err := d.MemoryCommit(ctx, b, []memory.Operation{{Action: "add", Topic: "a", Kind: "decision", Text: "a", Sources: []memory.Source{user}}, {Action: "add", Topic: "b", Kind: "fact", Text: "b", Sources: []memory.Source{workspace}}}, memory.Report{}); err != nil {
		t.Fatal(err)
	}
	memoryExec(t, d, `INSERT INTO cron_jobs(id,workspace_id,spec,prompt,created_at) VALUES('job','w','0 * * * *','task',?)`, memoryTestTime)
	b = memoryBatch(t, d)
	var a memory.Entry
	for _, entry := range b.Existing {
		if entry.Topic == "a" {
			a = entry
		}
	}
	if err := d.MemoryCommit(ctx, b, []memory.Operation{{Action: "update", ID: a.ID, Revision: a.Revision, Text: "a and b", Sources: []memory.Source{user, workspace}}}, memory.Report{}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryKeepsFreshDigestWhenOldSourceListedFirst(t *testing.T) {
	ctx := context.Background()
	d := memoryTestDB(t)
	memorySeed(t, d, "w", "t", "completed")
	b := memoryBatch(t, d)
	old := memorySource(t, b, "workspaces", "w")
	entry := memoryAdd(t, d, b, "project-name", old)
	memoryExec(t, d, `UPDATE workspaces SET name='New project name' WHERE id='w'`)
	b = memoryBatch(t, d)
	fresh := memorySource(t, b, "workspaces", "w")
	if old.Digest == fresh.Digest {
		t.Fatal("fixture requires changed digest")
	}
	err := d.MemoryCommit(ctx, b, []memory.Operation{{Action: "update", ID: entry.ID, Revision: entry.Revision, Text: "New project name", Sources: []memory.Source{old, fresh}}}, memory.Report{})
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := d.MemoryList(ctx, "w", false)
	for _, s := range entries[0].Sources {
		if s.Digest == fresh.Digest {
			return
		}
	}
	t.Fatalf("fresh evidence dropped: got %+v, fresh=%+v", entries[0].Sources, fresh)
}
func TestMemoryDeletedEvidenceCannotReactivateLater(t *testing.T) {
	ctx := context.Background()
	d := memoryTestDB(t)
	memorySeed(t, d, "w", "t", "completed")
	b := memoryBatch(t, d)
	old := memorySource(t, b, "items", "t-user")
	entry := memoryAdd(t, d, b, "platform", old)
	memoryExec(t, d, `DELETE FROM items WHERE id='t-user'`)
	b = memoryBatch(t, d)
	if err := d.MemoryCommit(ctx, b, nil, memory.Report{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := d.MemoryList(ctx, "w", false)
	if entries[0].Status != "retired" {
		t.Fatal("fixture must retire")
	}
	entry = entries[0]
	memoryExec(t, d, `UPDATE workspaces SET name='Unrelated new name' WHERE id='w'`)
	b = memoryBatch(t, d)
	err := d.MemoryCommit(ctx, b, []memory.Operation{{Action: "update", ID: entry.ID, Revision: entry.Revision, Text: "macOS only", Sources: []memory.Source{old, memorySource(t, b, "workspaces", "w")}}}, memory.Report{})
	if err != nil {
		return
	}
	entries, _ = d.MemoryList(ctx, "w", false)
	if entries[0].Status == "active" {
		t.Fatalf("deleted source reactivated through later unrelated batch: %+v", entries[0])
	}
}
