package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/0xfakeSpike/ollupi/pkg/jsonrpc"
)

type Method func(params json.RawMessage) (any, error)

var builtinMethods = map[string]bool{
	methodInitialize: true, methodInitialized: true,
	methodThreadStart: true, methodThreadResume: true, methodThreadList: true, methodThreadRead: true,
	methodTurnStart: true, methodTurnSteer: true, methodTurnInterrupt: true,
	methodModelList:     true,
	methodWorkspaceList: true, methodWorkspaceAdd: true, methodWorkspaceRename: true, methodWorkspaceArchive: true,
	methodToolCall:   true,
	methodMemoryList: true, methodMemoryStatus: true, methodMemoryUpdate: true, methodMemoryForget: true,
}

func (s *Server) Register(name string, m Method) error {
	switch {
	case m == nil:
		return fmt.Errorf("gateway: method %q has no handler", name)
	case strings.TrimSpace(name) != name || name == "" || strings.HasPrefix(name, "rpc."):
		return fmt.Errorf("gateway: invalid method name %q", name)
	case builtinMethods[name]:
		return fmt.Errorf("gateway: method %q is part of the protocol", name)
	}
	if _, dup := s.methods[name]; dup {
		return fmt.Errorf("gateway: method %q is registered twice", name)
	}
	s.methods[name] = m
	return nil
}

func callMethod(m Method, params json.RawMessage) (any, func(), *jsonrpc.Error) {
	result, err := m(params)
	if err != nil {
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) && rpcErr != nil {
			return nil, nil, rpcErr
		}
		return nil, nil, serverError(err)
	}
	if result == nil {
		result = struct{}{}
	}
	return result, nil, nil
}
