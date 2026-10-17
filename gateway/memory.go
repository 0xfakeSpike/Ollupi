package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0xfakeSpike/ollupi/jsonrpc"
	"github.com/0xfakeSpike/ollupi/memory"
)

const errMemoryConflict = -32009

func (c *conn) memoryRequest(method string, params json.RawMessage) (any, func(), *jsonrpc.Error) {
	store := c.s.memory
	if store == nil {
		return nil, nil, jsonrpc.Errorf(jsonrpc.CodeServerError, "long-term memory is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
		var entries []memory.Entry
		entries, err = store.MemoryList(ctx, p.WorkspaceID, p.IncludeGlobal)
		if entries == nil {
			entries = []memory.Entry{}
		}
		result = struct {
			Data []memory.Entry `json:"data"`
		}{entries}
	case methodMemoryStatus:
		if rpcErr := jsonrpc.DecodeParams(params, &struct{}{}); rpcErr != nil {
			return nil, nil, rpcErr
		}
		result, err = store.MemoryStatus(ctx)
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
			if strings.TrimSpace(p.Text) == "" || utf8.RuneCountInString(strings.TrimSpace(p.Text)) > 4000 {
				return nil, nil, jsonrpc.Errorf(jsonrpc.CodeInvalidParams, "memory text must contain 1 to 4000 characters")
			}
			var entry memory.Entry
			entry, err = store.MemoryEdit(ctx, p.ID, p.Revision, strings.TrimSpace(p.Text))
			result = struct {
				Memory memory.Entry `json:"memory"`
			}{entry}
		} else {
			err = store.MemoryForget(ctx, p.ID, p.Revision)
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
