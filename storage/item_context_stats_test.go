package storage

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/thread"
)

func TestItemContextStatsPersistInBody(t *testing.T) {
	root := t.TempDir()
	db := openContextStatsDB(t, root)
	change := contextStatsChange()
	at := change.Thread.CreatedAt
	stats := thread.ContextStats{BudgetTokens: 64000, SystemTokens: 300, MessagesTokens: 500, ToolsTokens: 200, EstimatedTokens: 1000}
	usage := thread.Usage{InputTokens: 910, CachedInputTokens: 120, OutputTokens: 35}
	user := thread.NewUserMessage("user", change.Turns[0].ID, "Hello", at)
	user.Seq = 1
	change.Items = []thread.Item{
		user,
		{ID: "reasoning", TurnID: "turn", Seq: 2, Type: thread.ItemReasoning, Status: thread.ItemCompleted, ResponseID: "response", Text: "Thinking", ContextStats: &stats, Usage: &usage, StartedAt: at, CompletedAt: &at},
		{ID: "answer", TurnID: "turn", Seq: 3, Type: thread.ItemAgentMessage, Status: thread.ItemCompleted, ResponseID: "response", Text: "Hello!", StartedAt: at, CompletedAt: &at},
		{ID: "failed", TurnID: "turn", Seq: 4, Type: thread.ItemError, Status: thread.ItemFailed, ResponseID: "failed-response", Error: "request failed", ContextStats: &stats, StartedAt: at, CompletedAt: &at},
		{ID: "compaction", TurnID: "turn", Seq: 5, Type: thread.ItemContextCompaction, Status: thread.ItemCompleted, ResponseID: "compaction-response", Text: "Summary", ContextStats: &stats, Usage: &usage, StartedAt: at, CompletedAt: &at},
	}
	if err := db.Save(change); err != nil {
		t.Fatal(err)
	}

	// The existing JSON body carries the new data. Legacy and sibling items
	// omit the optional fields instead of fabricating zero measurements.
	for _, item := range change.Items {
		var body string
		if err := db.conn.QueryRow(`SELECT body FROM items WHERE id=?`, item.ID).Scan(&body); err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["contextStats"]; present != (item.ContextStats != nil) {
			t.Errorf("item %s: contextStats presence = %v", item.ID, present)
		}
		if _, present := fields["usage"]; present != (item.Usage != nil) {
			t.Errorf("item %s: usage presence = %v", item.ID, present)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openContextStatsDB(t, root)
	views, err := reopened.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || !reflect.DeepEqual(views[0].Items, change.Items) {
		t.Fatalf("reopened items differ: got %#v", views)
	}
	if views[0].Turns[0].Usage != change.Turns[0].Usage {
		t.Fatalf("item saves changed cumulative turn usage: got %+v", views[0].Turns[0].Usage)
	}
}

func TestItemContextStatsSurviveApprovalAndToolResult(t *testing.T) {
	root := t.TempDir()
	db := openContextStatsDB(t, root)
	change := contextStatsChange()
	at := change.Thread.CreatedAt
	stats := thread.ContextStats{BudgetTokens: 64000, SystemTokens: 200, MessagesTokens: 900, ToolsTokens: 300, EstimatedTokens: 1400}
	usage := thread.Usage{InputTokens: 1300, CachedInputTokens: 80, OutputTokens: 42}
	change.Items = []thread.Item{{
		ID: "tool", TurnID: "turn", Seq: 1,
		Type: thread.ItemMcpToolCall, Status: thread.ItemInProgress,
		ResponseID: "tool-response", CallID: "tool-call", Tool: "test-tool", Arguments: `{}`,
		ContextStats: &stats, Usage: &usage,
		Approval: &thread.Approval{ID: "approval", RequestedAt: at}, StartedAt: at,
	}}
	if err := db.Save(change); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openContextStatsDB(t, root)
	store, err := thread.Open(reopened)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := store.Recover(change.Thread.ID)
	if err != nil || len(closed) != 0 {
		t.Fatalf("pending approval should survive recovery: closed=%v, err=%v", closed, err)
	}
	view, ok := store.Read(change.Thread.ID)
	if !ok || len(view.Items) != 1 {
		t.Fatalf("missing recovered item: %#v", view)
	}
	assertContextStats(t, view.Items[0], stats, usage)

	// Read returns detached snapshots: client-side modifications must not
	// corrupt the stored counters while an approval is pending.
	view.Items[0].ContextStats.SystemTokens = -1
	view.Items[0].Usage.InputTokens = -1
	view, _ = store.Read(change.Thread.ID)
	assertContextStats(t, view.Items[0], stats, usage)

	item := view.Items[0]
	item.Approval.Decision = thread.DecisionAccept
	item.Approval.DecidedAt = &at
	item, isNew, err := store.Record(change.Thread.ID, item)
	if err != nil || isNew {
		t.Fatalf("approve existing item: isNew=%v, err=%v", isNew, err)
	}
	item.Status, item.CompletedAt = thread.ItemCompleted, &at
	item.Result = &thread.ToolResult{Output: json.RawMessage(`"done"`)}
	if _, isNew, err := store.Record(change.Thread.ID, item); err != nil || isNew {
		t.Fatalf("complete existing item: isNew=%v, err=%v", isNew, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	completedDB := openContextStatsDB(t, root)
	views, err := completedDB.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || len(views[0].Items) != 1 {
		t.Fatalf("expected the original item after result update: %#v", views)
	}
	got := views[0].Items[0]
	assertContextStats(t, got, stats, usage)
	if !reflect.DeepEqual(got, item) {
		t.Fatalf("completed tool changed across reopen: got %+v, want %+v", got, item)
	}
	if views[0].Turns[0].Usage != change.Turns[0].Usage {
		t.Fatalf("item rewrites changed cumulative turn usage: %+v", views[0].Turns[0].Usage)
	}
}

func openContextStatsDB(t *testing.T, root string) *DB {
	t.Helper()
	db, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func contextStatsChange() thread.Change {
	at := time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC)
	return thread.Change{
		Thread: thread.Thread{ID: "thread", Source: thread.SourceGateway, Preview: "Hello", CreatedAt: at, UpdatedAt: at},
		Turns:  []thread.Turn{{ID: "turn", ThreadID: "thread", Seq: 1, Status: thread.TurnInProgress, Agent: "test", Model: "test-model", ModelProfileID: "primary", ApprovalPolicy: "always", Usage: thread.Usage{InputTokens: 2300, CachedInputTokens: 150, OutputTokens: 100}, StartedAt: at}},
	}
}

func assertContextStats(t *testing.T, item thread.Item, stats thread.ContextStats, usage thread.Usage) {
	t.Helper()
	if item.ContextStats == nil || *item.ContextStats != stats {
		t.Fatalf("item %s context stats: got %+v, want %+v", item.ID, item.ContextStats, stats)
	}
	if item.Usage == nil || *item.Usage != usage {
		t.Fatalf("item %s usage: got %+v, want %+v", item.ID, item.Usage, usage)
	}
}
