package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/contextmanager"
	"github.com/0xfakeSpike/ollupi/agent/permission"
	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/pkg/redact"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

type fakeProvider struct {
	mu        sync.Mutex
	calls     []string
	active    int
	maxActive int
	specs     []*tools.Spec
	results   map[string]tools.Result
}

func (p *fakeProvider) Name() (string, string) { return "fake", "fake tools for tests" }

func (p *fakeProvider) ToolList(context.Context) ([]*tools.Spec, error) {
	return p.specs, nil
}

func (p *fakeProvider) Invoke(_ context.Context, tool string, _ map[string]any) (tools.Result, error) {
	p.mu.Lock()
	p.active++
	p.maxActive = max(p.maxActive, p.active)
	p.mu.Unlock()
	time.Sleep(time.Millisecond)
	p.mu.Lock()
	p.calls = append(p.calls, tool)
	p.active--
	p.mu.Unlock()
	if result, ok := p.results[tool]; ok {
		return result, nil
	}
	return tools.Result{Output: "ok"}, nil
}

func (p *fakeProvider) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func spec(name string, readOnly bool) *tools.Spec {
	return &tools.Spec{
		Name: name, Description: name, InputSchema: map[string]any{"type": "object"},
		Annotations: &tools.Annotations{ReadOnlyHint: readOnly},
	}
}

type recordSink struct {
	mu     sync.Mutex
	items  []repo.ItemRow
	deltas int
}

func (s *recordSink) record(it repo.ItemRow) (repo.ItemRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == it.ID {
			s.items[i] = it
			return it, nil
		}
	}
	s.items = append(s.items, it)
	return it, nil
}

func (s *recordSink) delta(repo.ItemRow, string) {
	s.mu.Lock()
	s.deltas++
	s.mu.Unlock()
}

func (s *recordSink) ofType(kind repo.ItemType) []repo.ItemRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []repo.ItemRow
	for _, it := range s.items {
		if it.Type == kind {
			out = append(out, it)
		}
	}
	return out
}

func (s *recordSink) lastCall(id string) (repo.ItemRow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.items) - 1; i >= 0; i-- {
		if s.items[i].ID == id {
			return s.items[i], true
		}
	}
	return repo.ItemRow{}, false
}

func newTestRuntime(t *testing.T, provider tools.Provider, policy permission.Policy, rules permission.Rules) (*Runtime, *recordSink) {
	t.Helper()
	registry, err := tools.New(context.Background(), provider)
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	checker, err := permission.New(policy, rules, registry)
	if err != nil {
		t.Fatalf("build permission checker: %v", err)
	}
	contexts := contextmanager.New("gpt-4o", llm.Limits{Context: 128000, Output: 16384})
	sink := &recordSink{}
	rt := &Runtime{
		Registry:    registry,
		Contexts:    contexts,
		Turn:        repo.TurnRow{ID: uuid.NewString(), Model: llm.Model{Name: "gpt-4o"}, Status: repo.TurnInProgress},
		Caller:      tools.Caller{},
		Prompt:      "assistant",
		Tools:       registry.Descriptors(),
		Permissions: checker,
		Record:      sink.record,
		Delta:       sink.delta,
		redactor:    redact.New(),
		system:      "assistant",
	}
	return rt, sink
}

func callItem(rt *Runtime, id, tool string, readOnly bool, set map[string]any) repo.ItemRow {
	now := time.Now().UTC()
	it := repo.ItemRow{
		ID: id, TurnID: rt.Turn.ID, Type: repo.ItemToolCall, Status: repo.ItemInProgress,
		CallID: "call-" + id, ResponseID: "resp-" + id, Tool: tool, Arguments: "{}", StartedAt: now,
		Annotations: &tools.Annotations{ReadOnlyHint: readOnly},
	}
	for key, value := range set {
		switch key {
		case "arguments":
			it.Arguments = value.(string)
		case "approval":
			it.Approval = value.(*repo.Approval)
		}
	}
	return it
}

func TestPlanBatchesReadOnlyCalls(t *testing.T) {
	provider := &fakeProvider{specs: []*tools.Spec{spec("read1", true), spec("read2", true)}}
	rt, _ := newTestRuntime(t, provider, permission.OnRequest, permission.Rules{})
	rt.Items = []repo.ItemRow{
		callItem(rt, uuid.NewString(), "read1", true, nil),
		callItem(rt, uuid.NewString(), "read2", true, nil),
	}
	plan, err := rt.planCalls(context.Background())
	if err != nil || len(plan.actions) != 2 {
		t.Fatalf("plan: actions=%+v err=%v", plan.actions, err)
	}
	for _, action := range plan.actions {
		if action.kind != actionRunReadOnly || action.job == nil {
			t.Fatalf("expected read-only run actions, got %+v", action)
		}
	}
	outcome, done, err := rt.executePlan(context.Background(), plan)
	if err != nil || done || outcome != Continue {
		t.Fatalf("execute: outcome=%v done=%v err=%v", outcome, done, err)
	}
	if calls := provider.recorded(); len(calls) != 2 || provider.maxActive != 2 {
		t.Fatalf("read-only calls were not parallel: calls=%v maxActive=%d", calls, provider.maxActive)
	}
}

func TestSerialCallFlushesReadOnlyBatch(t *testing.T) {
	provider := &fakeProvider{specs: []*tools.Spec{spec("read1", true), spec("write", false), spec("read2", true)}}
	rt, _ := newTestRuntime(t, provider, permission.Never, permission.Rules{})
	rt.Items = []repo.ItemRow{
		callItem(rt, uuid.NewString(), "read1", true, nil),
		callItem(rt, uuid.NewString(), "write", false, nil),
		callItem(rt, uuid.NewString(), "read2", true, nil),
	}
	plan, err := rt.planCalls(context.Background())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.actions) != 3 || plan.actions[0].kind != actionRunReadOnly || plan.actions[1].kind != actionRun || plan.actions[2].kind != actionRunReadOnly {
		t.Fatalf("unexpected plan: %+v", plan.actions)
	}
	if _, _, err := rt.executePlan(context.Background(), plan); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got, want := fmt.Sprint(provider.recorded()), "[read1 write read2]"; got != want {
		t.Fatalf("execution order = %s, want %s", got, want)
	}
}

func TestApprovalWaitStopsLaterCalls(t *testing.T) {
	provider := &fakeProvider{specs: []*tools.Spec{spec("read", true), spec("ask", false)}}
	rt, sink := newTestRuntime(t, provider, permission.OnRequest, permission.Rules{})
	askID := uuid.NewString()
	rt.Items = []repo.ItemRow{
		callItem(rt, uuid.NewString(), "read", true, nil),
		callItem(rt, askID, "ask", false, nil),
		callItem(rt, uuid.NewString(), "read", true, nil),
	}
	outcome, done, err := rt.drain(context.Background())
	if err != nil || !done || outcome != Wait {
		t.Fatalf("drain: outcome=%v done=%v err=%v", outcome, done, err)
	}
	if calls := provider.recorded(); len(calls) != 1 || calls[0] != "read" {
		t.Fatalf("expected only the read-only call to run, got %v", calls)
	}
	ask, ok := sink.lastCall(askID)
	if !ok || ask.Approval == nil || ask.Status != repo.ItemInProgress {
		t.Fatalf("approval call was not left waiting: %+v", ask)
	}
}

func TestDecidedInvalidArgumentsFailItem(t *testing.T) {
	provider := &fakeProvider{specs: []*tools.Spec{spec("ask", false)}}
	rt, sink := newTestRuntime(t, provider, permission.Untrusted, permission.Rules{})
	it := callItem(rt, uuid.NewString(), "ask", false, map[string]any{"arguments": "{"})
	it.Approval = &repo.Approval{Decision: repo.DecisionAccept}
	rt.Items = []repo.ItemRow{it}
	_, _, job, err := rt.decided(it)
	if err != nil || job != nil {
		t.Fatalf("decided: job=%+v err=%v", job, err)
	}
	stored, ok := sink.lastCall(it.ID)
	if !ok || stored.Status != repo.ItemFailed || stored.Error == "" {
		t.Fatalf("invalid approved call was not failed in place: %+v", stored)
	}
}

func TestToolCallsCountsRefusedCalls(t *testing.T) {
	provider := &fakeProvider{specs: nil}
	rt, _ := newTestRuntime(t, provider, permission.OnRequest, permission.Rules{})
	now := time.Now().UTC()
	rt.Items = []repo.ItemRow{
		{ID: uuid.NewString(), TurnID: rt.Turn.ID, Type: repo.ItemToolCall, Status: repo.ItemFailed, StartedAt: now, CompletedAt: &now},
		callItem(rt, uuid.NewString(), "unoffered", false, nil),
	}
	if n := rt.toolCalls(); n != 1 {
		t.Fatalf("refused call not counted: got %d", n)
	}
}

func TestBatchHaltSkipsLaterCalls(t *testing.T) {
	provider := &fakeProvider{
		specs:   []*tools.Spec{spec("read", true), spec("write", false)},
		results: map[string]tools.Result{"read": {Err: fmt.Errorf("boom")}},
	}
	rt, sink := newTestRuntime(t, provider, permission.Never, permission.Rules{})
	server := newSSEStream(t,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
	rt.LLM = openaiFor(t, server.URL)
	var items []repo.ItemRow
	for range maxConsecutiveFailures {
		items = append(items, callItem(rt, uuid.NewString(), "read", true, nil))
	}
	writeID := uuid.NewString()
	readID := uuid.NewString()
	items = append(items,
		callItem(rt, writeID, "write", false, nil),
		callItem(rt, readID, "read", true, nil),
	)
	rt.Items = items
	outcome, done, err := rt.drain(context.Background())
	if err != nil || !done || outcome != Done {
		t.Fatalf("drain: outcome=%v done=%v err=%v", outcome, done, err)
	}
	if calls := provider.recorded(); len(calls) != maxConsecutiveFailures {
		t.Fatalf("expected only the failing batch to run, got %d calls", len(calls))
	}
	for _, id := range []string{writeID, readID} {
		it, ok := sink.lastCall(id)
		if !ok || it.Status != repo.ItemFailed || !strings.Contains(it.Error, "skipped") {
			t.Fatalf("later call %s was not skipped: %+v", id, it)
		}
	}
}

func newSSEStream(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer does not support flushing")
			return
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	t.Cleanup(server.Close)
	return server
}

func openaiFor(t *testing.T, url string) *llm.OpenAI {
	return llm.NewOpenAI(url, "test", map[string]llm.Limits{"gpt-4o": {Context: 128000, Output: 16384}})
}

func TestStepStreamsAnswer(t *testing.T) {
	content := `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`
	finish := `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`
	server := newSSEStream(t, content, finish)
	provider := &fakeProvider{}
	rt, sink := newTestRuntime(t, provider, permission.OnRequest, permission.Rules{})
	rt.LLM = openaiFor(t, server.URL)
	rt.Items = []repo.ItemRow{repo.NewUserMessage(uuid.NewString(), rt.Turn.ID, "hi", time.Now().UTC())}

	outcome, usage, err := rt.Step(context.Background())
	if err != nil || outcome != Done {
		t.Fatalf("step: outcome=%v usage=%+v err=%v", outcome, usage, err)
	}
	agent := sink.ofType(repo.ItemAgentMessage)
	if len(agent) != 1 || agent[0].Text != "Hello" || agent[0].Status != repo.ItemCompleted {
		t.Fatalf("agent message not recorded: %+v", agent)
	}
	if agent[0].ContextStats == nil || agent[0].Usage == nil || agent[0].Usage.InputTokens != 12 {
		t.Fatalf("agent message is not the usage carrier: %+v", agent[0])
	}
	if sink.deltas == 0 {
		t.Fatal("no stream deltas were emitted")
	}
}

func TestStepRunsToolCallAndContinues(t *testing.T) {
	tool := `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"read1","arguments":"{}"}}]},"finish_reason":null}]}`
	finish := `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":2,"total_tokens":14}}`
	server := newSSEStream(t, tool, finish)
	provider := &fakeProvider{specs: []*tools.Spec{spec("read1", true)}}
	rt, sink := newTestRuntime(t, provider, permission.OnRequest, permission.Rules{})
	rt.LLM = openaiFor(t, server.URL)
	rt.Items = []repo.ItemRow{repo.NewUserMessage(uuid.NewString(), rt.Turn.ID, "run it", time.Now().UTC())}

	outcome, _, err := rt.Step(context.Background())
	if err != nil || outcome != Continue {
		t.Fatalf("step: outcome=%v err=%v", outcome, err)
	}
	if calls := provider.recorded(); len(calls) != 1 || calls[0] != "read1" {
		t.Fatalf("tool not executed: %v", calls)
	}
	items := sink.ofType(repo.ItemToolCall)
	if len(items) != 1 || items[0].Status != repo.ItemCompleted || items[0].Result == nil {
		t.Fatalf("tool call not completed with a result: %+v", items)
	}
	if items[0].ContextStats == nil || items[0].Usage == nil {
		t.Fatalf("tool-call-only response has no usage carrier: %+v", items[0])
	}
}
