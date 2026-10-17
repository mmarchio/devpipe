package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"overseer/internal/config"
	"overseer/internal/execution"
)

var errP16Crash = errors.New("simulated crash")

// p16RestartRig builds a p16 rig on a dedicated database; boot() starts a
// fresh overseer instance (restart) on it with startup recovery enabled.
func p16RestartRig(t *testing.T, o p16Options) (*p16Rig, func() *Harness) {
	t.Helper()
	phases := o.phases
	if len(phases) == 0 {
		phases = []p15Phase{{1, "Foundation", "PENDING", 1}}
	}
	base, boot := p15RestartRig(t, phases, o.config)
	r := &p16Rig{p15Rig: base}
	p16Configure(t, r, o)
	return r, func() *Harness {
		r.h.Close()
		h := boot(func(c *config.Config) { c.Engine.RecoveryDisabled = false })
		r.h = h
		return h
	}
}

// TestP16Restart — WBS 16.77–16.80, 16.83: a fresh instance reconstructs
// the execution from the database at every crash point and resumes with no
// duplicate dispatch, checkpoint or DONE event.
func TestP16Restart(t *testing.T) {
	type crash struct {
		name      string
		action    execution.StepAction
		stage     string
		state     string
		wantSends int
	}
	for _, c := range []crash{
		{"after intent before dispatch", execution.ActionDispatch, "dispatch", "IMPLEMENT", 2},
		{"after response before evaluation", execution.ActionDispatch, "dispatched", "IMPLEMENT", 2},
		{"during validation", execution.ActionRunUnitValidation, "validation", "VALIDATE", 2},
		{"after commit before checkpoint row", execution.ActionUnitGates, "checkpoint-committed", "UNIT_DONE", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, restart := p16RestartRig(t, p16Options{})
			r.queuePlan(r.planJSON(p16Unit{key: "a"}))
			r.queue("complete")
			crashed := false
			r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
				if a == c.action && stage == c.stage && !crashed {
					crashed = true
					return errP16Crash
				}
				return nil
			})
			if _, err := r.eng().Run(context.Background(), r.execID, 100); !errors.Is(err, errP16Crash) {
				t.Fatalf("run did not crash: %v", err)
			}
			if r.state(t) != c.state {
				t.Fatalf("crashed at %s, want %s", r.state(t), c.state)
			}
			restart()
			r.runUntil(t, execution.StepCompleted, 100)
			if len(r.g.Messages()) != c.wantSends {
				t.Fatalf("worker messages = %d, want %d", len(r.g.Messages()), c.wantSends)
			}
			evs := r.events(t)
			if countOf(evs, "WORK_UNIT_DONE") != 1 || countOf(evs, "PROJECT_COMPLETED") != 1 {
				t.Fatalf("duplicate effects: %v", evs)
			}
			if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE status = 'CREATED' AND execution_id = '"+r.execID.String()+"'"); n != 2 {
				t.Fatalf("checkpoints = %d", n)
			}
			if out := p15Git(t, r.dir, "log", "--format=%s"); strings.Count(out, "unit checkpoint") != 1 {
				t.Fatalf("duplicate checkpoint commits:\n%s", out)
			}
			if c.stage == "validation" {
				if n := r.count(t, "SELECT count(*) FROM validation_runs WHERE status = 'ABANDONED' AND execution_id = '"+r.execID.String()+"'"); n != 1 {
					t.Fatal("interrupted validation not abandoned")
				}
			}
		})
	}
}

// TestP16RestartUnknownDelivery — WBS 16.78: a dispatch in flight when the
// instance dies is reconciled as unknown and never resent.
func TestP16RestartUnknownDelivery(t *testing.T) {
	r, restart := p16RestartRig(t, p16Options{})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, stage string) error {
		if a == execution.ActionDispatch && stage == "dispatched" {
			// The process dies after the send; the dispatch row is left in
			// flight as if the response never arrived.
			r.exec(t, "ALTER TABLE worker_dispatches DISABLE TRIGGER ALL")
			r.exec(t, "UPDATE worker_dispatches SET status = 'WAITING', delivery = NULL, completed_at = NULL WHERE state_key = 'IMPLEMENT' AND "+r.where())
			r.exec(t, "ALTER TABLE worker_dispatches ENABLE TRIGGER ALL")
			return errP16Crash
		}
		return nil
	})
	if _, err := r.eng().Run(context.Background(), r.execID, 100); !errors.Is(err, errP16Crash) {
		t.Fatalf("no crash: %v", err)
	}
	restart()
	sends := len(r.g.Messages())
	r.runUntil(t, execution.StepRecoveryRequired, 50)
	if len(r.g.Messages()) != sends || !strings.Contains(r.recoveryReason(t), "delivery unknown") {
		t.Fatalf("sends %d→%d reason %q", sends, len(r.g.Messages()), r.recoveryReason(t))
	}
}
