// Package memory maintains durable, scoped knowledge independently of agent
// execution. SQL is the source of truth for both evidence and derived memory.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var ErrConflict = errors.New("memory changed while processing; retry")

// Source identifies the exact evidence shown to the extractor. Digest is a hash
// of its normalized snapshot, not a claim that the current row is unchanged.
type Source struct {
	Table      string `json:"table"`
	ID         string `json:"id"`
	Digest     string `json:"digest,omitempty"`
	Operation  string `json:"operation,omitempty"`
	OccurredAt string `json:"occurredAt,omitempty"`
}

type Fact struct {
	Source Source          `json:"source"`
	Data   json.RawMessage `json:"data"`
}

type Change struct {
	ID      int64 `json:"id"`
	Version int64 `json:"version"`
}

// Batch is a coherent database snapshot within one workspace. Empty workspace
// means General. Changes are durable queue receipts, acknowledged atomically
// with the derived memories only if their versions still match.
type Batch struct {
	WorkspaceID string   `json:"workspaceId"`
	Changes     []Change `json:"-"`
	Facts       []Fact   `json:"facts"`
	Existing    []Entry  `json:"existing"`
}

type Entry struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId,omitempty"`
	Topic       string    `json:"topic"`
	Kind        string    `json:"kind"`
	Text        string    `json:"text"`
	Status      string    `json:"status"` // active, retired, forgotten
	Revision    int       `json:"revision"`
	UserEdited  bool      `json:"userEdited,omitempty"`
	Sources     []Source  `json:"sources"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Operation uses an existing ID and revision for updates/retirements. Adds use
// a stable topic to deduplicate repeated extraction. Scope is assigned by the
// manager, never by model output.
type Operation struct {
	Action   string   `json:"action"` // add, update, retire
	ID       string   `json:"id,omitempty"`
	Revision int      `json:"revision,omitempty"`
	Topic    string   `json:"topic,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Text     string   `json:"text,omitempty"`
	Sources  []Source `json:"sources"`
}

type Report struct {
	Model                string `json:"model"`
	ModelProfileID       string `json:"modelProfileId"`
	EstimatedInputTokens int    `json:"estimatedInputTokens"`
	InputTokens          int64  `json:"inputTokens"`
	OutputTokens         int64  `json:"outputTokens"`
	UsageReported        bool   `json:"usageReported"`
}

type Status struct {
	GenerationEnabled *bool      `json:"generationEnabled,omitempty"`
	UseEnabled        *bool      `json:"useEnabled,omitempty"`
	Pending           int        `json:"pending"`
	Failed            int        `json:"failed"`
	Active            int        `json:"active"`
	LastSuccess       *time.Time `json:"lastSuccess,omitempty"`
	LastError         string     `json:"lastError,omitempty"`
	LastReport        *Report    `json:"lastReport,omitempty"`
}

type Store interface {
	MemoryNextBatch(context.Context, int) (*Batch, error)
	MemoryCommit(context.Context, *Batch, []Operation, Report) error
	MemoryFailure(context.Context, string) error
	MemoryList(context.Context, string, bool) ([]Entry, error)
	MemoryStatus(context.Context) (Status, error)
}

// RetryStore isolates an unprocessable batch so one failed scope cannot stop
// extraction for every other workspace. Business row changes reset its backoff.
type RetryStore interface {
	MemoryDefer(context.Context, *Batch, string) error
}

// AdminStore supports explicit user corrections and forgetting without a model
// call. Forgotten entries remain tombstones so replay cannot recreate them.
type AdminStore interface {
	MemoryList(context.Context, string, bool) ([]Entry, error)
	MemoryStatus(context.Context) (Status, error)
	MemoryEdit(context.Context, string, int, string) (Entry, error)
	MemoryForget(context.Context, string, int) error
}

type Reader interface {
	Recall(context.Context, string, string, int) ([]Entry, error)
}
