package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// p18CrashAtCheckpoint drives a restartable rig to a crash at the unit
// checkpoint stage ("checkpoint" = before the commit, "checkpoint-
// committed" = after the commit, before its database record).
func p18CrashAtCheckpoint(t *testing.T, stage string) (*p16Rig, func() *Harness) {
	t.Helper()
	r, restart := p16RestartRig(t, p16Options{})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	crashed := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionUnitGates && s == stage && !crashed {
			crashed = true
			return errP16Crash
		}
		return nil
	})
	if _, err := r.eng().Run(context.Background(), r.execID, 100); !errors.Is(err, errP16Crash) {
		t.Fatalf("run did not crash at %s: %v", stage, err)
	}
	return r, restart
}

func p18TrailerCommits(t *testing.T, r *p16Rig) int {
	t.Helper()
	return strings.Count(p15Git(t, r.dir, "log", "--format=%s"), "Unit a")
}

// T18.147 / T18.58 — killed after the git commit, before the database
// record: the restarted instance finds the commit by its trailer,
// verifies its parent and adopts it — no duplicate commit, unit DONE.
func TestT18147CrashAfterCommitAdopted(t *testing.T) {
	r, restart := p18CrashAtCheckpoint(t, "checkpoint-committed")
	committed := p15Git(t, r.dir, "rev-parse", "HEAD")
	restart()
	r.runUntil(t, execution.StepCompleted, 100)
	if n := p18TrailerCommits(t, r); n != 1 {
		t.Fatalf("unit checkpoint commits %d", n)
	}
	var commit string
	_ = r.h.DBScan(context.Background(), "SELECT commit_sha FROM repository_checkpoints WHERE kind = 'UNIT' AND execution_id = '"+r.execID.String()+"'", &commit)
	if commit != committed {
		t.Fatalf("recorded %s, want the existing commit %s", commit, committed)
	}
	var reconciled string
	_ = r.h.DBScan(context.Background(), "SELECT COALESCE(payload->>'reconciled', '') FROM execution_events WHERE execution_id = '"+r.execID.String()+
		"' AND type = 'CHECKPOINT_CREATED' AND payload->>'kind' = 'UNIT'", &reconciled)
	if reconciled != "true" {
		t.Fatal("the checkpoint was not recorded as reconciled")
	}
}

// T18.128 — killed after the checkpoint intent, before the commit: the
// restarted instance determines no commit exists and commits once.
func TestT18128CrashBeforeCommit(t *testing.T) {
	r, restart := p18CrashAtCheckpoint(t, "checkpoint")
	if p18TrailerCommits(t, r) != 0 {
		t.Fatal("committed before the crash point")
	}
	restart()
	r.runUntil(t, execution.StepCompleted, 100)
	if n := p18TrailerCommits(t, r); n != 1 {
		t.Fatalf("unit checkpoint commits %d", n)
	}
}

// T18.59 — the commit status cannot be determined confidently: the
// execution enters CHECKPOINT_AMBIGUOUS recovery and the unit is not
// marked DONE on a guess.
func TestT1859AmbiguousCheckpoint(t *testing.T) {
	cases := map[string]func(t *testing.T, r *p16Rig){
		"two commits carry the trailer": func(t *testing.T, r *p16Rig) {
			msg := p15Git(t, r.dir, "log", "-1", "--format=%B")
			p15Git(t, r.dir, "commit", "-q", "--allow-empty", "-m", msg)
		},
		"trailer commit on an unexpected parent": func(t *testing.T, r *p16Rig) {
			cp := p15Git(t, r.dir, "rev-parse", "HEAD")
			p15Git(t, r.dir, "reset", "-q", "--hard", "HEAD~1")
			p18ExternalCommit(t, r.dir, "SOMEONE_ELSE.md")
			p15Git(t, r.dir, "cherry-pick", "--allow-empty", "--keep-redundant-commits", cp)
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			r, restart := p18CrashAtCheckpoint(t, "checkpoint-committed")
			tamper(t, r)
			before := p15Git(t, r.dir, "rev-list", "--count", "HEAD")
			restart()
			res, _ := r.eng().Run(context.Background(), r.execID, 100)
			if r.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, r) != "CHECKPOINT_AMBIGUOUS" {
				t.Fatalf("status %s condition %q reason %q; %s", r.execStatus(t), p18RecoveryCondition(t, r), r.recoveryReason(t), p16Trace(res))
			}
			if strings.Contains(r.unitStatuses(t, 1), "DONE") {
				t.Fatal("unit DONE on a guessed checkpoint")
			}
			if after := p15Git(t, r.dir, "rev-list", "--count", "HEAD"); after != before {
				t.Fatal("a new commit was made instead of stopping")
			}
		})
	}
}
