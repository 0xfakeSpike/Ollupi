package contextmanager

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
)

// Manager builds the prompts of one model. Every budget it hands out is a
// share of that model's input limit, which pkg/llm reports.
type Manager struct {
	model       string
	inputTokens int
}

func New(model string, limits llm.Limits) *Manager {
	return &Manager{model: model, inputTokens: limits.Input()}
}

const (
	autoCompactPercent = 80
	fixedPercent       = 50
	artifactPercent    = 15
)

func share(inputTokens, pct int) int { return max(256, inputTokens*pct/100) }

// Fits reports whether the parts sent with every request, the system prompt
// and the tool definitions, leave room for a conversation.
func (m *Manager) Fits(system string, tools any) error {
	s := m.stats(system, nil, tools)
	if limit := m.inputTokens * fixedPercent / 100; s.EstimatedTokens > limit {
		return fmt.Errorf("context: the system prompt and tool definitions take %d tokens, over half of the %d input tokens model %s allows", s.EstimatedTokens, m.inputTokens, m.model)
	}
	return nil
}

type Input struct {
	Items         []repo.ItemRow
	CurrentTurnID string
	System        string
	Tools         any
}

type Prompt struct {
	System          string
	Messages        []llm.ChatMessage
	NeedsCompaction bool
	Stats           repo.ContextStats
}

func (m *Manager) Build(in Input) (Prompt, error) {
	system := in.System
	memory := turnMemory(in.Items, in.CurrentTurnID)
	messages, err := m.transcript(in.Items, in.CurrentTurnID, memory)
	if err != nil {
		return Prompt{}, err
	}
	p := Prompt{System: system, Messages: messages}
	p.Stats = m.stats(system, messages, in.Tools)
	if memory != "" {
		p.Stats.MemoryTokens = llm.EstimateTokens(memory)
	}
	m.calibrate(&p.Stats, in.Items)
	compactable := hasItemsAfterCompaction(in.Items)
	switch {
	case p.Stats.CalibratedTokens > m.inputTokens*autoCompactPercent/100 && compactable:
		p.NeedsCompaction = true
	case p.Stats.CalibratedTokens > m.inputTokens:
		return Prompt{}, fmt.Errorf("context: the prompt needs %d tokens and nothing is left to compact; model %s allows %d", p.Stats.CalibratedTokens, m.model, m.inputTokens)
	}
	return p, nil
}

func (m *Manager) stats(system string, messages []llm.ChatMessage, tools any) repo.ContextStats {
	s := repo.ContextStats{
		Model:          m.model,
		BudgetTokens:   m.inputTokens,
		SystemTokens:   llm.EstimateTokens(system),
		MessagesTokens: llm.EstimateTokens(prettyJSON(messages)),
		ToolsTokens:    llm.EstimateTokens(prettyJSON(tools)),
	}
	s.EstimatedTokens = s.SystemTokens + s.MessagesTokens + s.ToolsTokens
	s.CalibratedTokens = s.EstimatedTokens
	return s
}

// calibrate corrects the estimate with the provider's count. The latest
// response to this model since the latest compaction carries both the estimate
// of the prompt it was sent and the prompt tokens the provider counted; the
// prompt now is that count plus what the estimate grew by since. Before a
// compaction the prompt had another shape, so nothing there is used.
func (m *Manager) calibrate(s *repo.ContextStats, items []repo.ItemRow) {
	for i, from := len(items)-1, latestCompaction(items); i > from; i-- {
		it := items[i]
		ref, usage := it.ContextStats, it.Usage
		if it.ResponseID == "" || it.Type == repo.ItemContextCompaction || ref == nil || usage == nil || usage.InputTokens <= 0 || ref.Model != m.model {
			continue
		}
		s.CalibratedTokens = max(0, int(usage.InputTokens)+s.EstimatedTokens-ref.EstimatedTokens)
		return
	}
}

func (m *Manager) SkillBudget() int {
	return share(m.inputTokens, 4)
}

func (m *Manager) MemoryBudget() int {
	return max(0, min(2000, m.inputTokens*5/100))
}

// ArtifactBudget bounds the context artifacts (activated skills) one tool
// result may bring, and all the active ones a compacted history carries.
func (m *Manager) ArtifactBudget() int {
	return share(m.inputTokens, artifactPercent)
}

// CheckArtifacts refuses artifacts that would not fit their budget, so a
// skill too large for this model is an error the model sees instead of a
// prompt that can no longer be compacted.
func (m *Manager) CheckArtifacts(artifacts []tools.ContextArtifact) error {
	if len(artifacts) == 0 {
		return nil
	}
	if need, budget := llm.EstimateTokens(renderArtifacts(artifacts)), m.ArtifactBudget(); need > budget {
		return fmt.Errorf("the tool's context artifacts need about %d tokens; model %s leaves %d for them", need, m.model, budget)
	}
	return nil
}

func turnMemory(items []repo.ItemRow, turnID string) string {
	for _, it := range items {
		if it.TurnID == turnID && it.Type == repo.ItemMemoryContext && it.Status == repo.ItemCompleted {
			return it.Text
		}
	}
	return ""
}

func (m *Manager) transcript(items []repo.ItemRow, turnID string, memory string) ([]llm.ChatMessage, error) {
	var out []llm.ChatMessage
	var pendingMemory *llm.ChatMessage
	if memory != "" {
		pendingMemory = &llm.ChatMessage{Role: llm.RoleUser, Content: memory}
	}
	appendMemory := func() {
		if pendingMemory != nil {
			out = append(out, *pendingMemory)
			pendingMemory = nil
		}
	}
	start := 0
	if c := latestCompaction(items); c >= 0 {
		for _, item := range items[:c+1] {
			if item.TurnID == turnID {
				appendMemory()
				break
			}
		}
		out = append(out, llm.ChatMessage{Role: llm.RoleUser, Content: m.compactedHistory(items[:c], items[c].Text)})
		start = c + 1
	}
	perObservation := min(maxObservationTokens, max(512, m.inputTokens/8))
	for i := start; i < len(items); {
		it := items[i]
		if it.TurnID == turnID {
			appendMemory()
		}
		switch {
		case it.Type == repo.ItemError && it.ContextStats != nil:

			i++
		case it.Type == repo.ItemUserMessage || it.Type == repo.ItemError:
			out = append(out, llm.ChatMessage{Role: llm.RoleUser, Content: it.Text})
			i++
		case it.Type == repo.ItemContextCompaction || it.Type == repo.ItemMemoryContext || it.Type == repo.ItemToolProgress:

			i++
		case it.ResponseID != "":
			assistant := llm.ChatMessage{Role: llm.RoleAssistant}
			var calls []llm.ChatMessage
			j := i
			for ; j < len(items) && items[j].ResponseID == it.ResponseID; j++ {
				part := items[j]
				switch {
				case part.Type == repo.ItemReasoning || part.Type == repo.ItemAgentMessage:

					if part.Status != repo.ItemCompleted {
						continue
					}
					if part.Type == repo.ItemReasoning {
						assistant.ReasoningContent += part.Text
					} else {
						assistant.Content += part.Text
					}
				case part.IsCall():
					assistant.ToolCalls = append(assistant.ToolCalls, llm.ToolCall{ID: part.CallID, Name: part.Tool, Arguments: part.Arguments})
					content, err := callResult(&items[j], perObservation)
					if err != nil {
						return nil, err
					}
					calls = append(calls, llm.ChatMessage{Role: llm.RoleTool, ToolCallID: part.CallID, Content: content})
				default:
					return nil, fmt.Errorf("context: item %s (%s) cannot be part of a model response", part.ID, part.Type)
				}
			}

			if assistant.Content != "" || len(assistant.ToolCalls) > 0 {
				out = append(out, assistant)
				out = append(out, calls...)
			}
			i = j
		default:
			return nil, fmt.Errorf("context: item %s (%s) has no place in the transcript", it.ID, it.Type)
		}
	}

	if pendingMemory != nil {
		out = append([]llm.ChatMessage{*pendingMemory}, out...)
	}
	return out, nil
}

// activeArtifacts returns the latest version of every artifact the items
// brought, ordered by when each was last brought in.
func activeArtifacts(items []repo.ItemRow) []tools.ContextArtifact {
	var out []tools.ContextArtifact
	for _, it := range items {
		if !it.IsCall() || it.Status != repo.ItemCompleted || it.Result == nil {
			continue
		}
		for _, artifact := range it.Result.Artifacts {
			out = slices.DeleteFunc(out, func(a tools.ContextArtifact) bool { return a.Kind == artifact.Kind && a.Name == artifact.Name })
			out = append(out, artifact)
		}
	}
	return out
}

func renderArtifact(a tools.ContextArtifact) string {
	return fmt.Sprintf("--- %s: %s ---\n%s", a.Kind, a.Name, a.Content)
}

func renderArtifacts(artifacts []tools.ContextArtifact) string {
	parts := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		parts = append(parts, renderArtifact(a))
	}
	return strings.Join(parts, "\n\n")
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("contextmanager: render %T: %v", v, err))
	}
	return string(b)
}
