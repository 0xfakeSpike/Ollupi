package repo

import (
	"fmt"
	"time"
)

type ThreadRow struct {
	ID             string    `gorm:"primaryKey" json:"id"`
	WorkspaceID    string    `gorm:"not null;default:'';index:threads_workspace_updated,priority:1" json:"workspaceId,omitempty"`
	Preview        string    `gorm:"not null" json:"preview"`
	ApprovalPolicy string    `gorm:"not null;default:''" json:"approvalPolicy,omitempty"`
	ParentThreadID string    `gorm:"not null;default:'';index" json:"parentThreadId,omitempty"`
	ParentItemID   string    `gorm:"not null;default:''" json:"parentItemId,omitempty"`
	CreatedAt      time.Time `gorm:"not null;autoCreateTime:false" json:"createdAt"`
	UpdatedAt      time.Time `gorm:"not null;autoUpdateTime:false;index;index:threads_workspace_updated,priority:2,sort:desc" json:"updatedAt"`
}

func (ThreadRow) TableName() string { return "threads" }

func (d *DB) Thread(id string) (ThreadView, error) {
	var out ThreadView
	err := d.Tx(func(tx *DB) error {
		if err := notFound(tx.gorm.Where("id = ?", id).Take(&out.Thread).Error); err != nil {
			return err
		}
		if err := tx.gorm.Where("thread_id = ?", id).Order("seq").Find(&out.Turns).Error; err != nil {
			return err
		}
		return tx.gorm.Where("thread_id = ?", id).Order("seq").Find(&out.Items).Error
	})
	if err != nil {
		return ThreadView{}, err
	}
	for i, it := range out.Items {
		if it.Seq != i+1 {
			return ThreadView{}, fmt.Errorf("storage: thread %s items have a gap at seq %d", id, i+1)
		}
	}
	return out, nil
}

func (d *DB) ThreadRow(id string) (ThreadRow, error) {
	var row ThreadRow
	return row, notFound(d.gorm.Where("id = ?", id).Take(&row).Error)
}

type ThreadFilter struct {
	WorkspaceID *string `json:"workspaceId,omitempty"`
	Limit       int     `json:"limit,omitempty"`
}

func (d *DB) ThreadRows(f ThreadFilter) ([]ThreadRow, error) {
	q := d.gorm.Order("updated_at DESC, created_at DESC")
	if f.WorkspaceID != nil {
		if *f.WorkspaceID == "" {
			q = q.Where("workspace_id = ''")
		} else {
			q = q.Where("workspace_id = ?", *f.WorkspaceID)
		}
	}
	q = q.Where("parent_thread_id = ''")
	if f.Limit > 0 {
		q = q.Limit(f.Limit)
	}
	var rows []ThreadRow
	return rows, q.Find(&rows).Error
}

func (d *DB) CreateThread(row ThreadRow) error {
	row.CreatedAt, row.UpdatedAt = row.CreatedAt.UTC(), row.UpdatedAt.UTC()
	return d.gorm.Create(&row).Error
}

func (d *DB) UpdateThread(row ThreadRow) error {
	res := d.gorm.Model(&ThreadRow{}).Where("id = ?", row.ID).Updates(map[string]any{
		"approval_policy": row.ApprovalPolicy, "preview": row.Preview, "updated_at": row.UpdatedAt.UTC(),
	})
	if err := checkOne(res); err != nil {
		return fmt.Errorf("thread %s: %w", row.ID, err)
	}
	return nil
}

type ThreadStatus string

const (
	ThreadIdle               ThreadStatus = "idle"
	ThreadActive             ThreadStatus = "active"
	ThreadWaitingOnApproval  ThreadStatus = "waitingOnApproval"
	ThreadWaitingOnUserInput ThreadStatus = "waitingOnUserInput"
)

type ThreadSummary struct {
	ThreadRow
	Status ThreadStatus `json:"status"`
}

type ThreadView struct {
	Thread ThreadRow `json:"thread"`
	Turns  []TurnRow `json:"turns"`
	Items  []ItemRow `json:"items"`
}

func (v ThreadView) Status() ThreadStatus {
	if _, active := v.ActiveTurn(); !active {
		return ThreadIdle
	}
	it, waiting := v.PendingRequest()
	switch {
	case !waiting:
		return ThreadActive
	case !it.WaitsForApproval():
		return ThreadWaitingOnUserInput
	default:
		return ThreadWaitingOnApproval
	}
}

func (v ThreadView) ActiveTurn() (TurnRow, bool) {
	if n := len(v.Turns); n > 0 && v.Turns[n-1].Status == TurnInProgress {
		return v.Turns[n-1], true
	}
	return TurnRow{}, false
}

func (v ThreadView) AgentCalls() []ItemRow {
	turn, ok := v.ActiveTurn()
	if !ok {
		return nil
	}
	var out []ItemRow
	for _, it := range v.Items {
		if it.TurnID == turn.ID && it.WaitsOnAgent() {
			out = append(out, it)
		}
	}
	return out
}

func (v ThreadView) FinalAnswer(turnID string) (string, bool) {
	for i := len(v.Items) - 1; i >= 0; i-- {
		if it := v.Items[i]; it.TurnID == turnID && it.Type == ItemAgentMessage && it.Status == ItemCompleted {
			return it.Text, true
		}
	}
	return "", false
}

func (v ThreadView) PendingRequest() (ItemRow, bool) {
	if p := v.PendingRequests(); len(p) > 0 {
		return p[0], true
	}
	return ItemRow{}, false
}

func (v ThreadView) PendingRequests() []ItemRow {
	turn, ok := v.ActiveTurn()
	if !ok {
		return nil
	}
	var out []ItemRow
	for _, it := range v.Items {
		if it.TurnID == turn.ID && it.IsPending() {
			out = append(out, it)
		}
	}
	return out
}
