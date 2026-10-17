// Package contextmanager renders a thread's items into a bounded native chat
// prompt. It owns token estimation, the truncation of tool output sent to the
// model, and the shape of compaction, but never calls an LLM or makes
// control-flow decisions.
//
// The prompt is designed for provider prefix caching: the system message and
// tool list are fixed and the transcript is append-only. When it grows past
// the auto-compact limit, the caller asks the model for a summary (see
// CompactionPrompt) and records it as a contextCompaction item; from then on
// everything before that item is replaced by the summary, as in Codex.
package contextmanager

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/sandbox"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/tools"
)

type Manager struct {
	inputTokens int
}

// New returns a manager for prompts of at most inputTokens estimated tokens.
// The caller supplies a validated positive budget.
func New(inputTokens int) *Manager {
	return &Manager{inputTokens: inputTokens}
}

// autoCompactPercent is the share of the input budget at which the thread
// is compacted, leaving room for the compaction request and the next steps.
const autoCompactPercent = 80

type allocation struct {
	project, skills, userMessages int
}

func (m *Manager) allocation() allocation {
	pct := func(n int) int { return max(256, m.inputTokens*n/100) }
	return allocation{project: pct(6), skills: pct(4), userMessages: pct(10)}
}

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message is one entry of the native chat transcript.
type Message struct {
	Role       string
	Content    string
	Reasoning  string
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolCall is one native function call exactly as the model produced it.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Input struct {
	// Items is every item of the thread, in order.
	Items []thread.Item
	// Memory is the current turn's frozen selection. Historical memoryContext
	// items are never replayed as conversational messages.
	Memory              []thread.MemoryReference
	CurrentTurnID       string
	Rules               string
	ProjectInstructions string
	ExecutionContext    *execution.Context
	Skills              []map[string]any
	// Tools is the tool list sent with the request, counted in the size.
	Tools any
}

// Prompt is the rendered request. Messages exclude the system message.
type Prompt struct {
	System   string
	Messages []Message
	// NeedsCompaction reports that the prompt passed the auto-compact limit
	// and there is something after the last compaction to summarize.
	NeedsCompaction bool
	Stats           Stats
}

type Stats struct {
	BudgetTokens    int
	SystemTokens    int
	MessagesTokens  int
	MemoryTokens    int
	ToolsTokens     int
	EstimatedTokens int
}

func (s Stats) Trace() string {
	return fmt.Sprintf("context tokens=%d/%d system=%d messages=%d memory=%d tools=%d", s.EstimatedTokens, s.BudgetTokens, s.SystemTokens, s.MessagesTokens, s.MemoryTokens, s.ToolsTokens)
}

// Build renders the system message and the chat transcript for one model
// call. It fails when the prompt exceeds the budget and compaction cannot
// help, instead of silently sending an oversized request.
func (m *Manager) Build(in Input) (Prompt, error) {
	system := m.system(in)
	messages, err := m.transcript(in.Items, in.CurrentTurnID, in.Memory)
	if err != nil {
		return Prompt{}, err
	}
	p := Prompt{System: system, Messages: messages}
	p.Stats = m.stats(system, messages, in.Tools)
	if len(in.Memory) > 0 {
		withoutMemory, err := m.transcript(in.Items, "", nil)
		if err != nil {
			return Prompt{}, err
		}
		p.Stats.MemoryTokens = p.Stats.MessagesTokens - estimateTokens(prettyJSON(withoutMemory))
	}
	compactable := hasItemsAfterCompaction(in.Items)
	switch {
	case p.Stats.EstimatedTokens > m.inputTokens*autoCompactPercent/100 && compactable:
		p.NeedsCompaction = true
	case p.Stats.EstimatedTokens > m.inputTokens:
		return Prompt{}, fmt.Errorf("context: the prompt needs %d tokens even after compaction; budget is %d", p.Stats.EstimatedTokens, m.inputTokens)
	}
	return p, nil
}

// compactionInstructions asks the model for a handoff summary (Codex's
// context checkpoint compaction).
const compactionInstructions = `You are performing a CONTEXT CHECKPOINT COMPACTION. Write a handoff summary for another language model that will resume this conversation. Include:
- the user's requests, preferences, and constraints, and the decisions made;
- current progress: what has been done, with the exact facts, ids, paths, and results it produced;
- what remains to be done, as clear next steps.
Be concise and structured. Write in the user's language. Do not call tools.`

// CompactionPrompt is the request that asks the model to summarize the
// thread: the current prompt followed by the compaction instructions. It
// keeps the same system message, so the provider's prompt cache still
// applies; the oldest messages are left out when the request would not fit.
func (m *Manager) CompactionPrompt(in Input) (Prompt, error) {
	system := m.system(in)
	// Long-term memory is injected again from the frozen selection after
	// compaction. Excluding it here prevents summaries from duplicating it.
	messages, err := m.transcript(in.Items, "", nil)
	if err != nil {
		return Prompt{}, err
	}
	instructions := Message{Role: RoleUser, Content: compactionInstructions}
	for {
		p := Prompt{System: system, Messages: append(append([]Message{}, messages...), instructions)}
		p.Stats = m.stats(system, p.Messages, in.Tools)
		if p.Stats.EstimatedTokens <= m.inputTokens {
			return p, nil
		}
		next := nextUserMessage(messages)
		if next < 0 {
			return Prompt{}, fmt.Errorf("context: the compaction request needs %d tokens; budget is %d", p.Stats.EstimatedTokens, m.inputTokens)
		}
		messages = messages[next:]
	}
}

func (m *Manager) system(in Input) string {
	alloc := m.allocation()
	project := "# WORKSPACE INSTRUCTIONS (from AGENTS.md)\n" + noneIfEmpty(clipText(in.ProjectInstructions, alloc.project))
	if in.ExecutionContext != nil {
		c := in.ExecutionContext
		var b strings.Builder
		b.WriteString("# EXECUTION ENVIRONMENT\n")
		if c.RootPath != "" {
			fmt.Fprintf(&b, "Project root: %s\n", c.RootPath)
		}
		fmt.Fprintf(&b, "Default command directory: %s\nCommands run: %s\n", c.DefaultCwd, sandboxDescription(c.Sandbox))
		b.WriteString("Commands use this turn's saved directories. A working directory does not restrict file access; the sandbox described in the run_command tool does.\n")
		for _, instruction := range c.Instructions {
			fmt.Fprintf(&b, "\n# %s INSTRUCTIONS (%s)\n%s\n", strings.ToUpper(instruction.Scope), instruction.Path, instruction.Content)
		}
		// Snapshot instructions are never truncated: partial rules can invert their
		// meaning. The complete system prefix is counted against the context budget.
		project = b.String()
	}
	return strings.Join([]string{strings.TrimSpace(in.Rules), project, "# AVAILABLE SKILLS\n" + noneIfEmpty(clipText(renderSkills(in.Skills), alloc.skills))}, "\n\n")
}

// sandboxDescription names how commands run, for the system prompt.
func sandboxDescription(kind string) string {
	switch kind {
	case sandbox.KindSeatbelt:
		return "inside the macOS sandbox"
	case sandbox.KindUnsandboxed:
		return "without a sandbox"
	default:
		panic(fmt.Sprintf("contextmanager: unknown sandbox kind %q", kind))
	}
}

func (m *Manager) stats(system string, messages []Message, tools any) Stats {
	s := Stats{
		BudgetTokens:   m.inputTokens,
		SystemTokens:   estimateTokens(system),
		MessagesTokens: estimateTokens(prettyJSON(messages)),
		ToolsTokens:    estimateTokens(prettyJSON(tools)),
	}
	s.EstimatedTokens = s.SystemTokens + s.MessagesTokens + s.ToolsTokens
	return s
}

// latestCompaction returns the index of the last completed compaction item,
// or -1.
func latestCompaction(items []thread.Item) int {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == thread.ItemContextCompaction && items[i].Status == thread.ItemCompleted {
			return i
		}
	}
	return -1
}

func hasItemsAfterCompaction(items []thread.Item) bool {
	for _, it := range items[latestCompaction(items)+1:] {
		if it.Type != thread.ItemContextCompaction && it.Type != thread.ItemMemoryContext {
			return true
		}
	}
	return false
}

// summaryPrefix introduces a compaction summary to the model (Codex).
const summaryPrefix = "Another language model started this conversation and produced the summary below. Use it to continue the work without repeating what was already done."

// transcript rebuilds the chat transcript from items. After a compaction,
// everything before it is replaced by one user message: the most recent
// earlier user messages, the summary, and the skill instructions that were
// active. The items of one model response become one assistant message
// followed by one tool message per call.
func (m *Manager) transcript(items []thread.Item, turnID string, memory []thread.MemoryReference) ([]Message, error) {
	var out []Message
	var pendingMemory *Message
	if len(memory) > 0 {
		message := memoryMessage(memory)
		pendingMemory = &message
	}
	appendMemory := func() {
		if pendingMemory != nil {
			out = append(out, *pendingMemory)
			pendingMemory = nil
		}
	}
	start := 0
	if c := latestCompaction(items); c >= 0 {
		// If this turn was already compacted, its user request is in the
		// summary. Put the memory reference before that summary.
		for _, item := range items[:c+1] {
			if item.TurnID == turnID {
				appendMemory()
				break
			}
		}
		out = append(out, Message{Role: RoleUser, Content: m.compactedHistory(items[:c], items[c].Text)})
		start = c + 1
	}
	perObservation := min(maxObservationTokens, max(512, m.inputTokens/8))
	for i := start; i < len(items); {
		it := items[i]
		if it.TurnID == turnID {
			appendMemory()
		}
		switch {
		case it.Type == thread.ItemError && it.ContextStats != nil:
			// A failed model request's diagnostic carrier is not a user
			// message. Keep it in history without replaying it to the model.
			i++
		case it.Type == thread.ItemUserMessage || it.Type == thread.ItemError:
			out = append(out, Message{Role: RoleUser, Content: it.Text})
			i++
		case it.Type == thread.ItemContextCompaction || it.Type == thread.ItemMemoryContext || it.Type == thread.ItemToolProgress:
			// A compaction that did not complete changed nothing.
			i++
		case it.ResponseID != "":
			assistant := Message{Role: RoleAssistant}
			var calls []Message
			j := i
			for ; j < len(items) && items[j].ResponseID == it.ResponseID; j++ {
				part := items[j]
				switch {
				case part.Type == thread.ItemReasoning || part.Type == thread.ItemAgentMessage:
					// Text of a response that failed or was cut off is not
					// part of the conversation.
					if part.Status != thread.ItemCompleted {
						continue
					}
					if part.Type == thread.ItemReasoning {
						assistant.Reasoning += part.Text
					} else {
						assistant.Content += part.Text
					}
				case part.IsCall():
					assistant.ToolCalls = append(assistant.ToolCalls, ToolCall{ID: part.CallID, Name: part.Tool, Arguments: part.Arguments})
					content, err := callResult(&items[j], perObservation)
					if err != nil {
						return nil, err
					}
					calls = append(calls, Message{Role: RoleTool, ToolCallID: part.CallID, Content: content})
				default:
					return nil, fmt.Errorf("context: item %s (%s) cannot be part of a model response", part.ID, part.Type)
				}
			}
			// A response cut off before its text or calls were recorded
			// (a crash) said nothing the model can be shown.
			if assistant.Content != "" || len(assistant.ToolCalls) > 0 {
				out = append(out, assistant)
				out = append(out, calls...)
			}
			i = j
		default:
			return nil, fmt.Errorf("context: item %s (%s) has no place in the transcript", it.ID, it.Type)
		}
	}
	// Callers with no current-turn marker still receive an explicit reference
	// before the transcript, never as the most recent user request.
	if pendingMemory != nil {
		out = append([]Message{*pendingMemory}, out...)
	}
	return out, nil
}

// compactedHistory renders what replaces the items before a compaction.
func (m *Manager) compactedHistory(before []thread.Item, summary string) string {
	var b strings.Builder
	if users := recentUserMessages(before, m.allocation().userMessages); len(users) > 0 {
		b.WriteString("Earlier messages from the user:\n")
		for _, text := range users {
			b.WriteString("- " + strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n  ") + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(summaryPrefix + "\n\n" + strings.TrimSpace(summary))
	for _, artifact := range activeArtifacts(before) {
		fmt.Fprintf(&b, "\n\n--- active %s: %s ---\n%s", artifact.Kind, artifact.Name, artifact.Content)
	}
	return b.String()
}

// recentUserMessages returns the most recent user messages that fit budget,
// oldest first.
func recentUserMessages(items []thread.Item, budget int) []string {
	var out []string
	used := 0
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type != thread.ItemUserMessage {
			continue
		}
		cost := estimateTokens(items[i].Text) + 4
		if used+cost > budget {
			break
		}
		used += cost
		out = append([]string{items[i].Text}, out...)
	}
	return out
}

// activeArtifacts returns the instructions loaded by successful tool calls
// (activated skills, subdirectory AGENTS.md files), the latest version of
// each.
func activeArtifacts(items []thread.Item) []tools.ContextArtifact {
	var out []tools.ContextArtifact
	index := map[string]int{}
	for _, it := range items {
		if it.Type != thread.ItemMcpToolCall || it.Status != thread.ItemCompleted || it.Result == nil {
			continue
		}
		for _, artifact := range it.Result.Artifacts {
			key := artifact.Kind + "/" + artifact.Name
			if i, ok := index[key]; ok {
				out[i] = artifact
				continue
			}
			index[key] = len(out)
			out = append(out, artifact)
		}
	}
	return out
}

// callResult renders the tool message answering one call item. Registry
// tool output is truncated to outputBudget tokens; the item keeps it in full.
func callResult(it *thread.Item, outputBudget int) (string, error) {
	switch {
	case it.Type == thread.ItemMcpToolCall && it.Result != nil:
		return renderObservation(it, outputBudget)
	case it.Type == thread.ItemCollabAgentToolCall && it.Status == thread.ItemCompleted:
		return fmt.Sprintf("status: ok\nagent: %s\nreport:\n%s", it.Agent, clipText(it.Text, outputBudget)), nil
	case it.Status == thread.ItemFailed || it.Status == thread.ItemDeclined:
		return "status: error\nerror: " + it.Error, nil
	case it.Type == thread.ItemTodoList:
		return "Plan updated", nil
	case it.Type == thread.ItemUserInputRequest && it.Status == thread.ItemCompleted:
		return "status: ok\n" + prettyJSON(map[string]any{"answers": it.Answers}), nil
	default:
		return "status: error\nerror: the call did not finish", nil
	}
}

const maxObservationTokens = 6000

// nextUserMessage returns the index of the first user message after the
// first message, or -1. A transcript must start with a user message.
func nextUserMessage(messages []Message) int {
	for i := 1; i < len(messages); i++ {
		if messages[i].Role == RoleUser {
			return i
		}
	}
	return -1
}

// renderObservation renders a registry tool result for the model.
func renderObservation(it *thread.Item, outputBudget int) (string, error) {
	result, err := it.LiveResult()
	if err != nil {
		return "", fmt.Errorf("context: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tool: %s\n", it.Tool)
	if result.Err != nil {
		fmt.Fprintf(&b, "status: error\nerror: %s\n", clipText(result.Err.Error(), outputBudget))
	} else {
		fmt.Fprintf(&b, "status: ok\nresult: kind=%s count=%d empty=%v\n", result.Kind, result.Count, result.Empty)
		if text := strings.TrimSpace(compactText(result)); text != "" {
			fmt.Fprintf(&b, "output:\n%s\n", clipText(text, outputBudget))
		}
		for _, artifact := range result.Artifacts {
			fmt.Fprintf(&b, "\n--- %s: %s ---\n%s\n", artifact.Kind, artifact.Name, artifact.Content)
		}
	}
	return strings.TrimSpace(b.String()), nil
}

func renderSkills(skills []map[string]any) string {
	if len(skills) == 0 {
		return ""
	}
	return prettyJSON(skills)
}

func estimateTokens(s string) int {
	ascii, nonASCII := 0, 0
	for _, r := range s {
		if r <= unicode.MaxASCII {
			ascii++
		} else {
			nonASCII++
		}
	}
	return (ascii+3)/4 + nonASCII
}

func clipText(s string, budget int) string {
	s = strings.TrimSpace(s)
	if s == "" || budget <= 0 || estimateTokens(s) <= budget {
		return s
	}
	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if estimateTokens(string(runes[:mid])+"\n[content truncated]") <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.TrimSpace(string(runes[:lo])) + "\n[content truncated]"
}

// prettyJSON renders prompt data. Everything rendered here came from JSON
// or is plain Go data, so a marshal failure is a bug.
func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("contextmanager: render %T: %v", v, err))
	}
	return string(b)
}

func noneIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}
