package integration

import (
	"context"
	"os"
	"syscall"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/faults"
)

// p18RecoveryCondition returns the execution's recorded recovery condition.
func p18RecoveryCondition(t *testing.T, r *p16Rig) string {
	t.Helper()
	var c string
	_ = r.h.DBScan(context.Background(), "SELECT COALESCE(recovery_evidence->>'condition', '') FROM executions WHERE id = '"+r.execID.String()+"'", &c)
	return c
}

// p18BreakRepoAt runs the engine, applying breakRepo the first time it
// reaches (action, "intent"), and asserts the execution stopped in
// recovery with condition and that no worker request followed the
// planner's.
func p18BreakRepoAt(t *testing.T, r *p16Rig, action execution.StepAction, breakRepo func(), condition string, wantMessages int) {
	t.Helper()
	broken := false
	r.eng().SetStepHook(func(ctx context.Context, a execution.StepAction, s string) error {
		if a == action && s == "intent" && !broken {
			broken = true
			breakRepo()
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	res, err := r.eng().Run(context.Background(), r.execID, 100)
	if err != nil || !broken {
		t.Fatalf("run: %v broken=%v %s", err, broken, p16Trace(res))
	}
	if r.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, r) != condition {
		t.Fatalf("status %s condition %q reason %q; %s", r.execStatus(t), p18RecoveryCondition(t, r), r.recoveryReason(t), p16Trace(res))
	}
	if n := len(r.g.Messages()); n != wantMessages {
		t.Fatalf("worker messages %d, want %d", n, wantMessages)
	}
}

// T18.49 — the repository identity is recorded at bootstrap: the real
// root commit, top level and branch.
func TestT1849IdentityRecordedAtBootstrap(t *testing.T) {
	r := p16NewRig(t)
	var root, branch, source string
	if err := r.h.DBScan(context.Background(), "SELECT root_commit, branch, source FROM execution_repository_identities WHERE execution_id = '"+
		r.execID.String()+"'", &root, &branch, &source); err != nil {
		t.Fatal(err)
	}
	if want := p15Git(t, r.dir, "rev-list", "--max-parents=0", "HEAD"); root != want || branch != "main" || source != "BOOTSTRAP" {
		t.Fatalf("identity root %s (want %s) branch %s source %s", root, want, branch, source)
	}
}

// T18.48 — the repository directory disappears before the unit's worker
// dispatch: execution stops with REPOSITORY_MISSING; nothing is sent.
func TestT1848MissingRepositoryPreventsDispatch(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18BreakRepoAt(t, r, execution.ActionDispatch, func() {
		if err := os.Rename(r.dir, r.dir+".gone"); err != nil {
			t.Fatal(err)
		}
	}, "REPOSITORY_MISSING", 1)
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE work_unit_id IS NOT NULL"); n != 0 {
		t.Fatalf("%d unit dispatches", n)
	}
}

// T18.48 — the repository disappears before validation: no validation
// run is started (nothing could falsely PASS or FAIL).
func TestT1848MissingRepositoryPreventsValidation(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18BreakRepoAt(t, r, execution.ActionRunUnitValidation, func() {
		if err := os.RemoveAll(r.dir); err != nil {
			t.Fatal(err)
		}
	}, "REPOSITORY_MISSING", 2)
	if n := r.count(t, "SELECT count(*) FROM validation_runs WHERE execution_id = '"+r.execID.String()+"'"); n != 0 {
		t.Fatalf("%d validation runs", n)
	}
}

// T18.49 — the project directory is replaced by an unrelated git
// repository at the same path: detected (root commit), nothing is sent.
func TestT1849UnrelatedRepositoryDetected(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18BreakRepoAt(t, r, execution.ActionDispatch, func() {
		if err := os.RemoveAll(r.dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(r.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p15Git(t, r.dir, "init", "-q", "-b", "main")
		if err := os.WriteFile(r.dir+"/README.md", []byte("someone else's project\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		p15Git(t, r.dir, "add", "README.md")
		p15Git(t, r.dir, "commit", "-q", "-m", "unrelated")
	}, "REPOSITORY_IDENTITY_MISMATCH", 1)
}

// T18.51 (guard part) — a branch switch stops new work.
func TestT1851BranchSwitchStopsDispatch(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18BreakRepoAt(t, r, execution.ActionDispatch, func() {
		p15Git(t, r.dir, "checkout", "-q", "-b", "elsewhere")
	}, "REPOSITORY_HEAD_CHANGED", 1)
}

// T18.53 — the repository becomes unwritable (injected EROFS on the
// write probe): worker dispatch stops with REPOSITORY_PERMISSION.
func TestT1853UnwritableRepositoryStops(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	// Plan the phase first (the planner dispatch also checks writability).
	reg := faults.NewRegistry()
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	armed := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionDispatch && s == "intent" && !armed {
			armed = true
			reg.Arm(faults.Action{Point: faults.FSWrite, Kind: faults.KindError, Err: syscall.EROFS})
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	res, err := r.eng().Run(ctx, r.execID, 100)
	if err != nil || r.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, r) != "REPOSITORY_PERMISSION" {
		t.Fatalf("err %v status %s condition %q; %s", err, r.execStatus(t), p18RecoveryCondition(t, r), p16Trace(res))
	}
	if n := len(r.g.Messages()); n != 1 {
		t.Fatalf("worker messages %d, want 1 (planner only)", n)
	}
}

// An execution bootstrapped before identities were recorded gets its
// identity at the first guard check and then proceeds normally.
func TestT1849IdentityRecordedAtFirstCheck(t *testing.T) {
	r := p16NewRig(t)
	r.exec(t, "DELETE FROM execution_repository_identities WHERE execution_id = '"+r.execID.String()+"'")
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 100)
	var source string
	_ = r.h.DBScan(context.Background(), "SELECT source FROM execution_repository_identities WHERE execution_id = '"+r.execID.String()+"'", &source)
	if source != "FIRST_CHECK" {
		t.Fatalf("identity source %q", source)
	}
}

// A branch switched right before the unit checkpoint stops the checkpoint
// before any commit lands on the other branch; the unit is not DONE.
func TestT1851BranchSwitchBlocksCheckpoint(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	var before string
	p18BreakRepoAt(t, r, execution.ActionUnitGates, func() {
		p15Git(t, r.dir, "checkout", "-q", "-b", "elsewhere")
		before = p15Git(t, r.dir, "rev-parse", "HEAD")
	}, "REPOSITORY_HEAD_CHANGED", 2)
	if after := p15Git(t, r.dir, "rev-parse", "HEAD"); after != before {
		t.Fatal("a checkpoint was committed onto the switched branch")
	}
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE kind = 'UNIT' AND execution_id = '"+r.execID.String()+"'"); n != 0 {
		t.Fatalf("%d unit checkpoint records", n)
	}
	if s := r.unitStatuses(t, 1); s == "a:DONE" {
		t.Fatal("unit DONE without its checkpoint")
	}
}
