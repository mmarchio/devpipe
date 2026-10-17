package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/hardening"
)

func p18Hook(t *testing.T, r *p16Rig, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, ".git", "hooks", name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// T18.57 — the index is locked by another git process at checkpoint
// time: after the bounded retry the execution waits with the checkpoint
// still PENDING (not FAILED, unit not DONE); once the lock is gone it
// completes with exactly one unit checkpoint commit.
func TestT1857IndexLockKeepsCheckpointPending(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	lock := filepath.Join(r.dir, ".git", "index.lock")
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == execution.ActionUnitGates && s == "checkpoint" {
			_ = os.WriteFile(lock, nil, 0o644)
		}
		return nil
	})
	res, err := r.eng().Run(context.Background(), r.execID, 100)
	r.eng().SetStepHook(nil)
	if err != nil || r.execStatus(t) != "WAITING" {
		t.Fatalf("err %v status %s; %s", err, r.execStatus(t), p16Trace(res))
	}
	var reason, cp string
	_ = r.h.DBScan(context.Background(), "SELECT COALESCE(wait_reason, '') FROM executions WHERE id = '"+r.execID.String()+"'", &reason)
	_ = r.h.DBScan(context.Background(), "SELECT status FROM repository_checkpoints WHERE kind = 'UNIT' AND execution_id = '"+r.execID.String()+"'", &cp)
	if !strings.Contains(reason, "repository busy") || cp != "PENDING" || strings.Contains(r.unitStatuses(t, 1), "DONE") {
		t.Fatalf("reason %q checkpoint %s units %s", reason, cp, r.unitStatuses(t, 1))
	}
	_ = os.Remove(lock)
	r.runUntil(t, execution.StepCompleted, 100)
	if out := p15Git(t, r.dir, "log", "--format=%s"); strings.Count(out, "Unit a") != 1 {
		t.Fatalf("unit checkpoint commits:\n%s", out)
	}
}

// T18.60 — a hung git (a commit hook that never returns) is killed at the
// configured timeout: the run returns promptly (the scheduler is not
// blocked), the checkpoint stays recoverable, and after the hook is fixed
// the execution completes without a duplicate commit.
func TestT1860HungGitDoesNotBlock(t *testing.T) {
	r := p16NewRigWith(t, p16Options{config: func(c *config.Config) {
		c.Engine.RunGitHooks = true
		c.Engine.GitTimeout = time.Second
	}})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	p18Hook(t, r, "pre-commit", "sleep 60")
	start := time.Now()
	res, err := r.eng().Run(context.Background(), r.execID, 100)
	if took := time.Since(start); err != nil || took > 30*time.Second || r.execStatus(t) != "WAITING" {
		t.Fatalf("err %v took %s status %s; %s", err, took, r.execStatus(t), p16Trace(res))
	}
	if strings.Contains(r.unitStatuses(t, 1), "DONE") {
		t.Fatal("unit DONE without its checkpoint")
	}
	_ = os.Remove(filepath.Join(r.dir, ".git", "hooks", "pre-commit"))
	r.runUntil(t, execution.StepCompleted, 100)
	if out := p15Git(t, r.dir, "log", "--format=%s"); strings.Count(out, "Unit a") != 1 {
		t.Fatalf("unit checkpoint commits:\n%s", out)
	}
}

// T18.61 — with hooks enabled, a hook rejecting the checkpoint fails it:
// REQUIRED blocks DONE (recovery); BEST_EFFORT records the failure and
// proceeds. With hooks off (default) the same hook is never run.
func TestT1861HookRejection(t *testing.T) {
	t.Run("REQUIRED", func(t *testing.T) {
		r := p16NewRigWith(t, p16Options{config: func(c *config.Config) { c.Engine.RunGitHooks = true }})
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		p18Hook(t, r, "pre-commit", "echo 'policy: commit rejected' >&2; exit 1")
		r.runUntil(t, execution.StepRecoveryRequired, 100)
		if !strings.Contains(r.recoveryReason(t), "required checkpoint failed") || strings.Contains(r.unitStatuses(t, 1), "DONE") {
			t.Fatalf("reason %q units %s", r.recoveryReason(t), r.unitStatuses(t, 1))
		}
		var cpErr string
		_ = r.h.DBScan(context.Background(), "SELECT COALESCE(error, '') FROM repository_checkpoints WHERE kind = 'UNIT' AND execution_id = '"+r.execID.String()+"'", &cpErr)
		if !strings.Contains(cpErr, "policy: commit rejected") {
			t.Fatalf("checkpoint error %q", cpErr)
		}
	})
	t.Run("BEST_EFFORT", func(t *testing.T) {
		r := p16NewRigWith(t, p16Options{config: func(c *config.Config) { c.Engine.RunGitHooks = true },
			engine: func(s *execution.EngineSettings, _ *hardening.Settings) {
				s.CheckpointPolicy = execution.CheckpointBestEffort
			}})
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		p18Hook(t, r, "pre-commit", "exit 1")
		r.runUntil(t, execution.StepCompleted, 100)
		if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE kind = 'UNIT' AND status = 'FAILED' AND execution_id = '"+r.execID.String()+"'"); n != 1 {
			t.Fatalf("failed checkpoint records %d", n)
		}
	})
	t.Run("hooks off", func(t *testing.T) {
		r := p16NewRig(t)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		p18Hook(t, r, "pre-commit", "exit 1")
		r.runUntil(t, execution.StepCompleted, 100)
	})
}
