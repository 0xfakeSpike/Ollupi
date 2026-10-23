package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/0xfakeSpike/ollupi/agent/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type RemoteSession struct {
	name     string
	endpoint string
	mu       sync.RWMutex
	sess     *mcp.ClientSession
	Tools    map[string]*mcp.Tool
}

type Client struct {
	Name    string
	Version string
}

func NewRemoteSession(ctx context.Context, client Client, endpoint string) (*RemoteSession, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("mcp: endpoint is required")
	}
	s := &RemoteSession{
		Tools:    make(map[string]*mcp.Tool),
		endpoint: endpoint,
	}
	if client.Name == "" {
		client.Name = "ollupi"
	}
	if client.Version == "" {
		client.Version = "0.1"
	}
	mc := mcp.NewClient(&mcp.Implementation{Name: client.Name, Version: client.Version}, nil)
	sess, err := mc.Connect(
		ctx,
		&mcp.StreamableClientTransport{Endpoint: endpoint},
		nil,
	)
	if err != nil {
		return nil, err
	}
	s.sess = sess
	s.name = sess.InitializeResult().ServerInfo.Name
	return s, nil
}

func (s *RemoteSession) Name() (string, string) {
	return s.name, fmt.Sprintf("Remote MCP tools (endpoint %s).", s.endpoint)
}

func (s *RemoteSession) ToolList(ctx context.Context) ([]*tools.Spec, error) {
	loaded := make(map[string]*mcp.Tool)
	var cursor string
	for {
		res, err := s.sess.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, t := range res.Tools {
			if t == nil || t.Name == "" {
				continue
			}
			loaded[t.Name] = t
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	s.mu.Lock()
	s.Tools = loaded
	s.mu.Unlock()
	specs := make([]*tools.Spec, 0, len(loaded))
	for _, tool := range loaded {
		specs = append(specs, toSpec(tool))
	}
	return specs, nil
}

func toSpec(t *mcp.Tool) *tools.Spec {
	spec := &tools.Spec{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	if t.Annotations != nil {
		spec.Annotations = &tools.Annotations{
			Title:           t.Annotations.Title,
			ReadOnlyHint:    t.Annotations.ReadOnlyHint,
			DestructiveHint: t.Annotations.DestructiveHint,
			IdempotentHint:  t.Annotations.IdempotentHint,
			OpenWorldHint:   t.Annotations.OpenWorldHint,
		}
	}
	return spec
}

func (s *RemoteSession) Invoke(ctx context.Context, localTool string, arguments map[string]any) (tools.Result, error) {
	s.mu.RLock()
	_, ok := s.Tools[localTool]
	s.mu.RUnlock()
	if !ok {
		return tools.Result{Err: fmt.Errorf("mcp: tool %q not exposed by MCP server", localTool)}, fmt.Errorf("mcp: tool %q not exposed by MCP server", localTool)
	}

	res, err := s.sess.CallTool(ctx, &mcp.CallToolParams{Name: localTool, Arguments: arguments})
	if err != nil {
		return tools.Result{Err: err}, err
	}
	if res.IsError {
		msg := joinTextContent(res.Content)
		if msg == "" {
			msg = "tool error"
		}
		return tools.Result{Output: msg, Err: errors.New(msg)}, nil
	}
	if res.StructuredContent != nil {
		return tools.Result{Output: res.StructuredContent}, nil
	}
	return tools.Result{Output: joinTextContent(res.Content)}, nil
}

func (s *RemoteSession) Close() error {
	return s.sess.Close()
}

func joinTextContent(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		} else {
			parts = append(parts, fmt.Sprint(c))
		}
	}
	return strings.Join(parts, "\n")
}
