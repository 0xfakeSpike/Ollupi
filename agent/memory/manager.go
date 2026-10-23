package memory

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/0xfakeSpike/ollupi/pkg/llm"
	"github.com/0xfakeSpike/ollupi/repo"
)

type Options struct {
	Model    llm.Model
	Generate bool
	Use      bool
	Interval time.Duration
	Timeout  time.Duration
	// ExtractionTokens caps one extraction request, to keep its cost down;
	// the model's own input limit (from pkg/llm) caps it further.
	ExtractionTokens int
	BatchSize        int
}

const defaultExtractionTokens = 12000

type Manager struct {
	db     *repo.DB
	model  *llm.OpenAI
	opts   Options
	budget int
	runMu  sync.Mutex
}

func New(db *repo.DB, model *llm.OpenAI, opts Options) (*Manager, error) {
	if db == nil {
		return nil, errors.New("memory: database is required")
	}
	if opts.Generate && (model == nil || strings.TrimSpace(opts.Model.Name) == "") {
		return nil, errors.New("memory: extraction model is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Minute
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 2 * time.Minute
	}
	if opts.ExtractionTokens <= 0 {
		opts.ExtractionTokens = defaultExtractionTokens
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 24
	}
	m := &Manager{db: db, model: model, opts: opts, budget: opts.ExtractionTokens}
	if opts.Generate {
		limits, err := model.Limits(opts.Model.Name)
		if err != nil {
			return nil, fmt.Errorf("memory: extraction model: %w", err)
		}
		m.budget = min(m.budget, limits.Input())
		if m.budget < 2048 {
			return nil, fmt.Errorf("memory: the extraction budget is %d tokens; it must be at least 2048", m.budget)
		}
	}
	return m, nil
}

func (m *Manager) Run(ctx context.Context) {
	if !m.opts.Generate {
		return
	}
	timer := time.NewTicker(m.opts.Interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := m.RunOnce(ctx); err != nil && ctx.Err() == nil {
				log.Printf("memory: %v", err)
			}
		}
	}
}

func (m *Manager) RunOnce(ctx context.Context) (err error) {
	if !m.opts.Generate {
		return nil
	}
	m.runMu.Lock()
	defer m.runMu.Unlock()
	var batch *Batch
	defer func() {
		if err != nil && ctx.Err() == nil && !errors.Is(err, ErrConflict) {
			if batch != nil {
				_ = deferBatch(m.db, batch, err.Error())
			}
			_ = m.db.SetMemoryError(clipMemory(err.Error(), 1000))
		}
	}()
	var evidence *Batch
	var payload string
	var estimated int
	batch, evidence, payload, estimated, err = m.gatherEvidence()
	if err != nil {
		return err
	}
	if batch == nil {
		return nil
	}

	report := repo.MemoryReport{Model: m.opts.Model.Clone(), EstimatedInputTokens: estimated}
	if len(evidence.Facts) == 0 {
		if err := commit(m.db, batch, nil, report); err != nil {
			return commitError(err)
		}
		return nil
	}
	ops, err := m.extract(ctx, evidence, payload, &report)
	if err != nil {
		return err
	}
	if err := commit(m.db, batch, ops, report); err != nil {
		return commitError(err)
	}
	return nil
}

// gatherEvidence reads the next pending batch and, if needed, shrinks it until
// its evidence fits the extraction budget. A nil batch means there is nothing
// to do. On error the batch is still returned so the caller can defer it.
func (m *Manager) gatherEvidence() (batch, evidence *Batch, payload string, estimated int, err error) {
	limit := m.opts.BatchSize
	for {
		batch, err = nextBatch(m.db, limit)
		if err != nil {
			return nil, nil, "", 0, errors.New("could not read pending memory evidence")
		}
		if batch == nil {
			return nil, nil, "", 0, nil
		}
		evidence = visibleEvidence(batch)
		payload, estimated, err = extractionInput(evidence, m.budget)
		if err == nil {
			return batch, evidence, payload, estimated, nil
		}
		if limit <= 1 {
			return batch, evidence, "", 0, err
		}
		limit = max(1, limit/2)
	}
}

func (m *Manager) extract(ctx context.Context, evidence *Batch, payload string, report *repo.MemoryReport) ([]Operation, error) {
	callCtx, cancel := context.WithTimeout(ctx, m.opts.Timeout)
	defer cancel()
	response, err := m.model.Chat(callCtx, llm.ChatRequest{
		Model:      m.opts.Model,
		Messages:   []llm.ChatMessage{{Role: llm.RoleSystem, Content: extractionInstructions}, {Role: llm.RoleUser, Content: payload}},
		ToolChoice: llm.ToolChoiceNone,
	}, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("memory extraction model request failed; check the configured model connection")
	}
	if len(response.Message.ToolCalls) != 0 || (response.FinishReason != "" && response.FinishReason != "stop") {
		return nil, errors.New("memory extraction did not produce a complete text response")
	}
	ops, err := parseOperations(response.Message.Content, evidence)
	if err != nil {
		return nil, err
	}
	report.InputTokens, report.OutputTokens = response.Usage.PromptTokens, response.Usage.CompletionTokens
	report.UsageReported = response.UsageReported
	return ops, nil
}

func commitError(err error) error {
	if errors.Is(err, ErrConflict) {
		return ErrConflict
	}
	return errors.New("could not save memory changes; pending evidence will be retried")
}

func (m *Manager) Status() (repo.MemoryStatus, error) {
	s, err := m.db.MemoryStatus()
	s.GenerationEnabled, s.UseEnabled = m.opts.Generate, m.opts.Use
	return s, err
}

func (m *Manager) Edit(id string, revision int, text string) (repo.MemoryRow, error) {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > 4000 {
		return repo.MemoryRow{}, errors.New("memory text must contain 1 to 4000 characters")
	}
	if redact(text) != text {
		return repo.MemoryRow{}, errors.New("remove credentials from memory text before saving")
	}
	var out repo.MemoryRow
	err := m.db.Tx(func(tx *repo.DB) error {
		e, err := tx.Memory(id)
		if err != nil {
			return err
		}
		if e.Revision != revision || e.Status == "forgotten" {
			return ErrConflict
		}
		e.Text, e.UserEdited, e.Status = text, true, "active"
		e.Revision++
		e.UpdatedAt = time.Now().UTC()
		out = e
		return tx.SaveMemory(e, "user_edit")
	})
	return out, err
}

func (m *Manager) Forget(id string, revision int) error {
	return m.db.Tx(func(tx *repo.DB) error {
		e, err := tx.Memory(id)
		if err != nil {
			return err
		}
		if e.Revision != revision {
			return ErrConflict
		}
		if e.Status == "forgotten" {
			return nil
		}
		e.Status, e.Text = "forgotten", ""
		e.Revision++
		e.UpdatedAt = time.Now().UTC()
		if err := tx.DeleteMemoryRevisions(id); err != nil {
			return err
		}
		return tx.SaveMemory(e, "user_forget")
	})
}
