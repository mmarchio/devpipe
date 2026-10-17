package logging

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"overseer/internal/config"
)

// T18.111 — records logged with an operation context carry its
// correlation id and fields; later fields replace earlier ones.
func TestT18111ContextFields(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewRedacting(config.LoggingConfig{Level: "info", Format: "json"}, &buf, func() []string { return []string{"s3cret"} })
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithCorrelationID(context.Background(), "req-1")
	ctx = WithFields(ctx, "execution_id", "e1", "step", 1)
	ctx = WithFields(ctx, "step", 2, "dispatch_id", "s3cret")
	l.InfoContext(ctx, "hello")
	out := buf.String()
	for _, want := range []string{`"correlation_id":"req-1"`, `"execution_id":"e1"`, `"step":2`, `"dispatch_id":"[REDACTED]"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
	if strings.Contains(out, `"step":1`) {
		t.Fatalf("stale field: %s", out)
	}
	if c, id := EnsureCorrelationID(context.Background()); id == "" || CorrelationID(c) != id {
		t.Fatal("no generated correlation id")
	}
	if _, id := EnsureCorrelationID(ctx); id != "req-1" {
		t.Fatal("existing correlation id replaced")
	}
}

// T18.185 (ring) — ERROR records are kept redacted and bounded; lower
// levels are not.
func TestT18185RecentErrorsRing(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewRedacting(config.LoggingConfig{Level: "info", Format: "text"}, &buf, func() []string { return []string{"s3cret"} })
	if err != nil {
		t.Fatal(err)
	}
	l.WithComponent("x").Info("fine")
	for i := 0; i < RingSize+5; i++ {
		l.WithComponent("x").Error("boom s3cret", "i", i)
	}
	got := l.RecentErrors()
	if len(got) != RingSize || strings.Contains(strings.Join(got, ""), "s3cret") || strings.Contains(strings.Join(got, ""), "fine") {
		t.Fatalf("ring: %d entries %q", len(got), got[:2])
	}
	if !strings.Contains(got[len(got)-1], `"i":104`) || !strings.Contains(got[0], `"component":"x"`) {
		t.Fatalf("order / attrs: %s … %s", got[0], got[len(got)-1])
	}
}
