package gateway

import (
	"encoding/json"
	"strings"

	"github.com/0xfakeSpike/ollupi/pkg/jsonrpc"
	"github.com/0xfakeSpike/ollupi/repo"
)

func (c *conn) memoryRequest(method string, params json.RawMessage) (any, func(), *jsonrpc.Error) {
	store := c.s.agent.Memory
	if store == nil {
		return nil, nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "long-term memory is unavailable")
	}
	var result any
	var err error
	switch method {
	case methodMemoryList:
		var p struct {
			WorkspaceID   string `json:"workspaceId,omitempty"`
			IncludeGlobal bool   `json:"includeGlobal,omitempty"`
		}
		if rpcErr := jsonrpc.DecodeParams(params, &p); rpcErr != nil {
			return nil, nil, rpcErr
		}
		var entries []repo.MemoryRow
		entries, err = store.List(p.WorkspaceID, p.IncludeGlobal)
		result = struct {
			Data []repo.MemoryRow `json:"data"`
		}{entries}
	case methodMemoryStatus:
		if rpcErr := jsonrpc.DecodeParams(params, &struct{}{}); rpcErr != nil {
			return nil, nil, rpcErr
		}
		result, err = store.Status()
	case methodMemoryUpdate, methodMemoryForget:
		var p struct {
			ID   string `json:"id"`
			Text string `json:"text,omitempty"`
		}
		if rpcErr := jsonrpc.DecodeParams(params, &p); rpcErr != nil {
			return nil, nil, rpcErr
		}
		if strings.TrimSpace(p.ID) == "" {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "id is required")
		}
		if method == methodMemoryUpdate {
			var entry repo.MemoryRow
			entry, err = store.Edit(p.ID, p.Text)
			result = struct {
				Memory repo.MemoryRow `json:"memory"`
			}{entry}
		} else {
			err = store.Forget(p.ID)
			result = struct{}{}
		}
	}
	if err != nil {
		return nil, nil, serverError(err)
	}
	return result, nil, nil
}
