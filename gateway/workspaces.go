package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0xfakeSpike/ollupi/pkg/jsonrpc"
	"github.com/0xfakeSpike/ollupi/pkg/sandbox"
	"github.com/0xfakeSpike/ollupi/repo"
	"github.com/google/uuid"
)

func (c *conn) workspaceRequest(method string, params json.RawMessage) (any, func(), *jsonrpc.Error) {
	manager := c.s.agent
	if method == methodWorkspaceList {
		if err := jsonrpc.DecodeParams(params, &struct{}{}); err != nil {
			return nil, nil, err
		}
		rows, err := manager.DB.Workspaces()
		if err != nil {
			return nil, nil, serverError(err)
		}
		return struct {
			Data []repo.WorkspaceRow `json:"data"`
		}{rows}, nil, nil
	}
	var result repo.WorkspaceRow
	var err error
	switch method {
	case methodWorkspaceAdd:
		var p struct {
			Path string `json:"path"`
			Name string `json:"name,omitempty"`
		}
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		result, err = addWorkspace(manager.DB, p.Path, p.Name)
	case methodWorkspaceRename:
		var p struct {
			WorkspaceID string `json:"workspaceId"`
			Name        string `json:"name"`
		}
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		result, err = renameWorkspace(manager.DB, p.WorkspaceID, p.Name)
	case methodWorkspaceArchive:
		var p struct {
			WorkspaceID string `json:"workspaceId"`
			Archived    *bool  `json:"archived"`
		}
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		if p.Archived == nil {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "archived is required")
		}
		result, err = archiveWorkspace(manager.DB, p.WorkspaceID, *p.Archived)
	}
	if err != nil {
		return nil, nil, serverError(err)
	}
	payload := struct {
		Workspace repo.WorkspaceRow `json:"workspace"`
	}{result}
	return payload, func() { c.s.broadcast("workspace/changed", payload) }, nil
}

func addWorkspace(db *repo.DB, path, name string) (repo.WorkspaceRow, error) {
	root, err := sandbox.CanonicalDirectory(path)
	if err != nil {
		return repo.WorkspaceRow{}, err
	}
	if name == "" {
		name = filepath.Base(root)
	}
	if name, err = workspaceName(name); err != nil {
		return repo.WorkspaceRow{}, err
	}
	now := time.Now().UTC()
	w := repo.WorkspaceRow{ID: uuid.NewString(), Name: name, RootPath: root, CreatedAt: now, UpdatedAt: now}
	return db.AddWorkspace(w,
		func(existing repo.WorkspaceRow) bool { return sameDirectory(existing.RootPath, root) },
		func(existing repo.WorkspaceRow) (repo.WorkspaceRow, bool) {
			if existing.ArchivedAt == nil {
				return existing, false
			}
			existing.ArchivedAt, existing.UpdatedAt = nil, now
			return existing, true
		})
}

func renameWorkspace(db *repo.DB, id, name string) (repo.WorkspaceRow, error) {
	name, err := workspaceName(name)
	if err != nil {
		return repo.WorkspaceRow{}, err
	}
	return changeWorkspace(db, id, func(w *repo.WorkspaceRow) { w.Name = name })
}

func archiveWorkspace(db *repo.DB, id string, archived bool) (repo.WorkspaceRow, error) {
	return changeWorkspace(db, id, func(w *repo.WorkspaceRow) {
		w.ArchivedAt = nil
		if archived {
			at := w.UpdatedAt
			w.ArchivedAt = &at
		}
	})
}

func changeWorkspace(db *repo.DB, id string, change func(*repo.WorkspaceRow)) (repo.WorkspaceRow, error) {
	w, err := db.UpdateWorkspace(id, func(w repo.WorkspaceRow) (repo.WorkspaceRow, error) {
		w.UpdatedAt = time.Now().UTC()
		change(&w)
		return w, nil
	})
	if repo.IsNotFound(err) {
		return repo.WorkspaceRow{}, fmt.Errorf("gateway: workspace %s not found", id)
	}
	return w, err
}

func workspaceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 120 {
		return "", fmt.Errorf("gateway: a workspace name must be 1 to 120 characters")
	}
	return name, nil
}

func sameDirectory(a, b string) bool {
	if a == b {
		return true
	}
	canonical, err := sandbox.CanonicalDirectory(a)
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
