package service

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// P13-34 (WBS 13.65): structured result logs carry correlation fields and
// never the result text.
func TestP1365ResultLogging(t *testing.T) {
	var buf bytes.Buffer
	s := NewResultService(ResultDeps{Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
	ph, wu := uuid.New(), uuid.New()
	d := &worker.WorkerDispatch{ID: uuid.New(), ProjectID: uuid.New(), SessionID: uuid.New(), PhaseID: &ph, WorkUnitID: &wu, StateKey: "IMPLEMENT", RoleKey: "ENGINEER"}
	now := time.Now()
	res := workerresult.WorkerResult{ProtocolVersion: workerresult.ProtocolV1_0, Status: workerresult.StatusComplete, Summary: "SECRET-SUMMARY-TEXT"}
	out := &ParseOutcome{Attempt: persistence.ParseAttemptRecord{Outcome: workerresult.OutcomeAccepted, ResultStatus: workerresult.StatusComplete,
		ParserVersion: workerresult.ParserVersion, StartedAt: now, CompletedAt: now.Add(3 * time.Millisecond)},
		Result: &persistence.StoredWorkerResult{ID: uuid.New(), Result: res}, Created: true}
	s.recordOutcome(context.Background(), &dispatchIDs{}, d, out)
	line := buf.String()
	for _, k := range []string{"project_id", "phase_id", "work_unit_id", "dispatch_id", "session_id", `"state"`, `"role"`, "result_status",
		"protocol_version", "parser_version", "parse_duration_ms", "repaired"} {
		if !strings.Contains(line, k) {
			t.Fatalf("missing %s in %s", k, line)
		}
	}
	if strings.Contains(line, "SECRET-SUMMARY-TEXT") {
		t.Fatal("result text logged")
	}
}
