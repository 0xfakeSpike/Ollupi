package agent

import (
	"context"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
)

func newTestScheduler(t *testing.T) (*scheduler, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := newScheduler(&Agent{ctx: ctx}, ctx)
	t.Cleanup(cancel)
	return s, cancel
}

func TestQueueIsFIFO(t *testing.T) {
	s, _ := newTestScheduler(t)
	s.mu.Lock()
	s.enqueue("t1", "u1")
	s.enqueue("t2", "u2")
	s.mu.Unlock()
	first, ok := s.nextWork()
	if !ok || first.threadID != "t1" || first.turnID != "u1" {
		t.Fatalf("first work = %+v ok=%v", first, ok)
	}
	second, ok := s.nextWork()
	if !ok || second.threadID != "t2" || second.turnID != "u2" {
		t.Fatalf("second work = %+v ok=%v", second, ok)
	}
}

func TestNextWorkBlocksUntilEnqueue(t *testing.T) {
	s, _ := newTestScheduler(t)
	got := make(chan work, 1)
	go func() {
		w, _ := s.nextWork()
		got <- w
	}()
	select {
	case w := <-got:
		t.Fatalf("nextWork returned before enqueue: %+v", w)
	case <-time.After(50 * time.Millisecond):
	}
	s.mu.Lock()
	s.enqueue("t1", "u1")
	s.mu.Unlock()
	select {
	case w := <-got:
		if w.threadID != "t1" || w.turnID != "u1" {
			t.Fatalf("unexpected work: %+v", w)
		}
	case <-time.After(time.Second):
		t.Fatal("nextWork was not woken by enqueue")
	}
}

func TestNextWorkWakesAndReturnsFalseOnShutdown(t *testing.T) {
	s, cancel := newTestScheduler(t)
	got := make(chan bool, 1)
	go func() {
		_, ok := s.nextWork()
		got <- ok
	}()
	cancel()
	select {
	case ok := <-got:
		if ok {
			t.Fatal("nextWork returned work during shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("nextWork did not wake on shutdown")
	}
}

func TestStopLockedRemovesQueuedWorkAndEndsTurn(t *testing.T) {
	db := openAgentTestDB(t)
	now := time.Now().UTC()
	if err := db.CreateThread(repo.ThreadRow{ID: "t1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if err := db.CreateTurn(repo.TurnRow{
		ID: "u1", ThreadID: "t1", Seq: 1, Status: repo.TurnInProgress,
		Model: llm.Model{Name: "gpt-4o"}, StartedAt: now,
	}); err != nil {
		t.Fatalf("create turn: %v", err)
	}
	s, _ := newTestScheduler(t)
	s.a.DB = db
	s.mu.Lock()
	s.enqueue("t1", "u1")
	sl := s.slots["t1"]
	s.stop("t1", sl)
	queued := s.queued("t1")
	s.mu.Unlock()
	if queued {
		t.Fatal("work is still queued after stop")
	}
	if _, err := db.ActiveTurn("t1"); err == nil {
		t.Fatal("turn is still in progress after stop")
	}
}
