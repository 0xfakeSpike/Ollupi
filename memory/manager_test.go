package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/0xfakeSpike/ollupi/llm"
)

type testStore struct {
	batch     *Batch
	entries   []Entry
	committed []Operation
	report    Report
	commits   int
	failure   string
	commitErr error
	readErr   error
	limits    []int
	deferred  int
}

func (s *testStore) MemoryNextBatch(_ context.Context, n int) (*Batch, error) {
	s.limits = append(s.limits, n)
	return s.batch, s.readErr
}
func (s *testStore) MemoryCommit(_ context.Context, _ *Batch, ops []Operation, r Report) error {
	if s.commitErr != nil {
		return s.commitErr
	}
	s.commits++
	s.committed = ops
	s.report = r
	s.batch = nil
	return nil
}
func (s *testStore) MemoryFailure(_ context.Context, e string) error { s.failure = e; return nil }
func (s *testStore) MemoryDefer(_ context.Context, _ *Batch, _ string) error {
	s.deferred++
	return nil
}
func (s *testStore) MemoryList(context.Context, string, bool) ([]Entry, error) {
	return s.entries, s.readErr
}
func (s *testStore) MemoryStatus(context.Context) (Status, error) { return Status{}, nil }

type modelFunc func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error)

func (f modelFunc) Chat(c context.Context, r llm.ChatRequest, d func(llm.Delta)) (llm.ChatResponse, error) {
	return f(c, r, d)
}
func fixtureBatch() *Batch {
	return &Batch{WorkspaceID: "project-a", Changes: []Change{{ID: 1, Version: 2}}, Facts: []Fact{{Source: Source{Table: "items", ID: "request-1", Digest: "v1", Operation: "upsert"}, Data: json.RawMessage(`{"text":"只做 macOS 应用，无需移动端适配","type":"userMessage"}`)}}}
}
func response(ops []Operation) llm.ChatResponse {
	b, _ := json.Marshal(map[string]any{"operations": ops})
	return llm.ChatResponse{Message: llm.ChatMessage{Content: string(b)}, FinishReason: "stop", UsageReported: true, Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 20}}
}
func addFor(b *Batch) Operation {
	return Operation{Action: "add", Topic: "客户端平台", Kind: "decision", Text: "当前只开发 macOS 桌面应用，无需移动端适配。", Sources: []Source{b.Facts[0].Source}}
}

func TestExtractionIsIndependentToolFreeAndAcknowledgesOnlyValidResponse(t *testing.T) {
	ctx := context.Background()
	batch := fixtureBatch()
	store := &testStore{batch: batch}
	called := 0
	model := modelFunc(func(_ context.Context, r llm.ChatRequest, _ func(llm.Delta)) (llm.ChatResponse, error) {
		called++
		if r.Model != "memory-model" || r.ModelProfileID != "local" || r.ToolChoice != llm.ToolChoiceNone || len(r.Tools) != 0 {
			t.Fatalf("request routing/tools: %+v", r)
		}
		if len(r.Messages) != 2 || r.Messages[0].Role != llm.RoleSystem || !strings.Contains(r.Messages[1].Content, "request-1") {
			t.Fatal("missing scoped evidence")
		}
		return response([]Operation{addFor(batch)}), nil
	})
	manager, err := New(store, model, Options{Generate: true, Use: true, Model: "memory-model", ModelProfileID: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if called != 1 || store.commits != 1 || len(store.committed) != 1 || !store.report.UsageReported || store.report.EstimatedInputTokens == 0 {
		t.Fatalf("bad commit: %+v", store)
	}
	if err = manager.RunOnce(ctx); err != nil || called != 1 {
		t.Fatal("empty queue called the model")
	}
}

func TestFailuresLeaveEvidencePendingAndCanRecoverAfterRestart(t *testing.T) {
	for _, kind := range []string{"model", "json", "source", "revision", "tool", "truncated", "conflict"} {
		t.Run(kind, func(t *testing.T) {
			b := fixtureBatch()
			s := &testStore{batch: b}
			secret := "sk-" + strings.Repeat("a", 40)
			model := modelFunc(func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error) {
				r := response([]Operation{addFor(b)})
				switch kind {
				case "model":
					return r, errors.New(secret)
				case "json":
					r.Message.Content = "{}"
				case "source":
					op := addFor(b)
					op.Sources[0].ID = "invented"
					r = response([]Operation{op})
				case "revision":
					op := addFor(b)
					op.Action = "update"
					op.ID = "missing"
					op.Revision = 99
					r = response([]Operation{op})
				case "tool":
					r.Message.ToolCalls = []llm.ToolCall{{Name: "shell"}}
				case "truncated":
					r.FinishReason = "length"
				case "conflict":
					s.commitErr = ErrConflict
				}
				return r, nil
			})
			m, _ := New(s, model, Options{Generate: true, Model: "test"})
			err := m.RunOnce(context.Background())
			if err == nil || s.commits != 0 || s.batch == nil {
				t.Fatalf("failure consumed evidence: %v", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(s.failure, secret) {
				t.Fatal("provider credential leaked")
			}
			s.commitErr = nil
			fresh, _ := New(s, modelFunc(func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error) {
				return response([]Operation{}), nil
			}), Options{Generate: true, Model: "test"})
			if err = fresh.RunOnce(context.Background()); err != nil || s.commits != 1 {
				t.Fatalf("retry failed: %v", err)
			}
		})
	}
}

func TestExtractorSeesOldNotesAndRedactedEvidence(t *testing.T) {
	b := fixtureBatch()
	key := "sk-" + strings.Repeat("z", 40)
	b.Facts[0].Data = json.RawMessage(`{"api_key":"hidden-key","text":"key ` + key + `"}`)
	b.Existing = []Entry{{ID: "old", WorkspaceID: b.WorkspaceID, Topic: "平台", Kind: "decision", Text: "只做 macOS", Status: "active", Revision: 2, Sources: []Source{b.Facts[0].Source}}}
	payload, _, err := extractionInput(b, 12000)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, key) || strings.Contains(payload, "hidden-key") || !strings.Contains(payload, "[credential redacted]") || !strings.Contains(payload, `"revision":2`) {
		t.Fatalf("unexpected sanitized prompt: %s", payload)
	}
	op := addFor(b)
	op.Action = "update"
	op.ID = "old"
	op.Revision = 2
	if _, err = parseOperations(response([]Operation{op}).Message.Content, b); err != nil {
		t.Fatal(err)
	}
	op.Text = "retain " + key
	if _, err = parseOperations(response([]Operation{op}).Message.Content, b); err == nil {
		t.Fatal("accepted credential in memory")
	}
}

func TestRecallScopesChineseRelevanceAndWholeNoteBudget(t *testing.T) {
	entries := []Entry{
		{ID: "global", Kind: "preference", Text: "回复使用中文。", Status: "active"},
		{ID: "project", WorkspaceID: "a", Kind: "fact", Topic: "数据库", Text: "数据库采用 SQLite，写事务保持简短。", Status: "active"},
		{ID: "other", WorkspaceID: "b", Kind: "preference", Text: "其他项目的机密。", Status: "active"},
		{ID: "forgotten", WorkspaceID: "a", Kind: "preference", Text: "已经忘记的内容。", Status: "forgotten"},
		{ID: "retired", WorkspaceID: "a", Kind: "preference", Text: "过期内容。", Status: "retired"},
		{ID: "large", WorkspaceID: "a", Kind: "decision", Text: strings.Repeat("大", 2000), Status: "active"},
	}
	s := &testStore{entries: entries}
	m, _ := New(s, nil, Options{Use: true})
	got, err := m.Recall(context.Background(), "a", "数据库怎样配置？", 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("recall = %+v", got)
	}
	for _, e := range got {
		if e.ID != "global" && e.ID != "project" {
			t.Fatalf("cross-scope or nonactive entry %s", e.ID)
		}
	}
	got, err = m.Recall(context.Background(), "", "数据库", 400)
	if err != nil || len(got) != 1 || got[0].ID != "global" {
		t.Fatalf("General leaked scope: %+v", got)
	}
}

func TestWorkerCancellationAndIndependentDisableFlags(t *testing.T) {
	s := &testStore{batch: fixtureBatch()}
	started := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ llm.ChatRequest, _ func(llm.Delta)) (llm.ChatResponse, error) {
		close(started)
		<-ctx.Done()
		return llm.ChatResponse{}, ctx.Err()
	})
	m, _ := New(s, model, Options{Generate: true, Model: "test", Interval: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("worker never started")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker ignored shutdown")
	}
	if s.commits != 0 || s.failure != "" {
		t.Fatal("shutdown advanced evidence or saved provider error")
	}
	disabled, _ := New(s, nil, Options{})
	if err := disabled.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ := disabled.MemoryStatus(context.Background())
	if *status.GenerationEnabled || *status.UseEnabled {
		t.Fatal("disable flags lost")
	}
}

func TestForgottenEvidenceIsNotResentAndManualEditsAreProtected(t *testing.T) {
	b := fixtureBatch()
	forgottenSource := b.Facts[0].Source
	b.Existing = []Entry{{ID: "forgotten", Status: "forgotten", Topic: "platform", Sources: []Source{forgottenSource, {Table: "workspaces", ID: b.WorkspaceID, Digest: "workspace-v1"}}}}
	b.Facts = append(b.Facts, Fact{Source: Source{Table: "workspaces", ID: b.WorkspaceID, Digest: "workspace-v1"}, Data: json.RawMessage(`{"name":"Ollupi"}`)})
	visible := visibleEvidence(b)
	if len(visible.Facts) != 1 || visible.Facts[0].Source.Table != "workspaces" {
		t.Fatalf("forgotten source leaked or unrelated scope suppressed: %+v", visible.Facts)
	}
	b.Existing = []Entry{{ID: "manual", WorkspaceID: b.WorkspaceID, Status: "active", Revision: 2, UserEdited: true, Topic: "platform", Kind: "decision", Text: "manual wording", Sources: []Source{forgottenSource}}}
	op := addFor(b)
	op.Action, op.ID, op.Revision = "update", "manual", 2
	ops, err := parseOperations(response([]Operation{op}).Message.Content, b)
	if err != nil || len(ops) != 0 {
		t.Fatalf("manual edit would be overwritten: %+v %v", ops, err)
	}
}

func TestUnprocessableEvidenceIsDeferredWithoutCallingTheModel(t *testing.T) {
	b := fixtureBatch()
	raw, _ := json.Marshal(map[string]any{"text": strings.Repeat("中文证据", 3000)})
	b.Facts[0].Data = raw
	s := &testStore{batch: b}
	m, _ := New(s, modelFunc(func(context.Context, llm.ChatRequest, func(llm.Delta)) (llm.ChatResponse, error) {
		t.Fatal("oversized request was sent")
		return llm.ChatResponse{}, nil
	}), Options{Generate: true, Model: "test", InputTokens: 2048})
	if err := m.RunOnce(context.Background()); err == nil || s.deferred != 1 || s.batch == nil || s.commits != 0 {
		t.Fatalf("oversized receipt lost or not deferred: %+v %v", s, err)
	}
	if s.limits[len(s.limits)-1] != 1 {
		t.Fatalf("did not try smaller batches: %v", s.limits)
	}
}

func TestCredentialsInsideJSONStringArgumentsAndTruncatedExcerpts(t *testing.T) {
	raw := json.RawMessage(`{"arguments":"{\"api_key\":\"custom-secret-12345\",\"password\":\"short\"}","excerpt":"{\"access_token\":\"custom-token-12345678\", truncated"}`)
	clean, err := sanitizeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"custom-secret-12345", "custom-token-12345678", "short"} {
		if strings.Contains(string(clean), secret) {
			t.Fatalf("nested credential not removed: %s", clean)
		}
	}
}
