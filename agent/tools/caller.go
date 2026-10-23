package tools

import "context"

type Caller struct {
	ThreadID    string
	WorkspaceID string
	Root        string
	Subagent    bool
}

type callerKey struct{}

func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

func CallerOf(ctx context.Context) Caller {
	c, _ := ctx.Value(callerKey{}).(Caller)
	return c
}
