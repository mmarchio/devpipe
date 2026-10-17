package integration

import (
	"context"
	"testing"

	"overseer/internal/continuation"
)

func (r *p14Rig) restart(t *testing.T, boot func() *Harness) {
	t.Helper()
	r.h.Close()
	r.h = boot()
}

// stopAt runs the controller until the continuation reaches status (a
// crash right before that step), then removes the hook.
func (r *p14Rig) stopAt(t *testing.T, status continuation.Status) *continuation.Continuation {
	t.Helper()
	r.cs().SetStepHook(func(c *continuation.Continuation) bool { return c.Status != status })
	r.process(t)
	r.cs().SetStepHook(nil)
	c := r.unitContinuations(t)[0]
	if c.Status != status {
		t.Fatalf("setup: continuation at %s, want %s", c.Status, status)
	}
	return &c
}

// P14-77 — WBS 14.94-14.100 + 14.117 + 14.130: PostgreSQL reconstructs the
// exact continuation position and every external side effect happens once.
func TestP14117RestartRecovery(t *testing.T) {
	complete := p14Report("complete", []string{"A", "B"}, nil, "Validate.")
	t.Run("crash_before_dispatch_after_reset", func(t *testing.T) { // 14.117
		r, boot := p14RestartRig(t)
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 57600) // 90%
		c := r.stopAt(t, continuation.StatusReady)
		s2 := *c.NewSessionID
		news, msgs := r.ops("new"), r.ops("msg")
		r.restart(t, boot)
		r.reply(complete, 3000)
		r.process(t)
		got := r.cont(t, c.ID)
		if got.Status != continuation.StatusCompleted || r.ops("new") != news || r.ops("msg") != msgs+1 {
			t.Fatalf("14.117 %s new=%d msg=%d", got.Status, r.ops("new")-news, r.ops("msg")-msgs)
		}
		if d := r.dispatchOf(t, *got.DispatchID).Dispatch; d.SessionID != s2 {
			t.Fatal("14.117 S2 not reconciled")
		}
	})
	t.Run("restart_across_reset_before_cwd", func(t *testing.T) { // 14.130 / 14.97
		r, boot := p14RestartRig(t)
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 57600)
		c := r.stopAt(t, continuation.StatusRestoringCWD)
		news, msgs := r.ops("new"), r.ops("msg")
		r.restart(t, boot)
		r.reply(complete, 3000)
		cds := r.ops("cd")
		r.process(t)
		got := r.cont(t, c.ID)
		if got.Status != continuation.StatusCompleted || r.ops("new") != news || r.ops("msg") != msgs+1 || r.ops("cd") != cds+1 ||
			*got.NewSessionID != *c.NewSessionID || got.WorkUnitID != r.pos.UnitID {
			t.Fatalf("14.130 %s new=%d cd=%d", got.Status, r.ops("new")-news, r.ops("cd")-cds)
		}
	})
	t.Run("restart_ready_same_session", func(t *testing.T) { // 14.95
		r, boot := p14RestartRig(t)
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 10000)
		c := r.stopAt(t, continuation.StatusReady)
		msgs := r.ops("msg")
		r.restart(t, boot)
		r.reply(complete, 12000)
		r.process(t)
		r.process(t)
		if got := r.cont(t, c.ID); got.Status != continuation.StatusCompleted || r.ops("msg") != msgs+1 {
			t.Fatalf("14.95 %s sends=%d", got.Status, r.ops("msg")-msgs)
		}
	})
	t.Run("restart_creating_session", func(t *testing.T) { // 14.96
		for _, created := range []bool{false, true} {
			r, boot := p14RestartRig(t)
			r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 57600)
			var c *continuation.Continuation
			if created {
				// The process died after Hermes confirmed S2 (and Phase 12
				// persisted it) but before RESTORING_CONTEXT was persisted.
				c = r.stopAt(t, continuation.StatusRestoringContext)
				r.force(t, "UPDATE continuations SET status = 'CREATING_SESSION', new_session_id = NULL, session_confirmed_at = NULL, "+
					"updated_at = (SELECT created_at FROM worker_sessions WHERE id = '"+c.NewSessionID.String()+"') - interval '1 second' WHERE id = '"+c.ID.String()+"'")
			} else {
				c = r.stopAt(t, continuation.StatusCreatingSession)
			}
			news := r.ops("new")
			r.restart(t, boot)
			r.reply(complete, 3000)
			r.process(t)
			got := r.cont(t, c.ID)
			want := 1
			if created {
				want = 0
			}
			if got.Status != continuation.StatusCompleted || r.ops("new")-news != want {
				t.Fatalf("14.96 created=%v: %s, %d new sessions", created, got.Status, r.ops("new")-news)
			}
		}
	})
	t.Run("restart_deciding", func(t *testing.T) { // 14.94
		r, boot := p14RestartRig(t)
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 40000)
		c := r.stopAt(t, continuation.StatusSameSession)
		first := string(continuation.EncodeDecision(*c.Decision))
		r.force(t, "UPDATE continuations SET status = 'DECIDING', strategy = NULL, decision_reason = NULL, decision = NULL, session_id = NULL WHERE id = '"+c.ID.String()+"'")
		r.restart(t, boot)
		got := r.stopAt(t, continuation.StatusSameSession)
		if string(continuation.EncodeDecision(*got.Decision)) != first {
			t.Fatalf("14.94 decision changed: %s vs %s", first, continuation.EncodeDecision(*got.Decision))
		}
	})
	t.Run("restart_dispatching_and_waiting", func(t *testing.T) { // 14.98 / 14.99
		for _, st := range []string{"DISPATCHING", "WAITING"} {
			r, boot := p14RestartRig(t)
			r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B", "C"}, "B"), 10000)
			r.reply(p14Report("incomplete", []string{"A", "B"}, []string{"C"}, "C"), 12000)
			r.process(t)
			cs := r.unitContinuations(t)
			if len(cs) != 2 {
				t.Fatalf("setup %+v", cs)
			}
			c1, c2 := cs[0], cs[1]
			set := "status = '" + st + "', completed_at = NULL"
			if st == "DISPATCHING" {
				set += ", dispatch_id = NULL, updated_at = (SELECT created_at FROM worker_dispatches WHERE id = '" + c1.DispatchID.String() + "') - interval '1 second'"
			}
			// Crash after the response was parsed, before the
			// continuation consumed it.
			r.force(t, "DELETE FROM continuations WHERE id = '"+c2.ID.String()+"'",
				"DELETE FROM worker_result_consumptions WHERE continuation_id = '"+c1.ID.String()+"'",
				"UPDATE continuations SET "+set+" WHERE id = '"+c1.ID.String()+"'")
			msgs := r.ops("msg")
			r.restart(t, boot)
			cs = r.unitContinuations(t)
			if len(cs) != 2 || cs[0].Status != continuation.StatusCompleted || cs[1].Status != continuation.StatusPending ||
				*cs[0].DispatchID != *c1.DispatchID || r.ops("msg") != msgs {
				t.Fatalf("14.98/14.99 %s: %+v", st, cs)
			}
		}
	})
	t.Run("restart_waiting_delivery_unknown", func(t *testing.T) { // 14.99 unknown
		r, boot := p14RestartRig(t)
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 10000)
		r.reply(complete, 12000)
		r.process(t)
		c := r.unitContinuations(t)[0]
		// The process died while D2 was in flight: Phase 12 recovery finds
		// it SENDING and declares delivery unknown on restart.
		r.force(t, "DELETE FROM worker_result_consumptions WHERE continuation_id = '"+c.ID.String()+"'",
			"DELETE FROM worker_result_parse_attempts WHERE dispatch_id = '"+c.DispatchID.String()+"'",
			"DELETE FROM worker_results WHERE dispatch_id = '"+c.DispatchID.String()+"'",
			"DELETE FROM worker_responses WHERE dispatch_id = '"+c.DispatchID.String()+"'",
			"UPDATE worker_dispatches SET status = 'WAITING', delivery = NULL, completed_at = NULL WHERE id = '"+c.DispatchID.String()+"'",
			"UPDATE continuations SET status = 'WAITING', completed_at = NULL WHERE id = '"+c.ID.String()+"'")
		msgs := r.ops("msg")
		r.restart(t, boot)
		r.process(t)
		got := r.cont(t, c.ID)
		if got.Status != continuation.StatusRecoveryRequired || got.RecoveryReason != continuation.RecoveryDeliveryUnknown ||
			r.ops("msg") != msgs || len(r.unitContinuations(t)) != 1 {
			t.Fatalf("14.99 unknown: %s %s", got.Status, got.RecoveryReason)
		}
	})
	t.Run("restart_after_incomplete_response", func(t *testing.T) { // 14.100
		r, boot := p14RestartRig(t)
		r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 10000)
		c := r.unitContinuations(t)[0]
		r.force(t, "DELETE FROM continuations WHERE id = '"+c.ID.String()+"'") // consumption cascades
		r.restart(t, boot)
		if _, err := r.cs().Recover(context.Background()); err != nil {
			t.Fatal(err)
		}
		cs := r.unitContinuations(t)
		if len(cs) != 1 || cs[0].SourceDispatchID != c.SourceDispatchID || cs[0].Status != continuation.StatusPending {
			t.Fatalf("14.100 %+v", cs)
		}
	})
}
