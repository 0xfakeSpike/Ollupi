package threadmanager

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/agent"
	"github.com/0xfakeSpike/ollupi/agent/contextmanager"
	"github.com/0xfakeSpike/ollupi/llm"
	"github.com/0xfakeSpike/ollupi/storage"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/tools"
)

func TestItemContextStatsAcrossUserInputRestart(t *testing.T) {
	root := t.TempDir()
	db := itemStatsDB(t, root)
	firstUsage := thread.Usage{InputTokens: 100, CachedInputTokens: 30, OutputTokens: 15}
	secondUsage := thread.Usage{InputTokens: 200, CachedInputTokens: 60, OutputTokens: 25}
	var firstCalls atomic.Int32
	m, stop := itemStatsManager(t, db, func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error) {
		if firstCalls.Add(1) != 1 {
			return llm.ChatResponse{}, errors.New("unexpected model request while waiting for input")
		}
		return llm.ChatResponse{
			Message:      llm.ChatMessage{ToolCalls: []llm.ToolCall{{ID: "question-call", Name: agent.ToolRequestUserInput, Arguments: `{"questions":[{"id":"color","header":"Color","question":"Which color?"}]}`}}},
			FinishReason: "tool_calls", UsageReported: true,
			Usage: llm.Usage{PromptTokens: 100, CachedPromptTokens: 30, CompletionTokens: 15},
		}, nil
	})
	th := createModelThread(t, m)
	started, err := m.StartTurn(th.ID, "Ask me for a color, then answer.")
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()
	paused, _ := m.ReadThread(th.ID)
	first, pending := paused.PendingRequest()
	if !pending || first.Type != thread.ItemUserInputRequest || firstCalls.Load() != 1 {
		t.Fatalf("expected one model call followed by a question: calls=%d view=%+v", firstCalls.Load(), paused)
	}
	assertItemStatsSnapshot(t, first, firstUsage)
	if paused.Turns[0].Usage != firstUsage {
		t.Fatalf("pause failed to record usage exactly once: %+v", paused.Turns[0].Usage)
	}
	stop()
	m.Wait()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := itemStatsDB(t, root)
	var secondCalls atomic.Int32
	requests := make(chan llm.ChatRequest, 1)
	restarted, stopRestarted := itemStatsManager(t, reopened, func(_ context.Context, request llm.ChatRequest, delta func(llm.Delta)) (llm.ChatResponse, error) {
		if secondCalls.Add(1) != 1 {
			return llm.ChatResponse{}, errors.New("unexpected extra model request after resumption")
		}
		requests <- request
		delta(llm.Delta{Content: "You chose blue."})
		return llm.ChatResponse{Message: llm.ChatMessage{Content: "You chose blue."}, FinishReason: "stop", UsageReported: true,
			Usage: llm.Usage{PromptTokens: 200, CachedPromptTokens: 60, CompletionTokens: 25}}, nil
	})
	restarted.Wait()
	recovered, _ := restarted.ReadThread(th.ID)
	stillPending, ok := recovered.PendingRequest()
	if !ok || !reflect.DeepEqual(stillPending, first) || secondCalls.Load() != 0 {
		t.Fatalf("recovery changed the pending model request: calls=%d item=%+v", secondCalls.Load(), stillPending)
	}
	answers := map[string]thread.UserInputAnswer{"color": {Answers: []string{"blue"}}}
	if err := restarted.AnswerUserInput(th.ID, first.ID, answers); err != nil {
		t.Fatal(err)
	}
	restarted.Wait()
	completed, _ := restarted.ReadThread(th.ID)
	if len(completed.Turns) != 1 || completed.Turns[0].ID != started.ID || completed.Turns[0].Status != thread.TurnCompleted || secondCalls.Load() != 1 {
		t.Fatalf("turn did not resume once: calls=%d turns=%+v", secondCalls.Load(), completed.Turns)
	}
	var carriers []thread.Item
	var summedUsage thread.Usage
	for _, item := range completed.Items {
		if item.ContextStats != nil {
			carriers = append(carriers, item)
		}
		if item.Usage != nil {
			summedUsage.Add(*item.Usage)
		}
	}
	if len(carriers) != 2 || carriers[0].ID != first.ID || carriers[0].ResponseID != first.ResponseID || carriers[0].ResponseID == carriers[1].ResponseID {
		t.Fatalf("expected distinct original and resumed carriers: %+v", carriers)
	}
	assertItemStatsSnapshot(t, carriers[0], firstUsage)
	assertItemStatsSnapshot(t, carriers[1], secondUsage)
	if !reflect.DeepEqual(carriers[0].ContextStats, first.ContextStats) || !reflect.DeepEqual(carriers[0].Answers, answers) {
		t.Fatalf("answering changed original context snapshot or lost answer: %+v", carriers[0])
	}
	wantTotal := firstUsage
	wantTotal.Add(secondUsage)
	if completed.Turns[0].Usage != wantTotal || summedUsage != wantTotal {
		t.Fatalf("usage duplicated or lost across restart: turn=%+v items=%+v want=%+v", completed.Turns[0].Usage, summedUsage, wantTotal)
	}
	if carriers[1].ContextStats.MessagesTokens <= first.ContextStats.MessagesTokens {
		t.Fatal("resumed call did not snapshot the larger transcript")
	}
	request := <-requests
	wire, err := json.Marshal(request.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), "blue") || strings.Contains(string(wire), "contextStats") {
		t.Fatalf("resumed transcript lacks the answer or includes statistics: %s", wire)
	}
	stopRestarted()
	restarted.Wait()
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	persisted, err := itemStatsDB(t, root).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || !reflect.DeepEqual(persisted[0], completed) {
		t.Fatalf("completed conversation changed after reopening: %+v", persisted)
	}
}

func TestItemContextStatsPersistWhenCancelledBeforeDelta(t *testing.T) {
	root := t.TempDir()
	db := itemStatsDB(t, root)
	started := make(chan struct{})
	m, stop := itemStatsManager(t, db, func(ctx context.Context, _ llm.ChatRequest, _ func(llm.Delta)) (llm.ChatResponse, error) {
		close(started)
		<-ctx.Done()
		return llm.ChatResponse{}, ctx.Err()
	})
	th := createModelThread(t, m)
	if _, err := m.StartTurn(th.ID, "Wait for cancellation."); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("model request did not start")
	}
	if err := m.InterruptTurn(th.ID); err != nil {
		t.Fatal(err)
	}
	m.Wait()
	view, _ := m.ReadThread(th.ID)
	if len(view.Items) != 2 || view.Turns[0].Status != thread.TurnInterrupted {
		t.Fatalf("cancelled turn lacks its fallback item: %+v", view)
	}
	failed := view.Items[1]
	if failed.Type != thread.ItemError || failed.Status != thread.ItemFailed || failed.ResponseID == "" || failed.ContextStats == nil || failed.Usage != nil {
		t.Fatalf("cancelled request has incorrect metadata: %+v", failed)
	}
	if view.Turns[0].Usage != (thread.Usage{}) {
		t.Fatalf("cancelled request fabricated usage: %+v", view.Turns[0].Usage)
	}
	stop()
	m.Wait()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	persisted, err := itemStatsDB(t, root).Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || !reflect.DeepEqual(persisted[0], view) {
		t.Fatalf("cancelled request changed after reopening: %+v", persisted)
	}
}

type itemStatsChat func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error)

func (f itemStatsChat) Chat(ctx context.Context, request llm.ChatRequest, delta func(llm.Delta)) (llm.ChatResponse, error) {
	return f(ctx, request, delta)
}

type itemStatsTools struct{}

func (itemStatsTools) ToolDescriptors(context.Context) ([]tools.ToolDescriptor, error) {
	return nil, nil
}
func (itemStatsTools) Invoke(context.Context, string, map[string]any) tools.Result {
	return tools.Result{Err: errors.New("unexpected registry invocation")}
}
func (itemStatsTools) HasPermissionContent(string) bool { return false }
func (itemStatsTools) PermissionContent(string, map[string]any) (string, bool, error) {
	return "", false, nil
}

type itemStatsSkills struct{}

func (itemStatsSkills) Descriptors() []map[string]any { return nil }

func itemStatsManager(t *testing.T, db *storage.DB, model itemStatsChat) (*Manager, context.CancelFunc) {
	t.Helper()
	runner, err := agent.NewRunner(agent.RunnerConfig{Model: model, Tools: itemStatsTools{}, Skills: itemStatsSkills{}, Contexts: contextmanager.New(64000)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ag := agent.Agent{Name: "main", Instructions: "Help the user.", Model: "base", ModelProfileID: "primary", ApprovalPolicy: agent.ApprovalOnRequest}
	m, err := New(ctx, runner, ag, nil, db, availableModels()...)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); m.Wait() })
	return m, cancel
}

func itemStatsDB(t *testing.T, root string) *storage.DB {
	t.Helper()
	db, err := storage.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func assertItemStatsSnapshot(t *testing.T, item thread.Item, usage thread.Usage) {
	t.Helper()
	s := item.ContextStats
	if item.ResponseID == "" || s == nil || s.BudgetTokens != 64000 || s.SystemTokens <= 0 || s.MessagesTokens <= 0 || s.ToolsTokens <= 0 || s.EstimatedTokens != s.SystemTokens+s.MessagesTokens+s.ToolsTokens {
		t.Fatalf("invalid input snapshot on item %s: %+v", item.ID, s)
	}
	if item.Usage == nil || *item.Usage != usage {
		t.Fatalf("item %s: usage=%+v want=%+v", item.ID, item.Usage, usage)
	}
}
