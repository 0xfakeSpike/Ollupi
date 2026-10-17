package tools

import "context"

// ProgressEmitter streams human-readable updates into the conversation feed
// while a long-running tool works.
type ProgressEmitter func(text string)

type progressKey struct{}

// WithProgressEmitter returns ctx with emit attached, for tools that report
// progress while they run.
func WithProgressEmitter(ctx context.Context, emit ProgressEmitter) context.Context {
	return context.WithValue(ctx, progressKey{}, emit)
}

// ProgressEmitterFrom returns the emitter attached to ctx, if any.
func ProgressEmitterFrom(ctx context.Context) (ProgressEmitter, bool) {
	emit, ok := ctx.Value(progressKey{}).(ProgressEmitter)
	return emit, ok
}
