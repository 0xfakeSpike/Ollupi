package threadmanager

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/agent"
	"github.com/0xfakeSpike/ollupi/storage"
	"github.com/0xfakeSpike/ollupi/thread"
)

type modelRunner struct {
	mu    sync.Mutex
	calls []agent.Agent
	run   func(context.Context, agent.Agent, agent.Turn, agent.Recorder) (agent.Result, error)
}

func (r *modelRunner) Run(ctx context.Context, a agent.Agent, turn agent.Turn, rec agent.Recorder) (agent.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, a)
	r.mu.Unlock()
	if r.run != nil {
		return r.run(ctx, a, turn, rec)
	}
	return agent.Result{}, nil
}

func (r *modelRunner) recorded() []agent.Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agent.Agent(nil), r.calls...)
}

func modelFixture(t *testing.T, db *storage.DB, runner *modelRunner, subagents []agent.Agent, profiles ...ModelProfile) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	main := agent.Agent{Name: "main", Instructions: "test", Model: "base", ModelProfileID: "primary", ApprovalPolicy: agent.ApprovalOnRequest}
	for _, s := range subagents {
		main.Subagents = append(main.Subagents, agent.Subagent{Name: s.Name, Description: "test"})
	}
	m, err := New(ctx, runner, main, subagents, db, profiles...)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Wait() })
	return m
}

func modelDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func availableModels() []ModelProfile {
	f := false
	return []ModelProfile{{ID: "primary", Name: "Primary connection", Model: "base"}, {ID: "other", Name: "Other", Model: "other-model", Settings: agent.ModelSettings{EnableThinking: &f}}}
}

func createModelThread(t *testing.T, m *Manager) thread.Thread {
	t.Helper()
	th, err := m.StartThread(StartThreadParams{Source: thread.SourceGateway})
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func TestModelSelectionIsPerConversationAndUnknownNeverFallsBack(t *testing.T) {
	r := &modelRunner{}
	m := modelFixture(t, modelDB(t), r, nil, availableModels()...)
	a, b := createModelThread(t, m), createModelThread(t, m)
	first, err := m.StartTurnWithModel(a.ID, "one", "other")
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if first.ModelProfileID != "other" || first.Model != "other-model" || first.ModelSettings.EnableThinking == nil || *first.ModelSettings.EnableThinking {
		t.Fatalf("wrong snapshot: %+v", first)
	}
	second, err := m.StartTurn(a.ID, "two")
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()
	separate, err := m.StartTurn(b.ID, "three")
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if second.ModelProfileID != "other" || separate.ModelProfileID != "primary" {
		t.Fatalf("conversation selection leaked: %s %s", second.ModelProfileID, separate.ModelProfileID)
	}
	if _, err = m.StartTurnWithModel(a.ID, "invalid", "unknown"); err == nil {
		t.Fatal("unknown profile accepted")
	}
	if len(r.recorded()) != 3 {
		t.Fatal("unknown profile reached runner")
	}
	data, err := json.Marshal(m.ListModels())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Thinking") || strings.Contains(string(data), "api") || strings.Contains(string(data), "Settings") {
		t.Fatalf("catalog exposed private configuration: %s", data)
	}
}

func approvalRunner() *modelRunner {
	return &modelRunner{run: func(_ context.Context, _ agent.Agent, turn agent.Turn, rec agent.Recorder) (agent.Result, error) {
		for _, it := range turn.Items {
			if it.ID == "approval" {
				return agent.Result{}, nil
			}
		}
		now := time.Now().UTC()
		it, err := rec.Record(thread.Item{ID: "approval", TurnID: turn.ID, Type: thread.ItemMcpToolCall, Status: thread.ItemInProgress, Tool: "test", CallID: "call", Arguments: `{}`, StartedAt: now, Approval: &thread.Approval{ID: "approval-id", RequestedAt: now}})
		return agent.Result{Pending: &it}, err
	}}
}

func TestApprovalAndRestartKeepOriginalModelSnapshot(t *testing.T) {
	db := modelDB(t)
	r := approvalRunner()
	m := modelFixture(t, db, r, nil, availableModels()...)
	th := createModelThread(t, m)
	turn, err := m.StartTurnWithModel(th.ID, "approve", "other")
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if _, err = m.StartTurnWithModel(th.ID, "switch", "primary"); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("changed active turn: %v", err)
	}
	// A new manager simulates restarting with edited profile settings while
	// the durable turn still waits for the approval.
	changed := availableModels()
	yes := true
	changed[1].Model = "new-model"
	changed[1].Settings.EnableThinking = &yes
	r2 := approvalRunner()
	restarted := modelFixture(t, db, r2, nil, changed...)
	if len(r2.recorded()) != 0 {
		t.Fatal("approval wait unexpectedly ran during recovery")
	}
	if err := restarted.ResolveApproval(th.ID, "approval-id", thread.DecisionAccept, "test"); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	calls := r2.recorded()
	if len(calls) != 1 {
		t.Fatalf("expected one resumed call, got %d", len(calls))
	}
	if calls[0].Model != "other-model" || calls[0].ModelProfileID != "other" || calls[0].ModelSettings.EnableThinking == nil || *calls[0].ModelSettings.EnableThinking {
		t.Fatalf("resumed with changed model: %+v", calls[0])
	}
	v, _ := restarted.ReadThread(th.ID)
	if v.Turns[0].ID != turn.ID || v.Turns[0].Status != thread.TurnCompleted {
		t.Fatalf("turn not resumed: %+v", v.Turns)
	}
	next, err := restarted.StartTurn(th.ID, "next")
	if err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	if next.Model != "new-model" || next.ModelSettings.EnableThinking == nil || !*next.ModelSettings.EnableThinking {
		t.Fatalf("next turn should use current configured profile: %+v", next)
	}
}

func TestMissingProfileFailsPendingTurnOnRecovery(t *testing.T) {
	db := modelDB(t)
	m := modelFixture(t, db, approvalRunner(), nil, availableModels()...)
	th := createModelThread(t, m)
	if _, err := m.StartTurnWithModel(th.ID, "approve", "other"); err != nil {
		t.Fatal(err)
	}
	m.Wait()
	r := &modelRunner{}
	restarted := modelFixture(t, db, r, nil, availableModels()[:1]...)
	restarted.Wait()
	v, _ := restarted.ReadThread(th.ID)
	if v.Turns[0].Status != thread.TurnFailed || !strings.Contains(v.Turns[0].Error, `"other"`) {
		t.Fatalf("missing profile silently resumed: %+v", v.Turns[0])
	}
	if len(r.recorded()) != 0 {
		t.Fatal("missing profile sent work to another connection")
	}
	if _, err := restarted.StartTurn(th.ID, "still other"); err == nil {
		t.Fatal("missing previous profile fell back")
	}
	if _, err := restarted.StartTurnWithModel(th.ID, "explicitly choose primary", "primary"); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
}

func TestRunningTurnRecoversSavedConnectionAfterShutdown(t *testing.T) {
	db := modelDB(t)
	started := make(chan struct{})
	r := &modelRunner{run: func(ctx context.Context, _ agent.Agent, _ agent.Turn, _ agent.Recorder) (agent.Result, error) {
		close(started)
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	m, err := New(ctx, r, agent.Agent{Name: "main", Model: "base", ModelProfileID: "primary"}, nil, db, availableModels()...)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Wait() })
	th := createModelThread(t, m)
	if _, err := m.StartTurnWithModel(th.ID, "work", "other"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not start")
	}
	cancel()
	m.Wait()
	changed := availableModels()
	changed[1].Model = "changed"
	r2 := &modelRunner{}
	restarted := modelFixture(t, db, r2, nil, changed...)
	restarted.Wait()
	calls := r2.recorded()
	if len(calls) != 1 || calls[0].ModelProfileID != "other" || calls[0].Model != "other-model" {
		t.Fatalf("recovered wrong model: %+v", calls)
	}
	view, _ := restarted.ReadThread(th.ID)
	if view.Turns[0].Status != thread.TurnCompleted {
		t.Fatalf("recovery did not complete: %+v", view.Turns)
	}
}

func TestSubagentInheritanceAndExplicitModelConnections(t *testing.T) {
	main := agent.Agent{Name: "main", Model: "base", ModelProfileID: "primary"}
	inherit := (agent.Definition{Name: "inherit", Instructions: "test"}).Agent(main)
	explicit := (agent.Definition{Name: "explicit", Instructions: "test", Model: "special"}).Agent(main)
	r := &modelRunner{run: func(_ context.Context, a agent.Agent, turn agent.Turn, rec agent.Recorder) (agent.Result, error) {
		now := time.Now().UTC()
		if a.Name != "main" {
			_, err := rec.Record(thread.Item{ID: turn.ID + "answer", TurnID: turn.ID, Type: thread.ItemAgentMessage, Status: thread.ItemCompleted, Text: "done", StartedAt: now, CompletedAt: &now})
			return agent.Result{}, err
		}
		for _, it := range turn.Items {
			if it.Type == thread.ItemCollabAgentToolCall {
				return agent.Result{}, nil
			}
		}
		for _, name := range []string{"inherit", "explicit"} {
			_, err := rec.Record(thread.Item{ID: turn.ID + name, TurnID: turn.ID, Type: thread.ItemCollabAgentToolCall, Status: thread.ItemInProgress, Agent: name, CallID: name, Tool: agent.ToolAgent, Arguments: `{"subagent_type":"` + name + `","description":"test","prompt":"work"}`, StartedAt: now})
			if err != nil {
				return agent.Result{}, err
			}
		}
		return agent.Result{Waiting: true}, nil
	}}
	m := modelFixture(t, modelDB(t), r, []agent.Agent{inherit, explicit}, availableModels()...)
	th := createModelThread(t, m)
	if _, err := m.StartTurnWithModel(th.ID, "delegate", "other"); err != nil {
		t.Fatal(err)
	}
	m.Wait()
	seen := map[string]agent.Agent{}
	for _, a := range r.recorded() {
		seen[a.Name] = a
	}
	if a := seen["inherit"]; a.ModelProfileID != "other" || a.Model != "other-model" || a.ModelSettings.EnableThinking == nil || *a.ModelSettings.EnableThinking {
		t.Fatalf("inherit mismatch: %+v", a)
	}
	if a := seen["explicit"]; a.ModelProfileID != "primary" || a.Model != "special" {
		t.Fatalf("explicit definition moved provider: %+v", a)
	}
	if a := seen["main"]; a.ModelProfileID != "other" || a.Model != "other-model" {
		t.Fatalf("parent resumed incorrectly: %+v", a)
	}
}
