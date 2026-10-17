package integration

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/recovery"
)

// p18RecoveryVia runs r with a scoped registry; arm is called the first
// time the engine reaches (action, "intent").
func p18RecoveryVia(t *testing.T, r *p16Rig, action execution.StepAction, arm func(reg *faults.Registry)) {
	t.Helper()
	reg := faults.NewRegistry()
	ctx, done := faults.WithRegistry(context.Background(), reg)
	defer done()
	armed := false
	r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
		if a == action && s == "intent" && !armed {
			armed = true
			arm(reg)
		}
		return nil
	})
	defer r.eng().SetStepHook(nil)
	for i := 0; i < 5 && r.execStatus(t) != "RECOVERY_REQUIRED"; i++ {
		_, _ = r.eng().Run(ctx, r.execID, 50)
	}
}

// p18RecoveryGenerators produce one execution stopped in each supported
// recovery condition.
func p18RecoveryGenerators() map[recovery.Condition]func(t *testing.T) *p16Rig {
	unit := func(t *testing.T, o p16Options) *p16Rig {
		r := p16NewRigWith(t, o)
		r.queuePlan(r.planJSON(p16Unit{key: "a"}))
		r.queue("complete")
		return r
	}
	fsFault := func(errno syscall.Errno) func(t *testing.T) *p16Rig {
		return func(t *testing.T) *p16Rig {
			r := unit(t, p16Options{})
			p18RecoveryVia(t, r, execution.ActionDispatch, func(reg *faults.Registry) {
				reg.Arm(faults.Action{Point: faults.FSWrite, Times: 1, Kind: faults.KindError, Err: errno})
			})
			return r
		}
	}
	return map[recovery.Condition]func(t *testing.T) *p16Rig{
		recovery.DeliveryUnknown: func(t *testing.T) *p16Rig {
			r := unit(t, p16Options{})
			p18RecoveryVia(t, r, execution.ActionDispatch, func(reg *faults.Registry) {
				reg.Arm(faults.Action{Point: faults.AfterWorkerSend, Times: 1, Kind: faults.KindError})
			})
			return r
		},
		recovery.RepositoryMissing: func(t *testing.T) *p16Rig {
			r := unit(t, p16Options{})
			p18BreakRepoAt(t, r, execution.ActionDispatch, func() { _ = os.RemoveAll(r.dir) }, "REPOSITORY_MISSING", 1)
			return r
		},
		recovery.RepositoryIdentityMismatch: func(t *testing.T) *p16Rig {
			r := unit(t, p16Options{})
			other := p15Repo(t)
			p18BreakRepoAt(t, r, execution.ActionDispatch, func() { _ = os.RemoveAll(r.dir); _ = os.Symlink(other, r.dir) },
				"REPOSITORY_IDENTITY_MISMATCH", 1)
			return r
		},
		recovery.RepositoryHeadChanged: func(t *testing.T) *p16Rig {
			r := unit(t, p16Options{})
			p18BreakRepoAt(t, r, execution.ActionDispatch, func() { p18ExternalCommit(t, r.dir, "HUMAN.md") }, "REPOSITORY_HEAD_CHANGED", 1)
			return r
		},
		recovery.RepositoryPermission: fsFault(syscall.EACCES),
		recovery.DiskFull:             fsFault(syscall.ENOSPC),
		recovery.CheckpointRejected: func(t *testing.T) *p16Rig {
			r := unit(t, p16Options{config: func(c *config.Config) { c.Engine.RunGitHooks = true }})
			_ = os.WriteFile(filepath.Join(r.dir, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
			_, _ = r.eng().Run(context.Background(), r.execID, 100)
			return r
		},
		recovery.CheckpointAmbiguous: func(t *testing.T) *p16Rig {
			r, restart := p18CrashAtCheckpoint(t, "checkpoint-committed")
			msg := p15Git(t, r.dir, "log", "-1", "--format=%B")
			p15Git(t, r.dir, "commit", "-q", "--allow-empty", "-m", msg)
			restart()
			_, _ = r.eng().Run(context.Background(), r.execID, 100)
			return r
		},
		recovery.ProtocolViolation: func(t *testing.T) *p16Rig {
			r := p16NewRig(t)
			r.queuePlan(r.planJSON(p16Unit{key: "a"}))
			for i := 0; i < 4; i++ {
				r.queueRaw("this is not the protocol", 1000)
			}
			_, _ = r.eng().Run(context.Background(), r.execID, 100)
			return r
		},
		recovery.ManualCriteria: func(t *testing.T) *p16Rig {
			r := p16NewRig(t)
			r.queuePlan(r.planJSON(p16Unit{key: "a", manual: true}))
			r.queue("complete")
			_, _ = r.eng().Run(context.Background(), r.execID, 100)
			return r
		},
		recovery.InvariantViolation: func(t *testing.T) *p16Rig {
			r := unit(t, p16Options{})
			p18BreakRepoAt(t, r, execution.ActionDispatch, func() { p18SecondExecutingPhase(t, r) }, "INVARIANT_VIOLATION", 1)
			return r
		},
		recovery.Panic: func(t *testing.T) *p16Rig {
			r := unit(t, p16Options{})
			fired := false
			r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
				if a == execution.ActionDispatch && s == "intent" && !fired {
					fired = true
					panic("injected")
				}
				return nil
			})
			_, _ = r.eng().Run(context.Background(), r.execID, 50)
			r.eng().SetStepHook(nil)
			return r
		},
		recovery.PromptTooLarge: func(t *testing.T) *p16Rig {
			r := p16NewRigWith(t, p16Options{config: p18CompressionCircuit(2, time.Hour)})
			p18ForceCompression(t, r)
			r.srv.Script(providertest.Reply{Status: 503, Body: `{"error":{"message":"overloaded"}}`})
			r.exec(t, "UPDATE project_settings SET context_window = 200 WHERE project_id = '"+r.pid.String()+"'")
			r.queuePlan(r.planJSON(p16Unit{key: "a"}))
			_, _ = r.eng().Run(context.Background(), r.execID, 50)
			return r
		},
		recovery.ValidationInfra: func(t *testing.T) *p16Rig {
			r := unit(t, p16Options{})
			p18RecoveryVia(t, r, execution.ActionRunUnitValidation, func(*faults.Registry) {
				_ = os.Chmod(r.script, 0o644) // the validation tool cannot run
			})
			return r
		},
	}
}

// T18.229 — recovery acceptance: every operator-facing recovery condition
// is generated in a real run and then: detected (RECOVERY_REQUIRED with
// the condition), carries evidence, offers exactly the safe actions of the
// code plan and none of its unsafe ones, is deterministic (same plan hash
// on repeated reads), and an operator action on it is audited and leaves
// the execution resumed or explicitly blocked. (Automatically handled
// conditions — RESUMABLE, EXPIRED_LEASE, INFLIGHT_DISPATCH,
// RUNNING_VALIDATION, DISPATCH_NOT_SENT, ORPHAN_PRECOMPUTE — are
// classified by TestT186StartupRecoveryScan; PROVIDER_UNAVAILABLE waits
// (TestT18214); RESPONSE_UNPERSISTED is produced by the crash matrix.)
func TestT18229RecoveryAcceptance(t *testing.T) {
	gens := p18RecoveryGenerators()
	conds := make([]string, 0, len(gens))
	for c := range gens {
		conds = append(conds, string(c))
	}
	sort.Strings(conds)
	for _, cs := range conds {
		cond := recovery.Condition(cs)
		gen := gens[cond]
		t.Run(cs, func(t *testing.T) {
			r := gen(t)
			if r.execStatus(t) != "RECOVERY_REQUIRED" || p18RecoveryCondition(t, r) != cs {
				t.Fatalf("generated %s / %q (%s)", r.execStatus(t), p18RecoveryCondition(t, r), r.recoveryReason(t))
			}
			b := newBrowser(t, r.h.BaseURL)
			base := "/api/v1/projects/" + r.pid.String() + "/executions/" + r.execID.String() + "/recovery"
			type planView struct {
				Current *struct {
					Evidence      map[string]any `json:"evidence"`
					SafeActions   []string       `json:"safe_actions"`
					UnsafeActions []string       `json:"unsafe_actions"`
				} `json:"current"`
				CurrentHash string `json:"current_hash"`
			}
			var p1, p2 planView
			if b.getJSON(base+"/plan", &p1) != http.StatusOK || p1.Current == nil || b.getJSON(base+"/plan", &p2) != http.StatusOK {
				t.Fatal("no recovery plan")
			}
			if p1.CurrentHash == "" || p1.CurrentHash != p2.CurrentHash {
				t.Fatalf("plan not deterministic: %q vs %q", p1.CurrentHash, p2.CurrentHash)
			}
			if len(p1.Current.Evidence) < 2 {
				t.Fatalf("no evidence: %v", p1.Current.Evidence)
			}
			want := recovery.PlanFor(recovery.Facts{Condition: cond})
			if !p18SameActions(p1.Current.SafeActions, want.SafeActions) || !p18SameActions(p1.Current.UnsafeActions, want.UnsafeActions) {
				t.Fatalf("plan actions safe %v unsafe %v, code says %v / %v", p1.Current.SafeActions, p1.Current.UnsafeActions, want.SafeActions, want.UnsafeActions)
			}
			var view struct {
				Actions []struct {
					Kind string `json:"kind"`
				} `json:"actions"`
			}
			if b.getJSON(base, &view) != http.StatusOK {
				t.Fatal("recovery view")
			}
			offered := map[string]bool{}
			for _, a := range view.Actions {
				offered[a.Kind] = true
			}
			for _, u := range want.UnsafeActions {
				if offered[string(u)] {
					t.Fatalf("unsafe action %s offered", u)
				}
			}
			// The operator acts; the action is audited.
			body := map[string]any{"reason": "acceptance operator", "confirm": true, "evidence_hash": p1.CurrentHash,
				"idempotency_key": "acceptance-" + cs}
			switch {
			case offered["RESOLVE_DELIVERY"]:
				body["action"], body["resolution"] = "RESOLVE_DELIVERY", "ACCEPT_REPOSITORY_STATE"
			case offered["APPROVE_CRITERIA"]:
				body["action"] = "APPROVE_CRITERIA"
			case offered["RETRY_CHECKPOINT"]:
				body["action"] = "RETRY_CHECKPOINT"
			case offered["ACCEPT_REPOSITORY_HEAD"]:
				body["action"] = "ACCEPT_REPOSITORY_HEAD"
			case offered["RUN_VALIDATION"]:
				body["action"] = "RUN_VALIDATION"
			default:
				body["action"] = "MANUAL_RESUME"
			}
			if code, raw := b.api(http.MethodPost, base+"/actions", body); code != http.StatusOK {
				t.Fatalf("operator %s: %d %s", body["action"], code, raw)
			}
			if n := r.count(t, "SELECT count(*) FROM audit_events WHERE action = 'RECOVERY_ACTION' AND reason = 'acceptance operator' AND result = 'SUCCEEDED'"); n != 1 {
				t.Fatalf("recovery action audited %d times", n)
			}
			if n := r.count(t, "SELECT count(*) FROM recovery_actions WHERE idempotency_key = 'acceptance-"+cs+"' AND status = 'SUCCEEDED'"); n != 1 {
				t.Fatalf("%d recorded actions", n)
			}
			_, _ = r.eng().Run(context.Background(), r.execID, 20)
			switch s := r.execStatus(t); s {
			case "RUNNING", "WAITING", "COMPLETED":
			case "RECOVERY_REQUIRED":
				if r.recoveryReason(t) == "" {
					t.Fatal("blocked without an explicit reason")
				}
			default:
				t.Fatalf("after the action: %s", s)
			}
		})
	}
}

func p18SameActions(got []string, want []recovery.Action) bool {
	if len(got) != len(want) {
		return false
	}
	m := map[string]bool{}
	for _, g := range got {
		m[g] = true
	}
	for _, w := range want {
		if !m[string(w)] {
			return false
		}
	}
	return true
}
