package contextmanager

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0xfakeSpike/ollupi/thread"
)

func TestMemoryIsCurrentTurnReferenceAndSubsetOfMessages(t *testing.T) {
	m := New(64000)
	memory := []thread.MemoryReference{{ID: "memory-1", Revision: 3, Text: "Only macOS is in scope."}}
	in := Input{Rules: "Follow the user's request.", CurrentTurnID: "current", Items: []thread.Item{
		{TurnID: "old", Type: thread.ItemUserMessage, Text: "Earlier request"},
		{TurnID: "old", Type: thread.ItemMemoryContext, Status: thread.ItemCompleted, Memory: []thread.MemoryReference{{ID: "old", Text: "OBSOLETE_MEMORY"}}},
		{TurnID: "old", Type: thread.ItemAgentMessage, Status: thread.ItemCompleted, ResponseID: "old-response", Text: "Earlier response"},
		{TurnID: "current", Type: thread.ItemUserMessage, Text: "Add Windows support now."},
		{TurnID: "current", Type: thread.ItemMemoryContext, Status: thread.ItemCompleted, Memory: memory},
	}}
	before, err := m.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Memory = memory
	after, err := m.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if before.System != after.System || len(after.Messages) != len(before.Messages)+1 {
		t.Fatal("memory changed the system prefix or added multiple messages")
	}
	if !strings.Contains(after.Messages[2].Content, memory[0].Text) || after.Messages[3].Content != "Add Windows support now." {
		t.Fatalf("memory must precede the current user request: %+v", after.Messages)
	}
	if strings.Contains(prettyJSON(after.Messages), "OBSOLETE_MEMORY") || strings.Count(prettyJSON(after.Messages), memory[0].Text) != 1 {
		t.Fatal("historical memory was replayed or the selection was duplicated")
	}
	s := after.Stats
	if s.MemoryTokens <= 0 || s.MemoryTokens != s.MessagesTokens-before.Stats.MessagesTokens || s.MemoryTokens > s.MessagesTokens {
		t.Fatalf("memory is not a subset of messages: %+v", s)
	}
	if s.EstimatedTokens != s.SystemTokens+s.MessagesTokens+s.ToolsTokens {
		t.Fatalf("memory was counted twice: %+v", s)
	}
	compaction, err := m.CompactionPrompt(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prettyJSON(compaction.Messages), memory[0].Text) || compaction.Stats.MemoryTokens != 0 {
		t.Fatal("memory leaked into compaction input")
	}
}

func TestMemorySurvivesCompactionWithoutDuplicatingHistory(t *testing.T) {
	m := New(64000)
	memory := []thread.MemoryReference{{ID: "memory", Revision: 2, Text: "PERSISTENT_REFERENCE"}}
	in := Input{CurrentTurnID: "current", Memory: memory, Items: []thread.Item{
		{TurnID: "current", Type: thread.ItemUserMessage, Text: "Current request"},
		{TurnID: "current", Type: thread.ItemMemoryContext, Status: thread.ItemCompleted, Memory: memory},
		{TurnID: "current", Type: thread.ItemContextCompaction, Status: thread.ItemCompleted, Text: "COMPACTED_HISTORY"},
	}}
	prompt, err := m.Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt.Messages) != 2 || !strings.Contains(prompt.Messages[0].Content, memory[0].Text) || !strings.Contains(prompt.Messages[1].Content, "COMPACTED_HISTORY") {
		t.Fatalf("compaction lost the frozen memory: %+v", prompt.Messages)
	}
	if strings.Count(prettyJSON(prompt.Messages), memory[0].Text) != 1 || prompt.NeedsCompaction {
		t.Fatal("snapshot replay creates duplicates or a compaction loop")
	}
	// Metadata appended after a compaction alone is not new conversation.
	in.Items = append(in.Items, thread.Item{TurnID: "current", Type: thread.ItemMemoryContext, Status: thread.ItemCompleted})
	if hasItemsAfterCompaction(in.Items) {
		t.Fatal("a memory snapshot is not compactable conversation")
	}
}

func TestMemoryBudgetPreservesWholeEntriesAndIncludesFraming(t *testing.T) {
	m := New(10000)
	if m.MemoryBudget() != 500 || New(64000).MemoryBudget() != 2000 {
		t.Fatal("memory budget must be 5 percent, capped at 2000")
	}
	oversized := thread.MemoryReference{ID: "huge", Revision: 1, Text: strings.Repeat("约束", 1000)}
	small := thread.MemoryReference{ID: "small", Revision: 1, Text: "Answer in Chinese."}
	selected := m.BoundMemory([]thread.MemoryReference{oversized, small})
	if !reflect.DeepEqual(selected, []thread.MemoryReference{small}) {
		t.Fatalf("entry was truncated or later fitting entries were dropped: %+v", selected)
	}
	if estimateTokens(prettyJSON([]Message{memoryMessage(selected)})) > m.MemoryBudget() {
		t.Fatal("rendered memory exceeded its allocation")
	}
	if len(New(100).BoundMemory([]thread.MemoryReference{small})) != 0 {
		t.Fatal("a budget too small for framing must omit memory")
	}
}
