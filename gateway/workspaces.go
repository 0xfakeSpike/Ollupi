package gateway

import (
	"encoding/json"
	"github.com/0xfakeSpike/ollupi/jsonrpc"
	"github.com/0xfakeSpike/ollupi/workspace"
)

type workspaceManager interface {
	ListWorkspaces() []workspace.Workspace
	AddWorkspace(path, name string) (workspace.Workspace, error)
	RenameWorkspace(id, name string) (workspace.Workspace, error)
	ArchiveWorkspace(id string, archived bool) (workspace.Workspace, error)
}

func (c *conn) workspaceRequest(method string, params json.RawMessage) (any, func(), *jsonrpc.Error) {
	manager, ok := c.s.threads.(workspaceManager)
	if !ok {
		return nil, nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "project workspaces are unavailable")
	}
	if method == methodWorkspaceList {
		if err := jsonrpc.DecodeParams(params, &struct{}{}); err != nil {
			return nil, nil, err
		}
		return struct {
			Data []workspace.Workspace `json:"data"`
		}{manager.ListWorkspaces()}, nil, nil
	}
	var result workspace.Workspace
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
		result, err = manager.AddWorkspace(p.Path, p.Name)
	case methodWorkspaceRename:
		var p struct {
			WorkspaceID string `json:"workspaceId"`
			Name        string `json:"name"`
		}
		if err := jsonrpc.DecodeParams(params, &p); err != nil {
			return nil, nil, err
		}
		result, err = manager.RenameWorkspace(p.WorkspaceID, p.Name)
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
		result, err = manager.ArchiveWorkspace(p.WorkspaceID, *p.Archived)
	}
	if err != nil {
		return nil, nil, serverError(err)
	}
	payload := struct {
		Workspace workspace.Workspace `json:"workspace"`
	}{result}
	return payload, func() { c.s.notifyWorkspace(payload) }, nil
}

func (s *Server) notifyWorkspace(payload any) {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		ready := c.initialized
		c.mu.Unlock()
		if ready {
			c.rpc.Notify("workspace/changed", payload)
		}
	}
}
