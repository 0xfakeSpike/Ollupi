package memory

import (
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

func openTestDB(t *testing.T) *repo.DB {
	t.Helper()
	db, err := repo.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testManager(t *testing.T, db *repo.DB) *Manager {
	t.Helper()
	m, err := New(db, nil, Options{Use: true})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	return m
}

func TestInProgressWorkProducesNoEvents(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	wsID := uuid.NewString()
	ws, err := db.AddWorkspace(repo.WorkspaceRow{
		ID: wsID, Name: "proj", RootPath: t.TempDir(), CreatedAt: now, UpdatedAt: now,
	}, func(repo.WorkspaceRow) bool { return false }, nil)
	if err != nil {
		t.Fatalf("add workspace: %v", err)
	}
	threadID, turnID := uuid.NewString(), uuid.NewString()
	if err := db.CreateThread(repo.ThreadRow{ID: threadID, WorkspaceID: ws.ID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := db.CreateTurn(repo.TurnRow{
		ID: turnID, ThreadID: threadID, Seq: 1, Status: repo.TurnInProgress,
		Model: llm.Model{Name: "test"}, StartedAt: now,
	}); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	save := func(it repo.ItemRow) {
		t.Helper()
		it.ThreadID, it.TurnID = threadID, turnID
		if err := db.SaveItem(it); err != nil {
			t.Fatalf("save item: %v", err)
		}
	}
	firstItem := repo.ItemRow{ID: uuid.NewString(), ThreadID: threadID, TurnID: turnID, Seq: 1, Type: repo.ItemUserMessage, Status: repo.ItemCompleted, Text: "hello", StartedAt: now, CompletedAt: &now}
	save(firstItem)
	save(repo.ItemRow{ID: uuid.NewString(), Seq: 2, Type: repo.ItemAgentMessage, Status: repo.ItemCompleted, Text: "hi", StartedAt: now, CompletedAt: &now})

	scope, ok, err := db.NextEventScope(time.Now().UnixMilli())
	if err != nil || !ok {
		t.Fatalf("expected workspace/thread events (ok=%v err=%v)", ok, err)
	}
	events, err := db.QueuedEvents(scope, time.Now().UnixMilli(), 10)
	if err != nil {
		t.Fatalf("read queued events: %v", err)
	}
	for _, ev := range events {
		if ev.SourceTable == "turns" {
			t.Fatalf("in-progress turn produced an event: %+v", ev)
		}
		if err := db.AckEvent(ev.ID); err != nil {
			t.Fatalf("ack event: %v", err)
		}
	}

	turn, err := db.ActiveTurn(threadID)
	if err != nil {
		t.Fatalf("read turn: %v", err)
	}
	turn.Status = repo.TurnCompleted
	turn.CompletedAt = &now
	if err := db.UpdateTurn(turn); err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	scope, ok, err = db.NextEventScope(time.Now().UnixMilli())
	if err != nil || !ok || scope != ws.ID {
		t.Fatalf("terminal turn event missing (scope=%q ok=%v err=%v)", scope, ok, err)
	}
	events, err = db.QueuedEvents(scope, time.Now().UnixMilli(), 10)
	if err != nil || len(events) != 3 {
		t.Fatalf("unexpected events: %+v err=%v", events, err)
	}
	tables := map[string]int{}
	for _, ev := range events {
		tables[ev.SourceTable]++
	}
	if tables["turns"] != 1 || tables["items"] != 2 {
		t.Fatalf("expected one turn event and two item events: %+v", events)
	}

	// Streaming edits after completion must not queue new events.
	firstItem.Text = "edited"
	if err := db.SaveItem(firstItem); err != nil {
		t.Fatalf("edit item: %v", err)
	}
	events, err = db.QueuedEvents(scope, time.Now().UnixMilli(), 10)
	if err != nil || len(events) != 3 {
		t.Fatalf("item edit queued an event: %+v err=%v", events, err)
	}
}

func TestContextEditForget(t *testing.T) {
	db := openTestDB(t)
	m := testManager(t, db)
	now := time.Now().UTC()
	entry := repo.MemoryRow{
		ID: uuid.NewString(), WorkspaceID: "", Topic: "Go preference", Kind: "fact",
		Text: "The user prefers Go.", Status: "active", Sources: []repo.MemorySource{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.SaveMemory(entry); err != nil {
		t.Fatalf("save memory: %v", err)
	}
	ctx, err := m.Context("any-workspace", "Go style", 100000)
	if err != nil || len(ctx.Entries) != 1 {
		t.Fatalf("recall failed: entries=%d err=%v", len(ctx.Entries), err)
	}
	if other, err := m.Context("any-workspace", "unrelated topic", 100000); err != nil || len(other.Entries) != 0 {
		t.Fatalf("expected no recall for unrelated query: entries=%d err=%v", len(other.Entries), err)
	}
	edited, err := m.Edit(entry.ID, "The user prefers Rust.")
	if err != nil || edited.UserEdited != true {
		t.Fatalf("edit memory: row=%+v err=%v", edited, err)
	}
	if err := m.Forget(entry.ID); err != nil {
		t.Fatalf("forget memory: %v", err)
	}
	if ctx, err = m.Context("any-workspace", "Go style", 100000); err != nil || len(ctx.Entries) != 0 {
		t.Fatalf("forgotten memory recalled: entries=%d err=%v", len(ctx.Entries), err)
	}
}

func TestBatchExtractionCommit(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC()
	threadID, turnID := uuid.NewString(), uuid.NewString()
	if err := db.CreateThread(repo.ThreadRow{ID: threadID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := db.CreateTurn(repo.TurnRow{ID: turnID, ThreadID: threadID, Seq: 1, Status: repo.TurnInProgress, Model: llm.Model{Name: "test"}, StartedAt: now}); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	item := repo.ItemRow{ID: uuid.NewString(), ThreadID: threadID, TurnID: turnID, Seq: 1, Type: repo.ItemUserMessage, Status: repo.ItemCompleted, Text: "the project uses Go", StartedAt: now, CompletedAt: &now}
	if err := db.SaveItem(item); err != nil {
		t.Fatalf("save item: %v", err)
	}
	turn, err := db.ActiveTurn(threadID)
	if err != nil {
		t.Fatalf("read turn: %v", err)
	}
	turn.Status, turn.CompletedAt = repo.TurnCompleted, &now
	if err := db.UpdateTurn(turn); err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	batch, err := nextBatch(db, 10)
	if err != nil || batch == nil || len(batch.Facts) == 0 {
		t.Fatalf("build batch: %+v err=%v", batch, err)
	}
	ops := []Operation{{
		Action: "add", Topic: "Project language", Kind: "fact", Text: "The project uses Go.",
		Sources: []repo.MemorySource{{Table: "items", ID: item.ID, Operation: "upsert"}},
	}}
	if err := commit(db, batch, ops, repo.MemoryReport{}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rows, err := db.Memories("", false)
	if err != nil || len(rows) != 1 || rows[0].Topic != "project language" {
		t.Fatalf("memory not committed: rows=%+v err=%v", rows, err)
	}
	if _, ok, err := db.NextEventScope(time.Now().UnixMilli()); err != nil || ok {
		t.Fatalf("events not consumed (ok=%v err=%v)", ok, err)
	}
}

func TestParseOperationsRejectsRevision(t *testing.T) {
	batch := &Batch{
		WorkspaceID: "",
		Facts:       []Fact{{Source: repo.MemorySource{Table: "items", ID: "i", Operation: "upsert"}}},
	}
	good := `{"operations":[{"action":"add","topic":"Topic","kind":"fact","text":"note","sources":[{"table":"items","id":"i","operation":"upsert"}]}]}`
	if ops, err := parseOperations(good, batch); err != nil || len(ops) != 1 {
		t.Fatalf("parse valid operations: ops=%+v err=%v", ops, err)
	}
	withRevision := `{"operations":[{"action":"add","revision":1,"topic":"Topic","kind":"fact","text":"note","sources":[{"table":"items","id":"i","operation":"upsert"}]}]}`
	if _, err := parseOperations(withRevision, batch); err == nil {
		t.Fatal("expected revision field to be rejected")
	}
}

func TestVisibleEvidenceDropsForgottenSources(t *testing.T) {
	batch := &Batch{
		Facts: []Fact{{Source: repo.MemorySource{Table: "items", ID: "i", Operation: "upsert"}}},
		Existing: []repo.MemoryRow{{
			ID: "gone", Status: "forgotten", Topic: "gone",
			Sources: []repo.MemorySource{{Table: "items", ID: "i"}},
		}},
	}
	if visible := visibleEvidence(batch); len(visible.Facts) != 0 {
		t.Fatalf("forgotten evidence was not filtered: %+v", visible.Facts)
	}
}
