package contextmanager

import (
	"fmt"
	"slices"
	"strings"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
)

const compactionInstructions = `You are performing a CONTEXT CHECKPOINT COMPACTION. Write a handoff summary for another language model that will resume this conversation. Include:
- the user's requests, preferences, and constraints, and the decisions made;
- current progress: what has been done, with the exact facts, ids, paths, and results it produced;
- what remains to be done, as clear next steps.
Be concise and structured. Write in the user's language. Do not call tools.`

func (m *Manager) CompactionPrompt(in Input) (Prompt, error) {
	system := in.System

	messages, err := m.transcript(in.Items, "", "")
	if err != nil {
		return Prompt{}, err
	}
	instructions := llm.ChatMessage{Role: llm.RoleUser, Content: compactionInstructions}
	// After an earlier compaction the first message is its summary, which the
	// new one has to build on: trimming drops whole user turns after it.
	keep := 0
	if latestCompaction(in.Items) >= 0 {
		keep = 1
	}
	for {
		p := Prompt{System: system, Messages: slices.Concat(messages, []llm.ChatMessage{instructions})}
		p.Stats = m.stats(system, p.Messages, in.Tools)
		m.calibrate(&p.Stats, in.Items)
		if p.Stats.CalibratedTokens <= m.inputTokens {
			return p, nil
		}
		next := nextUserMessage(messages, keep)
		if next < 0 {
			return Prompt{}, fmt.Errorf("context: the compaction request needs %d tokens; model %s allows %d", p.Stats.CalibratedTokens, m.model, m.inputTokens)
		}
		messages = slices.Concat(messages[:keep], messages[next:])
	}
}

func latestCompaction(items []repo.ItemRow) int {
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type == repo.ItemContextCompaction && items[i].Status == repo.ItemCompleted {
			return i
		}
	}
	return -1
}

func hasItemsAfterCompaction(items []repo.ItemRow) bool {
	for _, it := range items[latestCompaction(items)+1:] {
		if it.Type != repo.ItemContextCompaction && it.Type != repo.ItemMemoryContext {
			return true
		}
	}
	return false
}

const summaryPrefix = "Another language model started this conversation and produced the summary below. Use it to continue the work without repeating what was already done."

func (m *Manager) compactedHistory(before []repo.ItemRow, summary string) string {
	var b strings.Builder
	if users := recentUserMessages(before, share(m.inputTokens, 10)); len(users) > 0 {
		b.WriteString("Earlier messages from the user:\n")
		for _, text := range users {
			b.WriteString("- " + strings.ReplaceAll(strings.TrimSpace(text), "\n", "\n  ") + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(summaryPrefix + "\n\n" + strings.TrimSpace(summary))
	kept, dropped := recentArtifacts(activeArtifacts(before), m.ArtifactBudget())
	for _, artifact := range kept {
		fmt.Fprintf(&b, "\n\n--- active %s: %s ---\n%s", artifact.Kind, artifact.Name, artifact.Content)
	}
	for _, artifact := range dropped {
		fmt.Fprintf(&b, "\n\n--- %s %s was active earlier; its content no longer fits here, so load it again if it is still needed ---", artifact.Kind, artifact.Name)
	}
	return b.String()
}

// recentArtifacts keeps the most recently brought artifacts that fit the
// budget together, in their original order, and names the rest.
func recentArtifacts(active []tools.ContextArtifact, budget int) (kept, dropped []tools.ContextArtifact) {
	used := 0
	fits := make([]bool, len(active))
	for i := len(active) - 1; i >= 0; i-- {
		if cost := llm.EstimateTokens(renderArtifact(active[i])); used+cost <= budget {
			used += cost
			fits[i] = true
		}
	}
	for i, artifact := range active {
		if fits[i] {
			kept = append(kept, artifact)
		} else {
			dropped = append(dropped, artifact)
		}
	}
	return kept, dropped
}

func recentUserMessages(items []repo.ItemRow, budget int) []string {
	var out []string
	used := 0
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Type != repo.ItemUserMessage {
			continue
		}
		cost := llm.EstimateTokens(items[i].Text) + 4
		if used+cost > budget {
			break
		}
		used += cost
		out = append([]string{items[i].Text}, out...)
	}
	return out
}

// nextUserMessage is the first user message after the one at keep, so that
// messages[keep:next] is the oldest whole turn that can be dropped.
func nextUserMessage(messages []llm.ChatMessage, keep int) int {
	for i := keep + 1; i < len(messages); i++ {
		if messages[i].Role == llm.RoleUser {
			return i
		}
	}
	return -1
}
