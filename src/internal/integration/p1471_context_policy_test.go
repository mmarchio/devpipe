package integration

import (
	"context"
	"testing"

	"overseer/internal/continuation"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/worker"
)

// P14-71 — WBS 14.106 + 14.12: unknown telemetry → fresh session with
// reason context_unknown (default policy); estimated telemetry decides like
// telemetry and is recorded as estimated.
func TestP14106UnknownAndEstimated(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		r := p14NewRig(t)
		d1 := r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "Implement B"), 1000)
		r.telemetry(t, d1, "", 0)
		r.reply(p14Report("complete", []string{"A", "B"}, nil, "Validate."), 2000)
		newBefore := r.ops("new")
		r.process(t)
		c := r.unitContinuations(t)[0]
		if c.Strategy != continuation.ContinueFreshSession || c.Decision.Reason != continuation.ReasonContextUnknown ||
			c.Decision.Utilization != nil || r.ops("new") != newBefore+1 || c.SnapshotID == nil || c.Status != continuation.StatusCompleted {
			t.Fatalf("14.106 %+v %+v", c, c.Decision)
		}
		v, err := r.cs().View(context.Background(), &c)
		if err != nil || v.Context.Known || v.Context.Label != "Unknown" || v.Reason != "Context telemetry unavailable" {
			t.Fatalf("14.79 %+v", v)
		}
	})
	for _, tc := range []struct {
		name   string
		tokens int64
		want   continuation.Strategy
	}{{"estimated_below", 40000, continuation.ContinueSameSession}, {"estimated_at", 51200, continuation.ContinueFreshSession}} {
		t.Run(tc.name, func(t *testing.T) {
			r := p14NewRig(t)
			d1 := r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "Implement B"), 1000)
			r.telemetry(t, d1, worker.SourceEstimated, tc.tokens)
			r.reply(p14Report("complete", []string{"A", "B"}, nil, "Validate."), 2000)
			r.process(t)
			c := r.unitContinuations(t)[0]
			if c.Strategy != tc.want || !c.Decision.Estimated || c.Decision.Source != worker.SourceEstimated {
				t.Fatalf("T14.12 %+v", c.Decision)
			}
		})
	}
	t.Run("actual_beats_estimate", func(t *testing.T) {
		r := p14NewRig(t)
		d1 := r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "Implement B"), 20000)
		// An additional overseer estimate above the threshold must lose
		// against the provider's actual telemetry (WBS 14.11).
		if _, err := r.h.DBExec(context.Background(), "INSERT INTO worker_usage_observations (id, project_id, session_id, dispatch_id, context_tokens, context_window, estimated, source, observed_at) VALUES (gen_random_uuid(), '"+
			d1.ProjectID.String()+"', '"+d1.SessionID.String()+"', '"+d1.ID.String()+"', 63000, 64000, true, 'ESTIMATED', now())"); err != nil {
			t.Fatal(err)
		}
		r.reply(p14Report("complete", []string{"A", "B"}, nil, "Validate."), 2000)
		r.process(t)
		c := r.unitContinuations(t)[0]
		if c.Strategy != continuation.ContinueSameSession || c.Decision.Estimated || *c.Decision.ContextTokens != 20000 {
			t.Fatalf("14.11 %+v", c.Decision)
		}
	})
}

// P14-72 — WBS 14.107 + 14.108 + 14.44-14.46.
func TestP14107ContextLimitAndLostSession(t *testing.T) {
	t.Run("context_limit_initial", func(t *testing.T) {
		r := p14NewRig(t)
		before := r.workflowRows(t)
		d1 := r.fail(t, hermestest.Behavior{Status: 400, ErrorCode: "context_limit_exceeded", Message: "context window exceeded"})
		if d1.Status != worker.DispatchFailed || d1.ErrorCode != string(worker.CodeContextLimit) {
			t.Fatalf("setup %+v", d1)
		}
		var n int
		if err := r.h.DBScan(context.Background(), "SELECT count(*) FROM worker_results WHERE dispatch_id = '"+d1.ID.String()+"'", &n); err != nil || n != 0 {
			t.Fatal("14.107 fake WorkerResult created")
		}
		cs := r.unitContinuations(t)
		if len(cs) != 1 || cs[0].Trigger != continuation.TriggerContextLimit || cs[0].SourceResultID != nil || !cs[0].StatusUnknown ||
			len(cs[0].Captured.Completed) != 0 {
			t.Fatalf("T14.45.2 %+v", cs)
		}
		r.reply(p14Report("complete", []string{"A"}, nil, "Validate."), 2000)
		newBefore := r.ops("new")
		r.process(t)
		c := r.cont(t, cs[0].ID)
		if c.Strategy != continuation.ContinueFreshSession || c.Decision.Reason != continuation.ReasonContextLimit || r.ops("new") != newBefore+1 ||
			c.WorkUnitID != r.pos.UnitID || c.SnapshotID == nil || c.Status != continuation.StatusCompleted {
			t.Fatalf("14.107 %+v", c)
		}
		if r.workflowRows(t) != before {
			t.Fatal("14.120")
		}
	})
	t.Run("context_limit_keeps_known_status", func(t *testing.T) {
		r := p14NewRig(t)
		r.initial(t, p14Report("incomplete", []string{"A", "B"}, []string{"C"}, "Implement C"), 2000)
		r.reply(p14Report("complete", []string{"A", "B", "C"}, nil, "Validate."), 3000)
		r.process(t)
		r.fail(t, hermestest.Behavior{Status: 413, Message: "too large"})
		cs := r.unitContinuations(t)
		c2 := cs[len(cs)-1]
		if len(cs) != 2 || c2.Trigger != continuation.TriggerContextLimit || c2.StatusUnknown || c2.Attempt != 2 ||
			len(c2.Captured.Completed) != 2 || c2.Captured.Remaining[0] != "C" || c2.WorkUnitID != r.pos.UnitID {
			t.Fatalf("T14.45.1/T14.45.3 %+v", cs)
		}
	})
	t.Run("lost_session", func(t *testing.T) {
		r := p14NewRig(t)
		s1 := r.activeSession(t)
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "Implement B"), 19200) // 30%
		r.g.KillSession(s1.ProviderSessionID)
		r.reply(p14Report("complete", []string{"A", "B"}, nil, "Validate."), 2000)
		r.process(t)
		c := r.unitContinuations(t)[0]
		if c.Strategy != continuation.ContinueFreshSession || c.Decision.Reason != continuation.ReasonSessionLost || c.Status != continuation.StatusCompleted {
			t.Fatalf("14.108 %+v %+v", c, c.Decision)
		}
		if r.activeSession(t).ID == s1.ID || r.session(t, s1.ID).Status != worker.SessionLost {
			t.Fatal("14.108 S1 reused")
		}
	})
}
