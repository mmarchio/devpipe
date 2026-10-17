package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/worker"
)

// P12-66 (WBS 12.52): dispatch logs carry correlation fields only — no
// prompt / response content, no credentials.
func TestP1266DispatchLogging(t *testing.T) {
	var buf bytes.Buffer
	s := NewWorkerService(WorkerDeps{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	ph, wu := uuid.New(), uuid.New()
	start := time.Now()
	end := start.Add(1500 * time.Millisecond)
	d := worker.WorkerDispatch{ID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(), PhaseID: &ph, WorkUnitID: &wu, RuntimeStateID: uuid.New(),
		RequestID: uuid.New(), StateKey: "IMPLEMENT", RoleKey: "ENGINEER", Attempt: 1, Status: worker.DispatchCompleted, StartedAt: &start, CompletedAt: &end,
		PromptHash: "h"}
	o := &persistence.WorkerUsageObservation{Usage: worker.WorkerUsage{InputTokens: worker.Int64(10), OutputTokens: worker.Int64(5), ContextTokens: worker.Int64(15)},
		Source: worker.SourceProviderActual}
	s.logDispatch(context.Background(), d, o, nil)
	we := worker.NewError(worker.CodeAuthFailed, worker.DeliveryReceived, "rejected", nil)
	d.Status = worker.DispatchFailed
	s.logDispatch(context.Background(), d, nil, we)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines %d", len(lines))
	}
	var m map[string]any
	json.Unmarshal([]byte(lines[0]), &m)
	for _, k := range []string{"project_id", "phase_id", "work_unit_id", "runtime_state_id", "dispatch_id", "session_id", "state", "role", "duration_ms",
		"input_tokens", "context_tokens", "result"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("T12.52.1 missing %s in %s", k, lines[0])
		}
	}
	for _, k := range []string{"content", "prompt", "response", "token", "authorization"} {
		if _, ok := m[k]; ok {
			t.Fatalf("T12.52.2/T12.52.3 %s logged", k)
		}
	}
	if !strings.Contains(lines[1], `"error_code":"AUTHENTICATION_FAILED"`) {
		t.Fatal("failure result not logged")
	}
}

// WBS 12.53.4: audit payload keys that could carry content or secrets are
// dropped.
func TestP1265SanitizePayload(t *testing.T) {
	out := sanitizePayload(map[string]any{"content_hash": "x", "prompt": "p", "auth_token": "t", "code": "c", "Authorization": "b"})
	if len(out) != 2 || out["code"] != "c" || out["content_hash"] != "x" {
		t.Fatalf("%v", out)
	}
}
