package tools

import "context"

type ProgressEmitter func(text string)

type progressKey struct{}

func WithProgressEmitter(ctx context.Context, emit ProgressEmitter) context.Context {
	return context.WithValue(ctx, progressKey{}, emit)
}

func ProgressEmitterFrom(ctx context.Context) (ProgressEmitter, bool) {
	emit, ok := ctx.Value(progressKey{}).(ProgressEmitter)
	return emit, ok
}
