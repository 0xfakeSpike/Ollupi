package gateway

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/0xfakeSpike/ollupi/agent/memory"
	"github.com/0xfakeSpike/ollupi/pkg/jsonrpc"
	"github.com/0xfakeSpike/ollupi/repo"
)

const errMemoryConflict = -32009

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
		entries, err = c.s.agent.DB.Memories(p.WorkspaceID, p.IncludeGlobal)
		if entries == nil {
			entries = []repo.MemoryRow{}
		}
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
			ID       string `json:"id"`
			Revision int    `json:"revision"`
			Text     string `json:"text,omitempty"`
		}
		if rpcErr := jsonrpc.DecodeParams(params, &p); rpcErr != nil {
			return nil, nil, rpcErr
		}
		if strings.TrimSpace(p.ID) == "" || p.Revision < 1 {
			return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "id and positive revision are required")
		}
		if method == methodMemoryUpdate {
			var entry repo.MemoryRow
			entry, err = store.Edit(p.ID, p.Revision, p.Text)
			result = struct {
				Memory repo.MemoryRow `json:"memory"`
			}{entry}
		} else {
			err = store.Forget(p.ID, p.Revision)
			result = struct{}{}
		}
	}
	if errors.Is(err, memory.ErrConflict) {
		return nil, nil, jsonrpc.Errorf(errMemoryConflict, "memory changed since it was loaded; refresh and try again")
	}
	if err != nil {
		return nil, nil, serverError(err)
	}
	return result, nil, nil
}
