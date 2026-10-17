package threadmanager

import (
	"fmt"
	execcontext "github.com/0xfakeSpike/ollupi/execution"
	"github.com/0xfakeSpike/ollupi/thread"
	"github.com/0xfakeSpike/ollupi/workspace"
)

// Options must be installed before startup recovery. ResolveExecution captures
// the current settings for new turns; ValidateExecution checks saved settings
// before any resumed run. Existing turns never resolve a replacement snapshot.
type Options struct {
	Workspaces        *workspace.Manager
	ResolveExecution  func(thread.Thread) (*execcontext.Context, error)
	ValidateExecution func(*execcontext.Context) error
}

func (m *Manager) ListWorkspaces() []workspace.Workspace {
	if m.options.Workspaces == nil {
		return []workspace.Workspace{}
	}
	return m.options.Workspaces.List()
}
func (m *Manager) AddWorkspace(path, name string) (workspace.Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.options.Workspaces == nil {
		return workspace.Workspace{}, fmt.Errorf("project workspaces are unavailable")
	}
	return m.options.Workspaces.Add(path, name)
}
func (m *Manager) RenameWorkspace(id, name string) (workspace.Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.options.Workspaces == nil {
		return workspace.Workspace{}, fmt.Errorf("project workspaces are unavailable")
	}
	return m.options.Workspaces.Rename(id, name)
}
func (m *Manager) ArchiveWorkspace(id string, archived bool) (workspace.Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.options.Workspaces == nil {
		return workspace.Workspace{}, fmt.Errorf("project workspaces are unavailable")
	}
	return m.options.Workspaces.Archive(id, archived)
}
func (m *Manager) requireActiveWorkspace(id string) error {
	if id == "" {
		return nil
	}
	if m.options.Workspaces == nil {
		return fmt.Errorf("project workspaces are unavailable")
	}
	return m.options.Workspaces.RequireActive(id)
}
func (m *Manager) newExecutionContext(view thread.View) (*execcontext.Context, error) {
	// A child belongs to an already active parent turn, including when the
	// project was archived after the parent started. Do not reread its rules.
	if view.Thread.ParentThreadID != "" {
		parent, ok := m.threads.Read(view.Thread.ParentThreadID)
		if !ok {
			return nil, fmt.Errorf("parent conversation is unavailable")
		}
		turn, ok := parent.ActiveTurn()
		if !ok {
			return nil, fmt.Errorf("parent turn is no longer active")
		}
		if m.options.ResolveExecution != nil && turn.ExecutionContext == nil {
			return nil, fmt.Errorf("parent turn has no execution context")
		}
		return execcontext.Clone(turn.ExecutionContext), nil
	}
	if err := m.requireActiveWorkspace(view.Thread.WorkspaceID); err != nil {
		return nil, err
	}
	if m.options.ResolveExecution == nil {
		return nil, nil
	}
	snapshot, err := m.options.ResolveExecution(view.Thread)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, fmt.Errorf("execution context resolver returned no snapshot")
	}
	if err := m.validateExecutionContext(snapshot); err != nil {
		return nil, err
	}
	return execcontext.Clone(snapshot), nil
}
func (m *Manager) validateExecutionContext(snapshot *execcontext.Context) error {
	if m.options.ResolveExecution == nil && m.options.ValidateExecution == nil {
		return nil
	}
	if snapshot == nil {
		return fmt.Errorf("this unfinished turn predates execution snapshots; automatic recovery stopped because its original working directory cannot be verified; start a new turn")
	}
	if m.options.ValidateExecution != nil {
		return m.options.ValidateExecution(snapshot)
	}
	return execcontext.Validate(snapshot)
}
