package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/contextmanager"
	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/llm"
	"github.com/0xfakeSpike/ollupi/memory"
	"github.com/0xfakeSpike/ollupi/thread"
)

type memoryReaderFunc func(context.Context, string, string, int) ([]memory.Entry, error)

func (f memoryReaderFunc) Recall(ctx context.Context, workspaceID, query string, budget int) ([]memory.Entry, error) {
	return f(ctx, workspaceID, query, budget)
}

func newMemoryTestRunner(t *testing.T, reader memory.Reader, model statsModel) *Runner {
	t.Helper()
	runner, err := NewRunner(RunnerConfig{Model: model, Tools: statsTools{}, Skills: statsSkills{}, Contexts: contextmanager.New(64000), Memory: reader})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

func memoryTestAgent() Agent {
	return Agent{Name: "test", Instructions: "Help the user", Model: "test-model", ApprovalPolicy: ApprovalNever}
}

func TestMemorySnapshotSurvivesPauseAndRunnerRestart(t *testing.T) {
	recalls, modelCalls := 0, 0
	reader := memoryReaderFunc(func(_ context.Context, workspaceID, query string, budget int) ([]memory.Entry, error) {
		recalls++
		if workspaceID != "workspace-1" || query != "Improve the macOS app" || budget != 2000 {
			t.Fatalf("incorrect retrieval scope or query: %q %q %d", workspaceID, query, budget)
		}
		return []memory.Entry{
			{ID: "global", Revision: 2, Status: "active", Text: "GLOBAL_NOTE"},
			{ID: "project", Revision: 5, WorkspaceID: "workspace-1", Status: "active", Text: "PROJECT_NOTE"},
			{ID: "wrong-project", Revision: 1, WorkspaceID: "workspace-2", Status: "active", Text: "OTHER_PROJECT_SECRET"},
			{ID: "forgotten", Revision: 2, Status: "forgotten", Text: "FORGOTTEN_NOTE"},
		}, nil
	})
	model := statsModel(func(_ context.Context, request llm.ChatRequest, _ func(llm.Delta)) (llm.ChatResponse, error) {
		modelCalls++
		var content strings.Builder
		for _, message := range request.Messages {
			content.WriteString(message.Content)
		}
		if strings.Count(content.String(), "GLOBAL_NOTE") != 1 || strings.Count(content.String(), "PROJECT_NOTE") != 1 || strings.Contains(content.String(), "OTHER_PROJECT_SECRET") || strings.Contains(content.String(), "FORGOTTEN_NOTE") {
			t.Fatalf("incorrect memory input: %s", content.String())
		}
		if modelCalls == 1 {
			return llm.ChatResponse{Message: llm.ChatMessage{ToolCalls: []llm.ToolCall{{ID: "question", Name: ToolRequestUserInput, Arguments: `{"questions":[{"id":"scope","header":"Scope","question":"Which view?"}]}`}}}, FinishReason: "tool_calls"}, nil
		}
		return llm.ChatResponse{Message: llm.ChatMessage{Content: "Done"}, FinishReason: "stop"}, nil
	})
	recorder := &statsRecorder{t: t}
	user, _ := recorder.Record(thread.NewUserMessage("user", "turn", "Improve the macOS app", time.Now().UTC()))
	ctx := execution.WithWorkspaceID(context.Background(), "workspace-1")
	runner := newMemoryTestRunner(t, reader, model)
	result, err := runner.Run(ctx, memoryTestAgent(), Turn{ID: "turn", Items: []thread.Item{user}, Input: statsInput{}}, recorder)
	if err != nil || result.Pending == nil {
		t.Fatalf("turn did not pause: %+v %v", result, err)
	}
	pending := *result.Pending
	pending.Status = thread.ItemCompleted
	pending.Answers = map[string]thread.UserInputAnswer{"scope": {Answers: []string{"Settings"}}}
	now := time.Now().UTC()
	pending.CompletedAt = &now
	if _, err := recorder.Record(pending); err != nil {
		t.Fatal(err)
	}
	// A new Runner stands in for process recovery; even with its reader
	// disabled, the saved selection remains the input of this existing turn.
	runner = newMemoryTestRunner(t, nil, model)
	result, err = runner.Run(ctx, memoryTestAgent(), Turn{ID: "turn", Items: recorder.items, Input: statsInput{}}, recorder)
	if err != nil || result.Pending != nil || recalls != 1 || modelCalls != 2 {
		t.Fatalf("resume changed retrieval: %+v %v recalls=%d calls=%d", result, err, recalls, modelCalls)
	}
	snapshots := 0
	for _, item := range recorder.items {
		if item.Type == thread.ItemMemoryContext {
			snapshots++
			if len(item.Memory) != 2 || item.Memory[1].Revision != 5 {
				t.Fatalf("incorrect persisted selection: %+v", item.Memory)
			}
		}
		if item.ContextStats != nil && (item.ContextStats.MemoryTokens <= 0 || item.ContextStats.MemoryTokens > item.ContextStats.MessagesTokens) {
			t.Fatalf("missing persisted memory token share: %+v", item.ContextStats)
		}
	}
	if snapshots != 1 {
		t.Fatalf("got %d snapshots for one turn", snapshots)
	}
}

func TestMemoryFailureFreezesEmptySelectionAndContinues(t *testing.T) {
	recalls, calls := 0, 0
	runner := newMemoryTestRunner(t, memoryReaderFunc(func(context.Context, string, string, int) ([]memory.Entry, error) {
		recalls++
		return nil, errors.New("unavailable")
	}), statsModel(func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error) {
		calls++
		return llm.ChatResponse{Message: llm.ChatMessage{Content: "Done"}, FinishReason: "stop"}, nil
	}))
	recorder := &statsRecorder{t: t}
	user, _ := recorder.Record(thread.NewUserMessage("user", "turn", "hello", time.Now().UTC()))
	_, err := runner.Run(context.Background(), memoryTestAgent(), Turn{ID: "turn", Items: []thread.Item{user}, Input: statsInput{}}, recorder)
	if err != nil || recalls != 1 || calls != 1 {
		t.Fatalf("retrieval failure blocked the response: %v recalls=%d calls=%d", err, recalls, calls)
	}
	if len(recorder.items) != 3 || recorder.items[1].Type != thread.ItemMemoryContext || recorder.items[1].Status != thread.ItemCompleted || len(recorder.items[1].Memory) != 0 {
		t.Fatalf("missing empty snapshot: %+v", recorder.items)
	}
	if _, err := runner.start(context.Background(), memoryTestAgent(), Turn{ID: "turn", Items: recorder.items, Input: statsInput{}}, recorder); err != nil || recalls != 1 {
		t.Fatalf("empty snapshot retried retrieval on resume: %v recalls=%d", err, recalls)
	}
}

func TestMemoryRetrievalIsPerTurnAndGeneralCannotReadWorkspace(t *testing.T) {
	recalls := 0
	runner := newMemoryTestRunner(t, memoryReaderFunc(func(_ context.Context, workspaceID, query string, _ int) ([]memory.Entry, error) {
		recalls++
		if workspaceID != "" || query != "new request" {
			t.Fatalf("new-turn query reused prior history: %q %q", workspaceID, query)
		}
		return []memory.Entry{{ID: "global", Revision: 1, Status: "active", Text: "new memory"}, {ID: "workspace", Revision: 1, WorkspaceID: "private", Status: "active", Text: "private memory"}}, nil
	}), statsModel(func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error) {
		return llm.ChatResponse{}, nil
	}))
	recorder := &statsRecorder{t: t}
	recorder.Record(thread.Item{ID: "old-memory", TurnID: "old", Type: thread.ItemMemoryContext, Status: thread.ItemCompleted, Memory: []thread.MemoryReference{{ID: "old", Text: "old memory"}}})
	recorder.Record(thread.NewUserMessage("new-user", "new", "new request", time.Now().UTC()))
	r, err := runner.start(context.Background(), memoryTestAgent(), Turn{ID: "new", Items: recorder.items, Input: statsInput{}}, recorder)
	if err != nil || recalls != 1 || len(r.memory) != 1 || r.memory[0].ID != "global" {
		t.Fatalf("incorrect new-turn selection: %+v %v recalls=%d", r, err, recalls)
	}
}
