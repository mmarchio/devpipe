package readmodel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
)

// P17-18 (WBS 17.11 / 17.12) — only DONE counts; 10/12 → 83.3 %.
func TestP1718Progress(t *testing.T) {
	p := NewProgress(22, 30)
	if p.Percent != 73.3 || p.Label != "22 / 30 tasks complete" {
		t.Fatalf("%+v", p)
	}
	if p := NewProgress(10, 10); p.Percent != 100 {
		t.Fatalf("%+v", p)
	}
	if p := NewProgress(10, 12); p.Percent != 83.3 {
		t.Fatalf("amendment: %+v", p)
	}
	if p := NewProgress(0, 0); p.Percent != 0 || p.Label != "0 / 0 tasks complete" {
		t.Fatalf("empty: %+v", p)
	}
	if p := NewProgress(5, 3); p.Done != 3 {
		t.Fatalf("clamp: %+v", p)
	}
}

// P17-18 (WBS 17.13 / 17.14) — context usage is independent of progress;
// the threshold is normalized to percent.
func TestP1718ContextUsage(t *testing.T) {
	c := NewContextUsage(39200, 64000, 0.8)
	if !c.Known || c.Percent != 61.3 || c.ResetThreshold != 80 {
		t.Fatalf("%+v", c)
	}
	if c := NewContextUsage(10, 0, 80); c.Known || c.ResetThreshold != 80 {
		t.Fatalf("unknown window: %+v", c)
	}
	// Context changes never touch a Progress value (distinct types).
	p := NewProgress(3, 4)
	_ = NewContextUsage(63000, 64000, 0.8)
	if p.Percent != 75 {
		t.Fatal("progress changed")
	}
}

// P17-17 (WBS 17.85 / 17.168) — worker COMPLETE never yields DONE; stages
// follow persisted facts.
func TestP1717UnitStage(t *testing.T) {
	cases := []struct {
		f    UnitFacts
		want UnitStage
	}{
		{UnitFacts{Status: "READY"}, StageReady},
		{UnitFacts{Status: "EXECUTING"}, StageExecuting},
		{UnitFacts{Status: "INCOMPLETE"}, StageIncomplete},
		{UnitFacts{Status: "INCOMPLETE", ContinuationInFlight: true}, StageContinuing},
		{UnitFacts{Status: "VALIDATING", WorkerResult: "COMPLETE"}, StageComplete},
		{UnitFacts{Status: "VALIDATING", WorkerResult: "COMPLETE", ValidationOutcome: "RUNNING"}, StageValidating},
		{UnitFacts{Status: "VALIDATING", WorkerResult: "COMPLETE", ValidationOutcome: "PASS", CheckpointPending: true}, StageCheckpoint},
		{UnitFacts{Status: "DONE", WorkerResult: "COMPLETE"}, StageDone},
		{UnitFacts{Status: "EXECUTING", WorkerResult: "COMPLETE"}, StageExecuting},
	}
	for _, c := range cases {
		if got := StageOf(c.f); got != c.want {
			t.Errorf("%+v: %s want %s", c.f, got, c.want)
		}
	}
	for _, st := range []string{"PENDING", "READY", "EXECUTING", "INCOMPLETE", "VALIDATING", "FAILED", "BLOCKED"} {
		if StageOf(UnitFacts{Status: st, WorkerResult: "COMPLETE", ValidationOutcome: "PASS"}) == StageDone {
			t.Fatalf("%s shown DONE from worker / validation alone", st)
		}
	}
	r := ReportOf(UnitFacts{Status: "VALIDATING", WorkerResult: "COMPLETE"})
	if r.Worker != "COMPLETE" || r.Overseer != "VALIDATING" {
		t.Fatalf("%+v", r)
	}
}

// P17-21 (WBS 17.163) — live events carry ids / statuses only.
func TestP1721LiveEventMinimized(t *testing.T) {
	exec := uuid.New()
	e := execution.Event{ExecutionID: &exec, Type: execution.EventValidationFailed, CreatedAt: time.Unix(10, 0),
		Payload: map[string]any{
			"work_unit_id": "u-1", "validation_run_id": "r-1", "status": "FAIL",
			"prompt":  "SYSTEM PROMPT SECRET CONTENT",
			"stdout":  strings.Repeat("x", 5000),
			"summary": "worker says <script>alert(1)</script>",
			"attempt": float64(2),
			"note":    "long free text",
		}}
	le := FromExecutionEvent(42, e)
	b, _ := json.Marshal(le)
	s := string(b)
	for _, bad := range []string{"SYSTEM PROMPT", "xxxx", "script", "free text"} {
		if strings.Contains(s, bad) {
			t.Fatalf("payload leaked %q: %s", bad, s)
		}
	}
	if le.ProjectSeq != 42 || le.Refs["work_unit_id"] != "u-1" || le.Refs["validation_run_id"] != "r-1" || le.Status != "FAIL" || le.Category != "validation" || le.ExecutionID != exec.String() {
		t.Fatalf("%+v", le)
	}
}
