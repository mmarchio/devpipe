package integration

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"overseer/internal/continuation"
	"overseer/internal/prompt"
	"overseer/internal/worker"
)

func (r *p14Rig) doneCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.h.DBScan(context.Background(), "SELECT count(*) FROM work_units WHERE status = 'DONE' AND execution_plan_id = '"+r.pos.PlanID.String()+"'", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (r *p14Rig) consumptions(t *testing.T, where string) int {
	t.Helper()
	var n int
	if err := r.h.DBScan(context.Background(), "SELECT count(*) FROM worker_result_consumptions WHERE "+where, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 14.127 — Phase 14 acceptance: same session.
func TestP14127AcceptanceSameSession(t *testing.T) {
	r := p14NewRig(t)
	s1 := r.activeSession(t)
	done, before := r.doneCount(t), r.workflowRows(t)
	d1 := r.initial(t, p14Report("incomplete", []string{"A", "B"}, []string{"C", "D"}, "C"), 32840) // 51.31%
	r1 := r.result(t, d1)
	r.reply(p14Report("incomplete", []string{"A", "B", "C"}, []string{"D"}, "D"), 34000)
	news := r.ops("new")
	r.cs().SetStepHook(func(c *continuation.Continuation) bool { return c.Attempt == 1 }) // stop the chain after C1
	r.process(t)
	c1 := r.unitContinuations(t)[0]
	if c1.Attempt != 1 || *c1.SourceResultID != r1.ID || r.consumptions(t, "result_id = '"+r1.ID.String()+"'") != 1 {
		t.Fatal("C1 / R1 consumed exactly once")
	}
	if c1.Strategy != continuation.ContinueSameSession || c1.Decision.Reason != continuation.ReasonBelowThreshold {
		t.Fatalf("decision %+v", c1.Decision)
	}
	if r.session(t, s1.ID).Status != worker.SessionActive || r.ops("new") != news {
		t.Fatal("S1 not ACTIVE / new session created")
	}
	if c1.SnapshotID == nil || c1.SourceSnapshotID == nil || *c1.SnapshotID == *c1.SourceSnapshotID {
		t.Fatal("continuation context snapshot not created")
	}
	if p14TemplateKey(t, r, *c1.WorkerPromptID) != "CONTINUE_WORK_UNIT" {
		t.Fatal("CONTINUE_WORK_UNIT prompt not generated")
	}
	ms := r.g.MessagesFor(s1.ProviderSessionID)
	last := ms[len(ms)-1].Content
	if !strings.Contains(last, "completed: A; B") || !strings.Contains(last, "remaining: C; D") || !strings.Contains(last, prompt.PriorWorkerReportOpen) {
		t.Fatal("R1 structured status not included")
	}
	if d2 := r.dispatchOf(t, *c1.DispatchID).Dispatch; d2.SessionID != s1.ID || *d2.WorkUnitID != r.pos.UnitID || *d2.PhaseID != r.pos.PhaseID {
		t.Fatal("prompt not dispatched through S1 for the same unit / phase")
	}
	if r.doneCount(t) != done || r.workflowRows(t) != before {
		t.Fatal("WBS DONE count / work unit / phase changed")
	}
}

// 14.128 — Phase 14 acceptance: fresh session, exact persisted sequence.
func TestP14128AcceptanceFreshSession(t *testing.T) {
	r := p14NewRig(t)
	s1 := r.activeSession(t)
	done, before := r.doneCount(t), r.workflowRows(t)
	r.initial(t, p14Report("incomplete", []string{"A", "B"}, []string{"C", "D"}, "C"), 58000) // 90.625%
	r.reply(p14Report("complete", []string{"A", "B", "C", "D"}, nil, "Validate."), 5000)
	from := len(r.g.Ops())
	r.process(t)
	c1 := r.unitContinuations(t)[0]
	want := []string{"PENDING", "DECIDING", "RESET_REQUIRED", "CREATING_SESSION", "RESTORING_CONTEXT", "RESTORING_CWD", "READY", "DISPATCHING", "WAITING", "COMPLETED"}
	if got := r.history(t, c1.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("sequence %v", got)
	}
	if got := r.opLog(from); !reflect.DeepEqual(got, []string{"new", "cd", "msg"}) {
		t.Fatalf("external order %v", got)
	}
	s2 := r.activeSession(t)
	if r.session(t, s1.ID).Status != worker.SessionReset || *c1.NewSessionID != s2.ID || c1.Decision.Reason != continuation.ReasonAboveThreshold {
		t.Fatal("S2 confirmed / S1 RESET")
	}
	d2 := r.dispatchOf(t, *c1.DispatchID).Dispatch
	if d2.SessionID != s2.ID || d2.ProjectID != r.pos.ProjectID || *d2.PhaseID != r.pos.PhaseID || *d2.WorkUnitID != r.pos.UnitID {
		t.Fatal("same project / phase / unit through S2")
	}
	if c1.Captured.Remaining[1] != "D" || len(r.g.MessagesFor(s2.ProviderSessionID)) != 1 {
		t.Fatal("captured status / old conversation")
	}
	if r.doneCount(t) != done || r.workflowRows(t) != before {
		t.Fatal("WBS progress changed")
	}
}

// 14.129 — Phase 14 acceptance: multi-continuation chain through the real
// background runner (managed execution, WBS 14.61/14.62).
func TestP14129AcceptanceChain(t *testing.T) {
	r := p14NewRigWith(t, true)
	s1 := r.activeSession(t)
	done, before := r.doneCount(t), r.workflowRows(t)
	// Initial reply (45%), then the replies to C1 (72%), C2 (86%), C3.
	content := p14Report("incomplete", []string{"A"}, []string{"B", "C", "D"}, "B")
	r.g.Script(p14Behavior(content, 28800))
	r.g.Script(p14Behavior(p14Report("incomplete", []string{"A", "B"}, []string{"C", "D"}, "C"), 46080))
	r.g.Script(p14Behavior(p14Report("incomplete", []string{"A", "B", "C"}, []string{"D"}, "D"), 55040))
	r.g.Script(p14Behavior(p14Report("complete", []string{"A", "B", "C", "D"}, nil, "Run deterministic validation."), 5000))
	r.attempt++
	r.dispatch(t, r.wp, r.attempt)

	deadline := time.Now().Add(30 * time.Second)
	var cs []continuation.Continuation
	for time.Now().Before(deadline) {
		cs = r.unitContinuations(t)
		if len(cs) == 3 && cs[2].Status == continuation.StatusCompleted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(cs) != 3 || cs[2].Status != continuation.StatusCompleted {
		t.Fatalf("chain did not finish: %+v", cs)
	}
	strategies := []continuation.Strategy{cs[0].Strategy, cs[1].Strategy, cs[2].Strategy}
	if !reflect.DeepEqual(strategies, []continuation.Strategy{continuation.ContinueSameSession, continuation.ContinueSameSession, continuation.ContinueFreshSession}) {
		t.Fatalf("strategies %v", strategies)
	}
	if *cs[0].SessionID != s1.ID || *cs[1].SessionID != s1.ID || *cs[2].NewSessionID == s1.ID {
		t.Fatal("S1, S1, S2 expected")
	}
	counts, err := r.cs().Metrics(context.Background(), r.pos.ProjectID)
	if err != nil || counts.PerWorkUnit[r.pos.UnitID].Continuations != 3 || counts.PerWorkUnit[r.pos.UnitID].SessionResets != 1 {
		t.Fatalf("3 attempts / 1 reset: %+v", counts)
	}
	snaps := map[string]bool{}
	for i, c := range cs {
		if c.WorkUnitID != r.pos.UnitID || c.Attempt != i+1 || c.SnapshotID == nil || c.WorkerPromptID == nil {
			t.Fatalf("continuation %d %+v", i, c)
		}
		snaps[c.SnapshotID.String()] = true
	}
	if len(snaps) != 3 {
		t.Fatal("each status snapshot retained")
	}
	var results, dup int
	if err := r.h.DBScan(context.Background(), "SELECT count(*), count(*) - count(DISTINCT result_id) FROM worker_result_consumptions WHERE project_id = '"+
		r.pos.ProjectID.String()+"'", &results, &dup); err != nil || results != 4 || dup != 0 {
		t.Fatalf("each result consumed once: %d (%d dup)", results, dup)
	}
	if last, err := r.rs().ResultByDispatch(context.Background(), r.pos.ProjectID, *cs[2].DispatchID); err != nil || !last.Result.IsComplete() {
		t.Fatal("COMPLETE forwarded toward validation")
	}
	if r.doneCount(t) != done || r.workflowRows(t) != before {
		t.Fatal("work unit automatically DONE / workflow advanced")
	}
}
