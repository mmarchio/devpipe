package logging

import (
	"context"
	"log/slog"
	"strings"
	"sync"
)

// Recent errors (WBS 18.185): every ERROR record is also kept, already
// redacted and encoded, in a bounded in-memory ring for diagnostic
// bundles.

// RingSize and RingEntryBytes bound the recent-error ring.
const (
	RingSize       = 100
	RingEntryBytes = 4096
)

// Ring is a bounded buffer of recent encoded records.
type Ring struct {
	mu      sync.Mutex
	entries []string
	next    int
	full    bool
}

func newRing() *Ring { return &Ring{entries: make([]string, RingSize)} }

// Write stores one encoded record (slog handlers write one per call).
func (r *Ring) Write(p []byte) (int, error) {
	s := strings.TrimRight(string(p), "\n")
	if len(s) > RingEntryBytes {
		s = s[:RingEntryBytes] + "…"
	}
	r.mu.Lock()
	r.entries[r.next] = s
	r.next = (r.next + 1) % len(r.entries)
	r.full = r.full || r.next == 0
	r.mu.Unlock()
	return len(p), nil
}

// Recent returns the stored records, oldest first.
func (r *Ring) Recent() []string {
	if r == nil {
		return []string{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []string{}
	if r.full {
		out = append(out, r.entries[r.next:]...)
	}
	out = append(out, r.entries[:r.next]...)
	return out
}

// teeHandler sends every record to the primary handler and ERROR records
// to the ring handler too.
type teeHandler struct {
	primary, ring slog.Handler
}

func (h teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.primary.Enabled(ctx, l) || l >= slog.LevelError
}

func (h teeHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		_ = h.ring.Handle(ctx, r.Clone())
	}
	if h.primary.Enabled(ctx, r.Level) {
		return h.primary.Handle(ctx, r)
	}
	return nil
}

func (h teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{h.primary.WithAttrs(attrs), h.ring.WithAttrs(attrs)}
}

func (h teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{h.primary.WithGroup(name), h.ring.WithGroup(name)}
}
