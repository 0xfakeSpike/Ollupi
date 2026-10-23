package repo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"gorm.io/gorm/clause"
)

type ItemRow struct {
	ID                string             `gorm:"primaryKey" json:"id"`
	ThreadID          string             `gorm:"not null;uniqueIndex:items_thread_seq,priority:1" json:"-"`
	Thread            *ThreadRow         `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	TurnID            string             `gorm:"not null;index" json:"turnId"`
	Turn              *TurnRow           `gorm:"constraint:OnDelete:CASCADE" json:"-"`
	Seq               int                `gorm:"not null;uniqueIndex:items_thread_seq,priority:2" json:"seq"`
	Type              ItemType           `gorm:"not null" json:"type"`
	Status            ItemStatus         `gorm:"not null" json:"status"`
	Text              string             `gorm:"not null;default:''" json:"text,omitempty"`
	CronJobID         string             `gorm:"not null;default:''" json:"cronJobId,omitempty"`
	ResponseID        string             `gorm:"not null;default:''" json:"responseId,omitempty"`
	ContextStats      *ContextStats      `gorm:"serializer:json" json:"contextStats,omitempty"`
	Usage             *Usage             `gorm:"serializer:json" json:"usage,omitempty"`
	Memory            []MemoryReference  `gorm:"serializer:json" json:"memory,omitempty"`
	CallID            string             `gorm:"not null;default:''" json:"callId,omitempty"`
	Tool              string             `gorm:"not null;default:''" json:"tool,omitempty"`
	Arguments         string             `gorm:"not null;default:''" json:"arguments,omitempty"`
	Server            string             `gorm:"not null;default:''" json:"server,omitempty"`
	Annotations       *tools.Annotations `gorm:"serializer:json" json:"annotations,omitempty"`
	Approval          *Approval          `gorm:"serializer:json" json:"approval,omitempty"`
	PreparedExecution *tools.Prepared    `gorm:"serializer:json" json:"preparedExecution,omitempty"`
	Await             *tools.Await       `gorm:"serializer:json" json:"await,omitempty"`
	Result            *ToolResult        `gorm:"serializer:json" json:"result,omitempty"`
	Error             string             `gorm:"not null;default:''" json:"error,omitempty"`
	StartedAt         time.Time          `gorm:"not null" json:"startedAt"`
	CompletedAt       *time.Time         `json:"completedAt,omitempty"`
}

func (ItemRow) TableName() string { return "items" }

type ItemType string

const (
	ItemUserMessage       ItemType = "userMessage"
	ItemAgentMessage      ItemType = "agentMessage"
	ItemReasoning         ItemType = "reasoning"
	ItemToolCall          ItemType = "toolCall"
	ItemContextCompaction ItemType = "contextCompaction"
	ItemMemoryContext     ItemType = "memoryContext"
	ItemToolProgress      ItemType = "toolProgress"
	ItemError             ItemType = "error"
)

type ItemStatus string

const (
	ItemInProgress ItemStatus = "inProgress"
	ItemCompleted  ItemStatus = "completed"
	ItemFailed     ItemStatus = "failed"
	ItemDeclined   ItemStatus = "declined"
)

// ContextStats describes one prompt. EstimatedTokens is the character-based
// estimate of its parts; CalibratedTokens is what the budget is checked
// against: the provider's count of an earlier prompt to the same model plus
// the estimated growth since (the plain estimate when there is no such count).
type ContextStats struct {
	Model            string `json:"model,omitempty"`
	BudgetTokens     int    `json:"budgetTokens"`
	SystemTokens     int    `json:"systemTokens"`
	MessagesTokens   int    `json:"messagesTokens"`
	MemoryTokens     int    `json:"memoryTokens,omitempty"`
	ToolsTokens      int    `json:"toolsTokens"`
	EstimatedTokens  int    `json:"estimatedTokens"`
	CalibratedTokens int    `json:"calibratedTokens,omitempty"`
}

type MemoryReference struct {
	ID          string `json:"id"`
	Revision    int    `json:"revision"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	Topic       string `json:"topic,omitempty"`
	Text        string `json:"text"`
}

type Approval struct {
	ID          string     `json:"id"`
	RequestedAt time.Time  `json:"requestedAt"`
	Reason      string     `json:"reason,omitempty"`
	Rule        string     `json:"rule,omitempty"`
	Content     string     `json:"content,omitempty"`
	Decision    Decision   `json:"decision,omitempty"`
	DecidedBy   string     `json:"decidedBy,omitempty"`
	DecidedAt   *time.Time `json:"decidedAt,omitempty"`
	Error       string     `json:"error,omitempty"`
}

type Decision string

const (
	DecisionAccept           Decision = "accept"
	DecisionAcceptForSession Decision = "acceptForSession"
	DecisionDecline          Decision = "decline"
)

func (a Approval) AvailableDecisions() []Decision {
	if a.Rule != "" {
		return []Decision{DecisionAccept, DecisionDecline}
	}
	return []Decision{DecisionAccept, DecisionAcceptForSession, DecisionDecline}
}

type ToolResult struct {
	Output    json.RawMessage         `json:"output,omitempty"`
	Artifacts []tools.ContextArtifact `json:"artifacts,omitempty"`
}

func NewUserMessage(id, turnID, text string, at time.Time) ItemRow {
	return ItemRow{ID: id, TurnID: turnID, Type: ItemUserMessage, Status: ItemCompleted, Text: text, StartedAt: at, CompletedAt: &at}
}

func (it ItemRow) IsCall() bool {
	return it.Type == ItemToolCall
}

func (it ItemRow) Awaits() bool {
	return it.Type == ItemToolCall && it.Status == ItemInProgress && it.Await != nil
}

func (it ItemRow) WaitsOnAgent() bool {
	return it.Awaits() && it.Await.By == tools.AwaitAgent
}

func (it ItemRow) IsPending() bool {
	return it.WaitsForApproval() || (it.Awaits() && it.Await.By == tools.AwaitUser)
}

func (it ItemRow) WaitsForApproval() bool {
	return it.Type == ItemToolCall && it.Status == ItemInProgress && it.Approval != nil && it.Approval.Decision == ""
}

func (it ItemRow) LiveResult() (tools.Result, error) {
	if it.Result == nil {
		return tools.Result{}, fmt.Errorf("item %s has no tool result", it.ID)
	}
	o := it.Result
	r := tools.Result{Tool: it.Tool, Artifacts: o.Artifacts}
	if len(o.Output) > 0 {
		dec := json.NewDecoder(bytes.NewReader(o.Output))
		dec.UseNumber()
		if err := dec.Decode(&r.Output); err != nil {
			return tools.Result{}, fmt.Errorf("decode tool output of item %s: %w", it.ID, err)
		}
	}
	if it.Error != "" {
		r.Err = errors.New(it.Error)
	}
	return r, nil
}

func ResultOf(result tools.Result) (*ToolResult, string, error) {
	out := &ToolResult{Artifacts: result.Artifacts}
	if result.Output != nil {
		raw, err := json.Marshal(result.Output)
		if err != nil {
			return nil, "", fmt.Errorf("encode tool output: %w", err)
		}
		out.Output = raw
	}
	if result.Err == nil {
		return out, "", nil
	}
	return out, result.Err.Error(), nil
}

func (d *DB) Item(id string) (ItemRow, error) {
	var row ItemRow
	return row, notFound(d.gorm.Where("id = ?", id).Take(&row).Error)
}

func (d *DB) NextItemSeq(threadID string) (int, error) {
	var n int64
	err := d.gorm.Model(&ItemRow{}).Where("thread_id = ?", threadID).Count(&n).Error
	return int(n) + 1, err
}

func (d *DB) SaveItem(row ItemRow) error {
	row.StartedAt, row.CompletedAt = row.StartedAt.UTC(), nullTime(row.CompletedAt)
	res := d.gorm.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"status", "text", "cron_job_id", "response_id", "context_stats", "usage", "memory",
			"call_id", "tool", "arguments", "server", "annotations", "approval", "prepared_execution", "await", "result", "error", "completed_at"}),
		Where: where(`items.thread_id = excluded.thread_id AND items.turn_id = excluded.turn_id
			AND items.seq = excluded.seq AND items.type = excluded.type`),
	}).Create(&row)
	if err := checkOne(res); err != nil {
		return fmt.Errorf("item %s: %w", row.ID, err)
	}
	return nil
}

func (d *DB) EndedItem(id string) (ItemRow, error) {
	var row ItemRow
	err := d.gorm.Table("items AS i").Select("i.*").Joins("JOIN turns t ON t.id = i.turn_id").
		Where("i.id = ? AND t.status <> 'inProgress' AND i.type NOT IN ('reasoning', 'contextCompaction', 'memoryContext')", id).Take(&row).Error
	return row, notFound(err)
}

func (d *DB) UserMessageIDs(turnID string, limit int) ([]string, error) {
	var ids []string
	err := d.gorm.Model(&ItemRow{}).Where("turn_id = ? AND type = 'userMessage'", turnID).Order("seq").Limit(limit).Pluck("id", &ids).Error
	return ids, err
}
