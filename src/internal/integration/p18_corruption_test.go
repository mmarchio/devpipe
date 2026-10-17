package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/execution"
)

// T18.199 — the context snapshot recorded for a dispatch step is altered
// before the prompt is (re)composed: corruption enters recovery and no
// prompt is composed or sent from it.
func TestT18199CorruptSnapshot(t *testing.T) {
	r, restart := p16RestartRig(t, p16Options{noBootstrap: true})
	// The roles' prompts include PROJECT_STATE, so the snapshot has a file.
	r.exec(t, "INSERT INTO role_state_files (id, role_id, state_file_key, position) SELECT gen_random_uuid(), id, 'PROJECT_STATE', 99 FROM roles WHERE project_id = '"+
		r.pid.String()+"' ON CONFLICT DO NOTHING")
	r.boot(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	crashed := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionDispatch && s == "dispatch" && !crashed {
			crashed = true
			return errP16Crash
		}
		return nil
	})
	if _, err := r.eng().Run(context.Background(), r.execID, 100); !errors.Is(err, errP16Crash) {
		t.Fatalf("no crash: %v", err)
	}
	sent := len(r.g.Messages())
	r.exec(t, `SET session_replication_role = replica; UPDATE context_snapshot_files SET content = content || ' INJECTED' WHERE snapshot_id =
		(SELECT context_snapshot_id FROM execution_steps WHERE execution_id = '`+r.execID.String()+`' AND action = 'DISPATCH' ORDER BY seq DESC LIMIT 1)`)
	if n := r.count(t, "SELECT count(*) FROM context_snapshot_files WHERE content LIKE '% INJECTED'"); n == 0 {
		t.Fatal("setup: no snapshot file was tampered")
	}
	restart()
	res, _ := r.eng().Run(context.Background(), r.execID, 100)
	if p18RecoveryCondition(t, r) != "ARTIFACT_CORRUPT" {
		t.Fatalf("condition %q reason %q; %s", p18RecoveryCondition(t, r), r.recoveryReason(t), p16Trace(res))
	}
	if len(r.g.Messages()) != sent {
		t.Fatal("a prompt was sent from a corrupt snapshot")
	}
	p18NotDone(t, r)
}

// T18.202 — a stored PASS validation run lost a required command's result:
// it cannot complete the unit.
func TestT18202IncompleteValidationCannotPass(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	tampered := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionUnitGates && s == "intent" && !tampered {
			tampered = true
			r.exec(t, `SET session_replication_role = replica; DELETE FROM validation_command_results WHERE run_id IN
				(SELECT id FROM validation_runs WHERE execution_id = '`+r.execID.String()+`' AND scope = 'UNIT')`)
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	res, _ := r.eng().Run(context.Background(), r.execID, 100)
	if !tampered || p18RecoveryCondition(t, r) != "ARTIFACT_CORRUPT" {
		t.Fatalf("condition %q reason %q; %s", p18RecoveryCondition(t, r), r.recoveryReason(t), p16Trace(res))
	}
	p18NotDone(t, r)
}
