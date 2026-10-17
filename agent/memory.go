package agent

import (
	"context"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/google/uuid"
)

// loadMemory freezes even an empty selection before the first model request.
// Approvals, subagents, and process recovery resume from the recorded revision
// and text without a second retrieval or dependence on a running memory worker.
func (rn *Runner) loadMemory(ctx context.Context, r *run) error {
	for _, item := range r.turnItems() {
		if item.Type == thread.ItemMemoryContext && item.Status == thread.ItemCompleted {
			r.memory = append([]thread.MemoryReference(nil), item.Memory...)
			return nil
		}
	}
	if rn.memory == nil {
		return nil
	}
	var requests []string
	for _, item := range r.turnItems() {
		if item.Type == thread.ItemUserMessage {
			requests = append(requests, item.Text)
		}
	}
	workspaceID := execution.WorkspaceID(ctx)
	entries, err := rn.memory.Recall(ctx, workspaceID, strings.Join(requests, "\n\n"), rn.contexts.MemoryBudget())
	var references []thread.MemoryReference
	if err != nil {
		// Do not log database text or potentially sensitive provider details.
		// An unavailable memory index must not prevent normal conversation.
		r.tracef("memory retrieval unavailable; continuing with an empty selection")
	} else {
		seen := make(map[string]bool)
		for _, entry := range entries {
			if entry.ID == "" || entry.Status != "active" || (entry.WorkspaceID != "" && entry.WorkspaceID != workspaceID) || seen[entry.ID] {
				continue
			}
			seen[entry.ID] = true
			references = append(references, thread.MemoryReference{ID: entry.ID, Revision: entry.Revision, WorkspaceID: entry.WorkspaceID, Topic: entry.Topic, Text: entry.Text})
		}
	}
	now := time.Now().UTC()
	item, err := r.record(thread.Item{
		ID: uuid.NewString(), TurnID: r.turnID, Type: thread.ItemMemoryContext,
		Status: thread.ItemCompleted, Memory: rn.contexts.BoundMemory(references),
		StartedAt: now, CompletedAt: &now,
	})
	if err != nil {
		return err
	}
	r.memory = append([]thread.MemoryReference(nil), item.Memory...)
	r.tracef("memory selection saved: %d entries", len(r.memory))
	return nil
}
