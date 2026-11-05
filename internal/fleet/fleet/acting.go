package fleet

import (
	"context"

	"github.com/lineahq/linea/pluginsdk"
)

type actingKey struct{}

// WithActing attaches the user a delegating management credential acts for.
// It is attribution only; fleet never authorizes on it.
func WithActing(ctx context.Context, a *pluginsdk.Acting) context.Context {
	if a == nil {
		return ctx
	}
	return context.WithValue(ctx, actingKey{}, *a)
}

// ActingFrom returns the acting-for value of a request, or nil.
func ActingFrom(ctx context.Context) *pluginsdk.Acting {
	if a, ok := ctx.Value(actingKey{}).(pluginsdk.Acting); ok {
		return &a
	}
	return nil
}
