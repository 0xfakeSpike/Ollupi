// Package workspace manages project directories independently of application data.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/0xfakeSpike/ollupi/execution"
	"github.com/google/uuid"
)

type Workspace struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	RootPath   string     `json:"rootPath"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	ArchivedAt *time.Time `json:"archivedAt,omitempty"`
}

type Persistence interface {
	WorkspacesSelectAll() ([]Workspace, error)
	WorkspaceSave(Workspace) error
}

type Manager struct {
	mu      sync.RWMutex
	store   Persistence
	entries map[string]Workspace
	check   RootCheck
}

// RootCheck rejects a directory that cannot be a project, such as the home
// directory when commands run in a sandbox. Nil accepts every directory.
type RootCheck func(root string) error

// New loads the projects. check runs when a project is added, so an
// unusable directory is refused there rather than when a turn starts.
func New(store Persistence, check RootCheck) (*Manager, error) {
	entries, err := store.WorkspacesSelectAll()
	if err != nil {
		return nil, fmt.Errorf("workspaces: load: %w", err)
	}
	m := &Manager{store: store, entries: make(map[string]Workspace), check: check}
	for _, w := range entries {
		m.entries[w.ID] = w
	}
	return m, nil
}

func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 120 {
		return "", fmt.Errorf("workspace: name must be 1 to 120 characters")
	}
	return name, nil
}

// Add returns the existing workspace for the same real directory and restores it
// if archived. Existing project names are preserved when adding it again.
func (m *Manager) Add(path, name string) (Workspace, error) {
	root, err := execution.CanonicalDirectory(path)
	if err != nil {
		return Workspace{}, err
	}
	if m.check != nil {
		if err := m.check(root); err != nil {
			return Workspace{}, err
		}
	}
	if name == "" {
		name = filepath.Base(root)
	}
	name, err = validName(name)
	if err != nil {
		return Workspace{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.entries {
		if sameDirectory(w.RootPath, root) {
			if w.ArchivedAt != nil {
				w.ArchivedAt = nil
				w.UpdatedAt = time.Now().UTC()
				if err := m.save(w); err != nil {
					return Workspace{}, err
				}
			}
			return clone(w), nil
		}
	}
	now := time.Now().UTC()
	w := Workspace{ID: uuid.NewString(), Name: name, RootPath: root, CreatedAt: now, UpdatedAt: now}
	if err := m.save(w); err != nil {
		return Workspace{}, err
	}
	return clone(w), nil
}

func (m *Manager) List() []Workspace {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Workspace, 0, len(m.entries))
	for _, w := range m.entries {
		out = append(out, clone(w))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}

func (m *Manager) Get(id string) (Workspace, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w, ok := m.entries[id]
	return clone(w), ok
}

func (m *Manager) RequireActive(id string) error {
	if id == "" {
		return nil
	}
	w, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("workspace: project %s not found", id)
	}
	if w.ArchivedAt != nil {
		return fmt.Errorf("workspace: project %s is archived; restore it before starting new work", w.Name)
	}
	root, err := execution.CanonicalDirectory(w.RootPath)
	if err != nil {
		return err
	}
	if root != w.RootPath {
		return fmt.Errorf("workspace: project directory changed; add its new location as a workspace")
	}
	return nil
}

func (m *Manager) Rename(id, name string) (Workspace, error) {
	name, err := validName(name)
	if err != nil {
		return Workspace{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.entries[id]
	if !ok {
		return Workspace{}, fmt.Errorf("workspace: project %s not found", id)
	}
	w.Name, w.UpdatedAt = name, time.Now().UTC()
	if err := m.save(w); err != nil {
		return Workspace{}, err
	}
	return clone(w), nil
}

func (m *Manager) Archive(id string, archived bool) (Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.entries[id]
	if !ok {
		return Workspace{}, fmt.Errorf("workspace: project %s not found", id)
	}
	now := time.Now().UTC()
	w.UpdatedAt = now
	w.ArchivedAt = nil
	if archived {
		w.ArchivedAt = &now
	}
	if err := m.save(w); err != nil {
		return Workspace{}, err
	}
	return clone(w), nil
}

func (m *Manager) save(w Workspace) error {
	if err := m.store.WorkspaceSave(w); err != nil {
		return fmt.Errorf("workspace: save: %w", err)
	}
	m.entries[w.ID] = clone(w)
	return nil
}
func clone(w Workspace) Workspace {
	if w.ArchivedAt != nil {
		at := *w.ArchivedAt
		w.ArchivedAt = &at
	}
	return w
}

// SameFile handles aliases such as case variations on case-insensitive volumes
// without changing path spelling on case-sensitive filesystems.
func sameDirectory(a, b string) bool {
	if a == b {
		return true
	}
	// A registered root that now redirects elsewhere cannot represent the new
	// canonical location: old threads must retain their original identity.
	canonical, err := execution.CanonicalDirectory(a)
	if err != nil || canonical != a {
		return false
	}
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	return err == nil && os.SameFile(left, right)
}
