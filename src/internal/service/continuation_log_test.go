package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/continuation"
	"overseer/internal/persistence"
)

type captureEvents struct {
	got []*persistence.WorkerEventRecord
}

func (c *captureEvents) Append(_ context.Context, e *persistence.WorkerEventRecord) error {
	c.got = append(c.got, e)
	return nil
}
func (c *captureEvents) List(context.Context, persistence.WorkerEventFilter) ([]persistence.WorkerEventRecord, error) {
	return nil, nil
}
func (c *captureEvents) AppendTool(context.Context, *persistence.WorkerToolEventRecord) error {
	return nil
}
func (c *captureEvents) ListTools(context.Context, uuid.UUID, uuid.UUID) ([]persistence.WorkerToolEventRecord, error) {
	return nil, nil
}

// P14-43 — WBS 14.87/14.89: structured continuation logs and audit events
// carry identifiers, strategy, reason and context usage — never prompts,
// worker status text or secrets.
func TestP1489ContinuationLogging(t *testing.T) {
	var buf bytes.Buffer
	ev := &captureEvents{}
	s := NewContinuationService(ContinuationDeps{Logger: slog.New(slog.NewJSONHandler(&buf, nil)), Events: ev})
	u := 0.5
	old, nw, d := uuid.New(), uuid.New(), uuid.New()
	c := &continuation.Continuation{ID: uuid.New(), ProjectID: uuid.New(), PhaseID: uuid.New(), WorkUnitID: uuid.New(), Attempt: 2,
		Status: continuation.StatusDispatching, Strategy: continuation.ContinueFreshSession, OldSessionID: &old, NewSessionID: &nw, DispatchID: &d,
		Decision: &continuation.Decision{Strategy: continuation.ContinueFreshSession, Reason: continuation.ReasonAtThreshold, Utilization: &u, Threshold: 0.8},
		Captured: continuation.CapturedStatus{Summary: "SECRET-SUMMARY sk-live-123", Remaining: []string{"SECRET-REMAINING"}, NextAction: "SECRET-NEXT"}}
	s.log(c, "continuation transition")
	s.event(context.Background(), c, continuation.EventDecided, nil)
	out := buf.String()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"project_id", "phase_id", "work_unit_id", "continuation_id", "attempt", "strategy", "decision_reason",
		"context_usage", "threshold", "old_session_id", "new_session_id", "dispatch_id"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("14.89 log field %s missing: %s", k, out)
		}
	}
	payload, _ := json.Marshal(ev.got[0].Payload)
	for _, leak := range []string{"SECRET", "sk-live"} {
		if strings.Contains(out, leak) || strings.Contains(string(payload), leak) {
			t.Fatalf("worker status text leaked: %s / %s", out, payload)
		}
	}
	if len(ev.got) != 1 || ev.got[0].Type != continuation.EventDecided {
		t.Fatal("event")
	}
}
