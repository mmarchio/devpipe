package integration

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"

	"overseer/internal/execution"
	"overseer/internal/faults"
)

// p18ReplaceCheck replaces the rig's validation script for the rest of
// the test.
func p18ReplaceCheck(t *testing.T, r *p16Rig, body string) {
	t.Helper()
	if err := os.WriteFile(r.script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// p18NotDone asserts no work unit was marked DONE and no DONE event exists.
func p18NotDone(t *testing.T, r *p16Rig) {
	t.Helper()
	if s := r.unitStatuses(t, 1); strings.Contains(s, "DONE") {
		t.Fatalf("unit falsely DONE: %s", s)
	}
	if countOf(r.events(t), "WORK_UNIT_DONE") != 0 {
		t.Fatal("WORK_UNIT_DONE recorded")
	}
}

// T18.54 — validation commands fail because the disk is full: DISK_FULL
// recovery (operator required), the run is not PASS / FAIL, the unit is
// not DONE and no FIX dispatch is attempted.
func TestT1854DiskFullDuringValidation(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18ReplaceCheck(t, r, `echo "write /tmp/go-build1/a.o: no space left on device" >&2; exit 1`)
	res, err := r.eng().Run(context.Background(), r.execID, 100)
	if err != nil || r.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, r) != "DISK_FULL" {
		t.Fatalf("err %v status %s condition %q reason %q; %s", err, r.execStatus(t), p18RecoveryCondition(t, r), r.recoveryReason(t), p16Trace(res))
	}
	p18NotDone(t, r)
	if n := r.count(t, "SELECT count(*) FROM validation_runs WHERE execution_id = '"+r.execID.String()+"' AND status IN ('PASS', 'FAIL')"); n != 0 {
		t.Fatalf("%d validation runs recorded as PASS / FAIL", n)
	}
	if n := len(r.g.Messages()); n != 2 {
		t.Fatalf("worker messages %d (a FIX dispatch followed the disk-full validation)", n)
	}
}

// T18.54 — the unit checkpoint commit fails with ENOSPC (REQUIRED
// policy): the unit is not DONE and the recovery names the full disk.
func TestT1854DiskFullDuringCheckpoint(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	reg := faults.NewRegistry().Arm(faults.Action{Point: faults.BeforeGitCommit, Kind: faults.KindError,
		Err: fmt.Errorf("write .git/objects: %w", syscall.ENOSPC)})
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	res, err := r.eng().Run(ctx, r.execID, 100)
	if err != nil || p18RecoveryCondition(t, r) != "DISK_FULL" || !strings.Contains(r.recoveryReason(t), "required checkpoint failed") {
		t.Fatalf("err %v condition %q reason %q; %s", err, p18RecoveryCondition(t, r), r.recoveryReason(t), p16Trace(res))
	}
	p18NotDone(t, r)
}

// T18.53 — write permission is lost before the checkpoint (read-only
// filesystem): the commit fails, the unit is not DONE and the condition
// is REPOSITORY_PERMISSION.
func TestT1853PermissionLossDuringCheckpoint(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	reg := faults.NewRegistry()
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionUnitGates && s == "intent" {
			reg.Arm(faults.Action{Point: faults.BeforeGitCommit, Kind: faults.KindError, Err: fmt.Errorf("git commit: %w", syscall.EROFS)})
			reg.Arm(faults.Action{Point: faults.FSWrite, Kind: faults.KindError, Err: syscall.EROFS})
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	res, err := r.eng().Run(ctx, r.execID, 100)
	if err != nil || p18RecoveryCondition(t, r) != "REPOSITORY_PERMISSION" {
		t.Fatalf("err %v condition %q reason %q; %s", err, p18RecoveryCondition(t, r), r.recoveryReason(t), p16Trace(res))
	}
	p18NotDone(t, r)
}

// T18.52 — the repository is deleted while validation runs (the command
// itself would PASS): the outcome is discarded (ABANDONED, never PASS),
// the execution stops with REPOSITORY_MISSING, and the unit is not DONE.
func TestT1852RepositoryDeletedDuringValidation(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18ReplaceCheck(t, r, `rm -rf "`+r.dir+`"; exit 0`)
	res, err := r.eng().Run(context.Background(), r.execID, 100)
	if err != nil || p18RecoveryCondition(t, r) != "REPOSITORY_MISSING" {
		t.Fatalf("err %v condition %q reason %q; %s", err, p18RecoveryCondition(t, r), r.recoveryReason(t), p16Trace(res))
	}
	var status string
	_ = r.h.DBScan(context.Background(), "SELECT status FROM validation_runs WHERE execution_id = '"+r.execID.String()+"' ORDER BY started_at DESC LIMIT 1", &status)
	if status != "ABANDONED" {
		t.Fatalf("validation run %s, want ABANDONED", status)
	}
	p18NotDone(t, r)
}
