package contextmanager

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0xfakeSpike/ollupi/thread"
)

func TestPromptStatsBreakdown(t *testing.T) {
	m := New(64000)
	in := Input{
		Rules: "Answer clearly. 使用中文。", ProjectInstructions: "Use Go.",
		Items: []thread.Item{{Type: thread.ItemUserMessage, Text: "Explain the context budget 上下文预算"}},
		Tools: []map[string]any{{"name": "read_file", "description": "Read project files"}},
	}
	for _, build := range []struct {
		name string
		fn   func(Input) (Prompt, error)
	}{{"normal", m.Build}, {"compaction", m.CompactionPrompt}} {
		t.Run(build.name, func(t *testing.T) {
			p, err := build.fn(in)
			if err != nil {
				t.Fatal(err)
			}
			s := p.Stats
			if s.BudgetTokens != 64000 || s.SystemTokens <= 0 || s.MessagesTokens <= 0 || s.ToolsTokens <= 0 {
				t.Fatalf("incomplete breakdown: %+v", s)
			}
			if s.EstimatedTokens != s.SystemTokens+s.MessagesTokens+s.ToolsTokens {
				t.Fatalf("breakdown does not sum to total: %+v", s)
			}
			// The breakdown describes the rendered request, including the
			// extra summary instruction on compaction requests.
			if s.SystemTokens != estimateTokens(p.System) || s.MessagesTokens != estimateTokens(prettyJSON(p.Messages)) || s.ToolsTokens != estimateTokens(prettyJSON(in.Tools)) {
				t.Fatalf("stats do not describe final prompt: %+v", s)
			}
		})
	}
	// Exposing components must preserve the existing 80% compaction trigger.
	in.Items[0].Text = strings.Repeat("a", 4000)
	p, err := New(1000).Build(in)
	if err != nil || !p.NeedsCompaction {
		t.Fatalf("compaction threshold changed: %+v, %v", p.Stats, err)
	}
}

func TestItemStatisticsDoNotEnterModelContext(t *testing.T) {
	m := New(64000)
	items := []thread.Item{
		{Type: thread.ItemUserMessage, Text: "hello"},
		{Type: thread.ItemReasoning, Status: thread.ItemCompleted, ResponseID: "response", Text: "thinking"},
		{Type: thread.ItemAgentMessage, Status: thread.ItemCompleted, ResponseID: "response", Text: "answer"},
		{Type: thread.ItemTodoList, Status: thread.ItemCompleted, ResponseID: "response", CallID: "call", Tool: "update_plan", Arguments: `{}`},
		{Type: thread.ItemError, Text: "Tool use stopped."},
	}
	before, err := m.Build(Input{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	stats := &thread.ContextStats{BudgetTokens: 64000, SystemTokens: 900001, MessagesTokens: 900002, ToolsTokens: 900003, EstimatedTokens: 2700006}
	items[1].ContextStats = stats
	items[1].Usage = &thread.Usage{InputTokens: 987654321, OutputTokens: 123456789}
	// A failed request is durable diagnostics, not another user instruction.
	items = append(items, thread.Item{Type: thread.ItemError, Status: thread.ItemFailed, ResponseID: "failed", Text: "model request failed", ContextStats: stats})
	after, err := m.Build(Input{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("statistics changed model input: before=%+v after=%+v", before, after)
	}
	// A compaction's stats are metadata too; only its summary is replayed.
	items = append(items, thread.Item{Type: thread.ItemContextCompaction, Status: thread.ItemCompleted, ResponseID: "compaction", Text: "summary"})
	before, err = m.Build(Input{Items: items})
	if err != nil {
		t.Fatal(err)
	}
	items[len(items)-1].ContextStats = stats
	items[len(items)-1].Usage = &thread.Usage{InputTokens: 777777}
	after, err = m.Build(Input{Items: items})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("compaction stats changed model input: %+v, %v", after, err)
	}
}
