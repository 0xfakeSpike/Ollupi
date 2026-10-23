package runtime

import (
	"errors"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

type streamedText struct {
	rt         *Runtime
	responseID string
	items      map[repo.ItemType]*repo.ItemRow
	stats      *repo.ContextStats
	usage      *repo.Usage
	carrierID  string
	text       map[repo.ItemType]*strings.Builder
	err        error
}

func newStreamedText(rt *Runtime, stats repo.ContextStats) *streamedText {
	return &streamedText{rt: rt, responseID: uuid.NewString(), items: map[repo.ItemType]*repo.ItemRow{}, text: map[repo.ItemType]*strings.Builder{}, stats: &stats}
}

func (st *streamedText) record(it repo.ItemRow) (repo.ItemRow, error) {
	if st.carrierID == "" || st.carrierID == it.ID {
		it.ContextStats, it.Usage = st.stats, st.usage
	}
	stored, err := st.rt.record(it)
	if err == nil && st.carrierID == "" {
		st.carrierID = stored.ID
	}
	return stored, err
}

func (st *streamedText) delta(d llm.Delta) {
	st.add(repo.ItemReasoning, d.Reasoning)
	st.add(repo.ItemAgentMessage, d.Content)
}

func (st *streamedText) add(kind repo.ItemType, text string) {
	if text == "" || st.err != nil {
		return
	}
	it, ok := st.items[kind]
	if !ok {
		started, err := st.record(repo.ItemRow{
			ID: uuid.NewString(), TurnID: st.rt.Turn.ID, Type: kind, Status: repo.ItemInProgress,
			ResponseID: st.responseID, StartedAt: time.Now().UTC(),
		})
		if err != nil {
			st.err = err
			return
		}
		it = &started
		st.items[kind] = it
		st.text[kind] = &strings.Builder{}
	}
	st.text[kind].WriteString(text)
	st.rt.delta(*it, text)
}

func (st *streamedText) complete(resp llm.ChatResponse) error {
	if st.err != nil {
		return st.err
	}
	for _, part := range []struct {
		kind repo.ItemType
		text string
	}{{repo.ItemReasoning, resp.Message.ReasoningContent}, {repo.ItemAgentMessage, resp.Message.Content}} {
		it, started := st.items[part.kind]
		if part.text == "" && !started {
			continue
		}
		now := time.Now().UTC()
		if !started {
			it = &repo.ItemRow{ID: uuid.NewString(), TurnID: st.rt.Turn.ID, Type: part.kind, ResponseID: st.responseID, StartedAt: now}
		}
		it.Text, it.Status, it.CompletedAt = part.text, repo.ItemCompleted, &now
		if _, err := st.record(*it); err != nil {
			return err
		}
	}
	return nil
}

func (st *streamedText) fail(cause error) error {
	if len(st.items) == 0 {
		now := time.Now().UTC()
		if _, err := st.record(repo.ItemRow{
			ID: uuid.NewString(), TurnID: st.rt.Turn.ID, Type: repo.ItemError, Status: repo.ItemFailed,
			ResponseID: st.responseID, Text: cause.Error(), Error: cause.Error(), StartedAt: now, CompletedAt: &now,
		}); err != nil {
			return errors.Join(cause, err)
		}
		return cause
	}
	for kind, it := range st.items {
		now := time.Now().UTC()
		it.Text, it.Status, it.Error, it.CompletedAt = st.text[kind].String(), repo.ItemFailed, cause.Error(), &now
		if _, err := st.record(*it); err != nil {
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
