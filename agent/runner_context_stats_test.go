package agent

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/contextmanager"
	"github.com/0xfakeSpike/ollupi/llm"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/tools"
)

type statsModel func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error)

func (f statsModel) Chat(ctx context.Context, req llm.ChatRequest, delta func(llm.Delta)) (llm.ChatResponse, error) {
	return f(ctx, req, delta)
}

// Copy through JSON so mutations to an in-memory item cannot silently update
// a persisted snapshot, matching the real store's boundary.
type statsRecorder struct {
	t      *testing.T
	items  []thread.Item
	writes []thread.Item
}

func (s *statsRecorder) Record(it thread.Item) (thread.Item, error) {
	s.t.Helper()
	data, err := json.Marshal(it)
	if err != nil {
		s.t.Fatal(err)
	}
	var saved thread.Item
	if err := json.Unmarshal(data, &saved); err != nil {
		s.t.Fatal(err)
	}
	for i := range s.items {
		if s.items[i].ID == it.ID {
			saved.Seq = s.items[i].Seq
			s.items[i] = saved
			s.writes = append(s.writes, saved)
			return saved, nil
		}
	}
	saved.Seq = len(s.items) + 1
	s.items = append(s.items, saved)
	s.writes = append(s.writes, saved)
	return saved, nil
}

func (*statsRecorder) Delta(thread.Item, string) {}

type statsTools struct {
	onInvoke func()
}

func (s statsTools) ToolDescriptors(context.Context) ([]tools.ToolDescriptor, error) {
	return []tools.ToolDescriptor{{Name: "echo", InputSchema: map[string]any{"type": "object"}}}, nil
}
func (s statsTools) Invoke(context.Context, string, map[string]any) tools.Result {
	if s.onInvoke != nil {
		s.onInvoke()
	}
	return tools.Result{Output: "echo result"}
}
func (statsTools) HasPermissionContent(string) bool { return false }
func (statsTools) PermissionContent(string, map[string]any) (string, bool, error) {
	return "", false, nil
}

type statsSkills struct{}

func (statsSkills) Descriptors() []map[string]any { return nil }

type statsInput struct{}

func (statsInput) Take() []string        { return nil }
func (statsInput) TakeOrClose() []string { return nil }

func statsFixture(t *testing.T, model statsModel) (*Runner, *run, *statsRecorder) {
	t.Helper()
	rn, err := NewRunner(RunnerConfig{Model: model, Tools: statsTools{}, Skills: statsSkills{}, Contexts: contextmanager.New(64000)})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &statsRecorder{t: t}
	user, _ := recorder.Record(thread.NewUserMessage("user", "turn", "Inspect this project", time.Now().UTC()))
	r, err := rn.start(context.Background(), Agent{Name: "test", Instructions: "Help the user", Model: "test-model", ApprovalPolicy: ApprovalNever}, Turn{ID: "turn", Items: []thread.Item{user}, Input: statsInput{}}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	return rn, r, recorder
}

func statsCalls() []llm.ToolCall {
	return []llm.ToolCall{{ID: "call-1", Name: "echo", Arguments: `{}`}, {ID: "call-2", Name: "echo", Arguments: `{}`}}
}

func assertStatsCarrier(t *testing.T, items []thread.Item, responseID string, wantStats *thread.ContextStats, wantUsage *thread.Usage) thread.Item {
	t.Helper()
	var first *thread.Item
	statsCount, usageCount := 0, 0
	for i := range items {
		it := &items[i]
		if it.ResponseID != responseID {
			continue
		}
		if first == nil {
			first = it
		}
		if it.ContextStats != nil {
			statsCount++
		}
		if it.Usage != nil {
			usageCount++
		}
	}
	if first == nil || responseID == "" {
		t.Fatalf("missing response items for %q", responseID)
	}
	if statsCount != 1 || !reflect.DeepEqual(first.ContextStats, wantStats) {
		t.Fatalf("stats carriers=%d; first stats=%+v, want %+v", statsCount, first.ContextStats, wantStats)
	}
	wantUsageCount := 0
	if wantUsage != nil {
		wantUsageCount = 1
	}
	if usageCount != wantUsageCount || !reflect.DeepEqual(first.Usage, wantUsage) {
		t.Fatalf("usage carriers=%d; first usage=%+v, want %+v", usageCount, first.Usage, wantUsage)
	}
	return *first
}

func TestResponseStatsBelongToFirstItem(t *testing.T) {
	for _, mode := range []string{"reasoning-first", "content-first", "nonstreaming", "tools-only"} {
		t.Run(mode, func(t *testing.T) {
			resp := llm.ChatResponse{Message: llm.ChatMessage{Content: "Looking into it", ReasoningContent: "Think first", ToolCalls: statsCalls()}, FinishReason: "tool_calls", UsageReported: true, Usage: llm.Usage{PromptTokens: 100, CachedPromptTokens: 20, CompletionTokens: 30}}
			if mode == "tools-only" {
				resp.Message.Content, resp.Message.ReasoningContent = "", ""
			}
			rn, r, recorder := statsFixture(t, func(_ context.Context, _ llm.ChatRequest, delta func(llm.Delta)) (llm.ChatResponse, error) {
				switch mode {
				case "reasoning-first":
					delta(llm.Delta{Reasoning: "Think "})
					delta(llm.Delta{Content: "Looking into it", Reasoning: "first"})
				case "content-first":
					delta(llm.Delta{Content: "Looking into it"})
					delta(llm.Delta{Reasoning: "Think first"})
				}
				return resp, nil
			})
			prompt, err := rn.contexts.Build(rn.contextInput(r))
			if err != nil {
				t.Fatal(err)
			}
			_, responseID, err := rn.respond(context.Background(), r, llm.ToolChoiceAuto)
			if err != nil {
				t.Fatal(err)
			}
			wantUsage := &thread.Usage{InputTokens: 100, CachedInputTokens: 20, OutputTokens: 30}
			first := assertStatsCarrier(t, recorder.items, responseID, contextStats(prompt.Stats), wantUsage)
			wantType := thread.ItemReasoning
			if mode == "content-first" {
				wantType = thread.ItemAgentMessage
			}
			if mode == "tools-only" {
				wantType = thread.ItemMcpToolCall
			}
			if first.Type != wantType {
				t.Fatalf("carrier type=%s, want %s", first.Type, wantType)
			}
			if r.usage != *wantUsage {
				t.Fatalf("run usage=%+v, want %+v", r.usage, *wantUsage)
			}
			wantItems := 5 // user, reasoning, message, two tool calls
			if mode == "tools-only" {
				wantItems = 3
			}
			if len(recorder.items) != wantItems {
				t.Fatalf("got %d items, want %d", len(recorder.items), wantItems)
			}
			if mode == "reasoning-first" || mode == "content-first" {
				started := recorder.writes[1]
				if started.ContextStats == nil || started.Usage != nil || started.Status != thread.ItemInProgress {
					t.Fatalf("request estimates must persist before usage is known: %+v", started)
				}
			}
		})
	}
}

func TestFailedResponseRetainsStatsAndReportedUsage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stream     bool
		reported   bool
		requestErr error
		finish     string
	}{
		{name: "transport", requestErr: errors.New("provider unavailable")},
		{name: "cancelled", requestErr: context.Canceled},
		{name: "cancelled-after-delta", stream: true, requestErr: context.Canceled},
		{name: "deadline", requestErr: context.DeadlineExceeded},
		{name: "partial", stream: true, requestErr: errors.New("broken stream")},
		{name: "partial-with-usage", stream: true, reported: true, requestErr: errors.New("broken stream")},
		{name: "empty-response", reported: true, finish: "stop"},
		{name: "truncated-response", stream: true, reported: true, finish: "length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rn, r, recorder := statsFixture(t, func(_ context.Context, _ llm.ChatRequest, delta func(llm.Delta)) (llm.ChatResponse, error) {
				resp := llm.ChatResponse{FinishReason: tc.finish, UsageReported: tc.reported}
				if tc.reported {
					resp.Usage = llm.Usage{PromptTokens: 101, CompletionTokens: 3}
				}
				if tc.stream {
					delta(llm.Delta{Content: "Partial answer"})
					delta(llm.Delta{Reasoning: "Partial thought"})
					resp.Message.Content = "Partial answer"
				}
				return resp, tc.requestErr
			})
			prompt, err := rn.contexts.Build(rn.contextInput(r))
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = rn.respond(context.Background(), r, llm.ToolChoiceAuto)
			if err == nil {
				t.Fatal("expected model failure")
			}
			if tc.requestErr != nil && !errors.Is(err, tc.requestErr) {
				t.Fatalf("lost original error: %v", err)
			}
			var wantUsage *thread.Usage
			if tc.reported {
				wantUsage = &thread.Usage{InputTokens: 101, OutputTokens: 3}
			}
			first := assertStatsCarrier(t, recorder.items, recorder.items[1].ResponseID, contextStats(prompt.Stats), wantUsage)
			if first.Status != thread.ItemFailed || first.Error == "" || first.CompletedAt == nil {
				t.Fatalf("unfinished failed carrier: %+v", first)
			}
			if tc.stream {
				if first.Type != thread.ItemAgentMessage || first.Text != "Partial answer" {
					t.Fatalf("partial response not retained: %+v", first)
				}
			} else if first.Type != thread.ItemError {
				t.Fatalf("expected fallback error item, got %s", first.Type)
			}
			if wantUsage != nil && r.usage != *wantUsage {
				t.Fatalf("lost failure usage: %+v", r.usage)
			}
			if wantUsage == nil && r.usage != (thread.Usage{}) {
				t.Fatalf("invented failure usage: %+v", r.usage)
			}
		})
	}
}

func TestSuccessfulResponseWithoutUsageDoesNotInventUsage(t *testing.T) {
	for _, reported := range []bool{false, true} {
		rn, r, recorder := statsFixture(t, func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error) {
			return llm.ChatResponse{Message: llm.ChatMessage{Content: "Done"}, FinishReason: "stop", UsageReported: reported}, nil
		})
		_, _, err := rn.respond(context.Background(), r, llm.ToolChoiceAuto)
		if err != nil {
			t.Fatal(err)
		}
		if got := recorder.items[1].Usage != nil; got != reported {
			t.Fatalf("reported=%v: usage present=%v", reported, got)
		}
	}
}

func TestToolCarrierSurvivesExecutionAndSubsequentCalls(t *testing.T) {
	modelCalls := 0
	var prompts []llm.ChatRequest
	rn, r, recorder := statsFixture(t, func(_ context.Context, req llm.ChatRequest, _ func(llm.Delta)) (llm.ChatResponse, error) {
		modelCalls++
		prompts = append(prompts, req)
		resp := llm.ChatResponse{UsageReported: true, Usage: llm.Usage{PromptTokens: int64(100 * modelCalls), CompletionTokens: int64(10 * modelCalls)}, FinishReason: "stop"}
		if modelCalls == 1 {
			resp.Message.ToolCalls, resp.FinishReason = statsCalls(), "tool_calls"
		} else {
			resp.Message.Content = "All done"
		}
		return resp, nil
	})
	toolCalls := 0
	rn.tools = statsTools{onInvoke: func() {
		toolCalls++
		// Both model calls must exist in storage before execution of either.
		if len(recorder.items) != 3 {
			t.Fatalf("tool execution began before all calls were recorded: %d items", len(recorder.items))
		}
	}}
	result, err := rn.Run(context.Background(), r.agent, Turn{ID: r.turnID, Items: r.items, Input: statsInput{}}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if modelCalls != 2 || toolCalls != 2 {
		t.Fatalf("model calls=%d tool calls=%d", modelCalls, toolCalls)
	}
	first, last := recorder.items[1], recorder.items[3]
	if first.ResponseID == last.ResponseID {
		t.Fatal("separate responses share an ID")
	}
	assertStatsCarrier(t, recorder.items, first.ResponseID, first.ContextStats, &thread.Usage{InputTokens: 100, OutputTokens: 10})
	assertStatsCarrier(t, recorder.items, last.ResponseID, last.ContextStats, &thread.Usage{InputTokens: 200, OutputTokens: 20})
	if first.Status != thread.ItemCompleted || first.Result == nil {
		t.Fatalf("tool did not complete: %+v", first)
	}
	if last.ContextStats.MessagesTokens <= first.ContextStats.MessagesTokens {
		t.Fatal("later call did not retain its own larger transcript estimate")
	}
	if result.Usage != (thread.Usage{InputTokens: 300, OutputTokens: 30}) {
		t.Fatalf("turn usage was duplicated or dropped: %+v", result.Usage)
	}
	encoded, err := json.Marshal(prompts[1].Messages)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "contextStats") || strings.Contains(string(encoded), "budgetTokens") {
		t.Fatal("item statistics leaked into model history")
	}
}

func TestToolCarrierSurvivesApprovalAndResume(t *testing.T) {
	modelCalls := 0
	rn, r, recorder := statsFixture(t, func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error) {
		modelCalls++
		resp := llm.ChatResponse{UsageReported: true, Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 10}, FinishReason: "stop"}
		if modelCalls == 1 {
			resp.Message.ToolCalls, resp.FinishReason = statsCalls()[:1], "tool_calls"
		} else {
			resp.Message.Content = "Approved operation completed"
		}
		return resp, nil
	})
	r.agent.ApprovalPolicy = ApprovalOnRequest
	firstRun, err := rn.Run(context.Background(), r.agent, Turn{ID: r.turnID, Items: r.items, Input: statsInput{}}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if firstRun.Pending == nil || firstRun.Pending.ContextStats == nil || firstRun.Pending.Usage == nil {
		t.Fatalf("approval item lost model stats: %+v", firstRun.Pending)
	}
	pending := *firstRun.Pending
	wantStats, wantUsage := *pending.ContextStats, *pending.Usage
	pending.Approval.Decision = thread.DecisionAccept
	if _, err := recorder.Record(pending); err != nil {
		t.Fatal(err)
	}
	secondRun, err := rn.Run(context.Background(), r.agent, Turn{ID: r.turnID, Items: recorder.items, Input: statsInput{}}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	first := assertStatsCarrier(t, recorder.items, pending.ResponseID, &wantStats, &wantUsage)
	if first.Status != thread.ItemCompleted || first.Result == nil {
		t.Fatalf("approved carrier did not complete: %+v", first)
	}
	if firstRun.Usage != wantUsage || secondRun.Usage != wantUsage {
		t.Fatalf("resume recounted prior usage: first=%+v second=%+v", firstRun.Usage, secondRun.Usage)
	}
}

func TestCompactionOwnsItsRequestStats(t *testing.T) {
	for _, failed := range []bool{false, true} {
		rn, r, recorder := statsFixture(t, func(_ context.Context, req llm.ChatRequest, _ func(llm.Delta)) (llm.ChatResponse, error) {
			if req.ToolChoice != llm.ToolChoiceNone {
				t.Fatal("compaction allowed tools")
			}
			resp := llm.ChatResponse{Message: llm.ChatMessage{Content: "Short summary"}, FinishReason: "stop", UsageReported: true, Usage: llm.Usage{PromptTokens: 123, CompletionTokens: 15}}
			if failed {
				return resp, errors.New("summary stream interrupted")
			}
			return resp, nil
		})
		prompt, err := rn.contexts.CompactionPrompt(rn.contextInput(r))
		if err != nil {
			t.Fatal(err)
		}
		err = rn.compact(context.Background(), r)
		if (err != nil) != failed {
			t.Fatalf("failed=%v, err=%v", failed, err)
		}
		first := assertStatsCarrier(t, recorder.items, recorder.items[1].ResponseID, contextStats(prompt.Stats), &thread.Usage{InputTokens: 123, OutputTokens: 15})
		if first.Type != thread.ItemContextCompaction {
			t.Fatalf("wrong compaction carrier: %s", first.Type)
		}
		if r.modelCalls() != 0 {
			t.Fatal("compaction consumed the normal response call limit")
		}
		if recorder.writes[1].ContextStats == nil || recorder.writes[1].Usage != nil {
			t.Fatal("compaction did not persist input estimates before sending")
		}
		if failed && first.Status != thread.ItemFailed || !failed && first.Status != thread.ItemCompleted {
			t.Fatalf("incorrect compaction status: %s", first.Status)
		}
	}
}

func TestAutomaticCompactionAndAnswerHaveDistinctStats(t *testing.T) {
	modelCalls := 0
	rn, r, recorder := statsFixture(t, func(_ context.Context, req llm.ChatRequest, _ func(llm.Delta)) (llm.ChatResponse, error) {
		modelCalls++
		resp := llm.ChatResponse{Message: llm.ChatMessage{Content: "A short result"}, FinishReason: "stop", UsageReported: true, Usage: llm.Usage{PromptTokens: int64(100 * modelCalls), CompletionTokens: 10}}
		if modelCalls == 1 && req.ToolChoice != llm.ToolChoiceNone {
			t.Fatal("first request should compact context")
		}
		return resp, nil
	})
	rn.contexts = contextmanager.New(6000)
	r.items[0].Text = strings.Repeat("Long previous history. ", 1800)
	if _, err := r.record(thread.NewUserMessage("latest", r.turnID, "Continue", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	_, responseID, err := rn.respond(context.Background(), r, llm.ToolChoiceAuto)
	if err != nil {
		t.Fatal(err)
	}
	if modelCalls != 2 || r.modelCalls() != 1 {
		t.Fatalf("wrong request counts: sent=%d normal=%d", modelCalls, r.modelCalls())
	}
	compaction, answer := recorder.items[2], recorder.items[3]
	assertStatsCarrier(t, recorder.items, compaction.ResponseID, compaction.ContextStats, &thread.Usage{InputTokens: 100, OutputTokens: 10})
	assertStatsCarrier(t, recorder.items, responseID, answer.ContextStats, &thread.Usage{InputTokens: 200, OutputTokens: 10})
	if compaction.ResponseID == responseID || compaction.ContextStats.EstimatedTokens == answer.ContextStats.EstimatedTokens {
		t.Fatal("compaction and answer must preserve separate request snapshots")
	}
	if r.usage != (thread.Usage{InputTokens: 300, OutputTokens: 20}) {
		t.Fatalf("compaction usage omitted or recounted: %+v", r.usage)
	}
}
