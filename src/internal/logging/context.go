package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
)

// Correlation (WBS 18.111 / 18.112): one operation — an HTTP request, or an
// engine step started by the scheduler — carries a correlation id in its
// context, together with the identifiers of what it acts on (execution,
// project, step, dispatch). Every record logged with that context
// (InfoContext, WarnContext, …) gets them as attributes, so the logs of one
// operation can be reconstructed by correlation_id and the history of an
// execution by execution_id.

type fieldsKey struct{}

type fields struct {
	attrs       []slog.Attr
	correlation string
}

func current(ctx context.Context) fields {
	if ctx == nil {
		return fields{}
	}
	f, _ := ctx.Value(fieldsKey{}).(fields)
	return f
}

// WithCorrelationID sets the operation's correlation id (empty = keep).
func WithCorrelationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	f := current(ctx)
	f.correlation = id
	return context.WithValue(ctx, fieldsKey{}, f)
}

// CorrelationID returns the context's correlation id ("" when none).
func CorrelationID(ctx context.Context) string { return current(ctx).correlation }

// EnsureCorrelationID returns ctx with a correlation id, generating one
// when the context carries none, and the id.
func EnsureCorrelationID(ctx context.Context) (context.Context, string) {
	if id := CorrelationID(ctx); id != "" {
		return ctx, id
	}
	id := NewCorrelationID()
	return WithCorrelationID(ctx, id), id
}

// NewCorrelationID returns a fresh random correlation id.
func NewCorrelationID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "op-" + hex.EncodeToString(b[:])
}

// WithFields adds key / value attributes logged with every record of ctx;
// a key set again replaces the earlier value.
func WithFields(ctx context.Context, args ...any) context.Context {
	f := current(ctx)
	add := slog.Group("", args...).Value.Group()
	attrs := make([]slog.Attr, 0, len(f.attrs)+len(add))
	for _, a := range f.attrs {
		replaced := false
		for _, n := range add {
			replaced = replaced || n.Key == a.Key
		}
		if !replaced {
			attrs = append(attrs, a)
		}
	}
	f.attrs = append(attrs, add...)
	return context.WithValue(ctx, fieldsKey{}, f)
}

// contextHandler adds the context's correlation id and fields to every
// record.
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	f := current(ctx)
	if f.correlation == "" && len(f.attrs) == 0 {
		return h.Handler.Handle(ctx, r)
	}
	// A key the record sets itself wins over the context's.
	have := map[string]bool{}
	r.Attrs(func(a slog.Attr) bool { have[a.Key] = true; return true })
	if f.correlation != "" && !have["correlation_id"] {
		r.AddAttrs(slog.String("correlation_id", f.correlation))
	}
	for _, a := range f.attrs {
		if !have[a.Key] {
			r.AddAttrs(a)
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
