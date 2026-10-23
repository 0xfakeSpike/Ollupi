package runtime

import (
	"errors"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

// responseRecorder accumulates the streamed parts of one model response and
// writes them as items. Each response has one context/usage carrier: the agent
// message item when there is one, otherwise the first tool call, otherwise the
// reasoning item. The carrier holds the response's ContextStats and Usage so
// the context manager can calibrate its token estimate against the provider's
// real count.
type responseRecorder struct {
	rt    *Runtime
	id    string
	stats repo.ContextStats
	usage *repo.Usage

	reasoning     *repo.ItemRow
	agent         *repo.ItemRow
	reasoningText strings.Builder
	agentText     strings.Builder
	err           error
}

func newResponseRecorder(rt *Runtime, stats repo.ContextStats) *responseRecorder {
	return &responseRecorder{rt: rt, id: uuid.NewString(), stats: stats}
}

func (r *responseRecorder) delta(d llm.Delta) {
	r.add(&r.reasoning, &r.reasoningText, repo.ItemReasoning, d.Reasoning)
	r.add(&r.agent, &r.agentText, repo.ItemAgentMessage, d.Content)
}

func (r *responseRecorder) add(target **repo.ItemRow, text *strings.Builder, kind repo.ItemType, content string) {
	if content == "" || r.err != nil {
		return
	}
	if *target == nil {
		started, err := r.rt.record(repo.ItemRow{
			ID: uuid.NewString(), TurnID: r.rt.Turn.ID, Type: kind, Status: repo.ItemInProgress,
			ResponseID: r.id, StartedAt: time.Now().UTC(),
		})
		if err != nil {
			r.err = err
			return
		}
		*target = &started
	}
	text.WriteString(content)
	r.rt.delta(**target, content)
}

type responsePart struct {
	kind repo.ItemType
	item *repo.ItemRow
	text string
}

// finish completes the text items of a successful response. The agent message
// carries the stats/usage when it exists; otherwise the reasoning item does.
func (r *responseRecorder) finish(resp llm.ChatResponse) error {
	if r.err != nil {
		return r.err
	}
	parts := []responsePart{
		{repo.ItemAgentMessage, r.agent, resp.Message.Content},
		{repo.ItemReasoning, r.reasoning, resp.Message.ReasoningContent},
	}
	for _, part := range parts {
		if part.text == "" && part.item == nil {
			continue
		}
		now := time.Now().UTC()
		it := part.item
		if it == nil {
			it = &repo.ItemRow{ID: uuid.NewString(), TurnID: r.rt.Turn.ID, Type: part.kind, ResponseID: r.id, StartedAt: now}
		}
		it.Text, it.Status, it.CompletedAt = part.text, repo.ItemCompleted, &now
		if part.kind == repo.ItemAgentMessage || (r.agent == nil && part.kind == repo.ItemReasoning) {
			it.ContextStats, it.Usage = &r.stats, r.usage
		}
		if _, err := r.rt.record(*it); err != nil {
			return err
		}
	}
	return nil
}

// fail marks the streamed items as failed, or records one error item when
// nothing was streamed yet.
func (r *responseRecorder) fail(cause error) error {
	if r.reasoning == nil && r.agent == nil {
		now := time.Now().UTC()
		if _, err := r.rt.record(repo.ItemRow{
			ID: uuid.NewString(), TurnID: r.rt.Turn.ID, Type: repo.ItemError, Status: repo.ItemFailed,
			ResponseID: r.id, ContextStats: &r.stats, Usage: r.usage,
			Text: cause.Error(), Error: cause.Error(), StartedAt: now, CompletedAt: &now,
		}); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	for _, part := range []responsePart{
		{repo.ItemReasoning, r.reasoning, r.reasoningText.String()},
		{repo.ItemAgentMessage, r.agent, r.agentText.String()},
	} {
		if part.item == nil {
			continue
		}
		now := time.Now().UTC()
		it := part.item
		it.Text, it.Status, it.Error, it.CompletedAt = part.text, repo.ItemFailed, cause.Error(), &now
		if part.kind == repo.ItemAgentMessage || (r.agent == nil && part.kind == repo.ItemReasoning) {
			it.ContextStats, it.Usage = &r.stats, r.usage
		}
		if _, err := r.rt.record(*it); err != nil {
			return errors.Join(cause, err)
		}
	}
	return cause
}

func (rt *Runtime) delta(it repo.ItemRow, text string) {
	if rt.Delta != nil {
		rt.Delta(it, text)
	}
}
