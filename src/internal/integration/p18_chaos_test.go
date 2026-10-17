package integration

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/faults"
	"overseer/internal/gateway/hermes/hermestest"
)

// p18Operator drives a chaos run to the end as an operator would: the
// engine runs; whenever the execution stops in recovery, fix (if any)
// repairs the environment for the condition and the operator performs
// the preferred action the recovery plan allows, through the audited API.
// It returns the number of reviewed resends the operator approved.
func p18Operator(t *testing.T, r *p17Rig, fix func(cond string), maxOps int) (resends int) {
	t.Helper()
	ctx := context.Background()
	base := "/api/v1/projects/" + r.pid.String() + "/executions/" + r.execID.String() + "/recovery"
	ops := 0
	for i := 0; i < 400; i++ {
		runCtx := ctx
		if r.ctx != nil {
			runCtx = r.ctx
		}
		res, err := r.eng().Run(runCtx, r.execID, 50)
		if err != nil {
			if !strings.Contains(err.Error(), "busy") {
				t.Logf("run: %v", err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if len(res) > 0 && res[len(res)-1].Outcome == execution.StepCompleted || r.execStatus(t) == "COMPLETED" {
			return resends
		}
		switch r.execStatus(t) {
		case "WAITING":
			time.Sleep(50 * time.Millisecond)
			continue
		case "RECOVERY_REQUIRED":
		default:
			continue
		}
		if ops++; ops > maxOps {
			t.Fatalf("more than %d operator interventions (last: %s / %s)", maxOps, p18RecoveryCondition(t, r.p16Rig), r.recoveryReason(t))
		}
		cond := p18RecoveryCondition(t, r.p16Rig)
		if fix != nil {
			fix(cond)
		}
		var plans struct {
			Current *struct {
				SafeActions []string `json:"safe_actions"`
			} `json:"current"`
			CurrentHash string `json:"current_hash"`
		}
		if code := r.b.getJSON(base+"/plan", &plans); code != http.StatusOK || plans.Current == nil {
			t.Fatalf("recovery plan: %d", code)
		}
		safe := map[string]bool{}
		for _, a := range plans.Current.SafeActions {
			safe[a] = true
		}
		body := map[string]any{"reason": "chaos operator: " + cond, "confirm": true, "evidence_hash": plans.CurrentHash,
			"idempotency_key": "chaos-" + r.execID.String() + "-" + itoa(ops)}
		switch {
		case safe["RESOLVE_DELIVERY"]:
			body["action"], body["resolution"] = "RESOLVE_DELIVERY", "RESEND_AFTER_REVIEW"
			resends++
		case safe["RETRY_CHECKPOINT"]:
			body["action"] = "RETRY_CHECKPOINT"
		case safe["ACCEPT_REPOSITORY_HEAD"]:
			body["action"] = "ACCEPT_REPOSITORY_HEAD"
		case safe["RUN_VALIDATION"]:
			body["action"] = "RUN_VALIDATION"
		case safe["RESUME"]:
			body["action"] = "MANUAL_RESUME"
		default:
			body["action"] = "MANUAL_RESUME"
		}
		// A 5xx (e.g. the database was cut mid-request) is retried with the
		// same idempotency key, as an operator would.
		code, raw := r.b.api(http.MethodPost, base+"/actions", body)
		for try := 0; code >= 500 && try < 10; try++ {
			time.Sleep(200 * time.Millisecond)
			code, raw = r.b.api(http.MethodPost, base+"/actions", body)
		}
		if code == http.StatusConflict && r.execStatus(t) != "RECOVERY_REQUIRED" {
			continue // the retried action had already been applied
		}
		if code != http.StatusOK {
			var ev string
			_ = r.h.DBScan(ctx, "SELECT recovery_evidence::text || ' / ' || recovery_reason FROM executions WHERE id = '"+r.execID.String()+"'", &ev)
			rows, _ := r.h.App.DB().Raw().Query(ctx, "SELECT id::text || ' ' || attempt || ' ' || status || ' ' || error_code FROM worker_dispatches WHERE project_id = $1 ORDER BY created_at", r.pid)
			for rows.Next() {
				var x string
				_ = rows.Scan(&x)
				t.Log(x)
			}
			rows.Close()
			t.Log(ev)
			t.Fatalf("operator %s on %s: %d %s", body["action"], cond, code, raw)
		}
	}
	t.Fatalf("chaos run did not finish: %s (%s)", r.execStatus(t), r.recoveryReason(t))
	return resends
}

// p18ChaosInvariants — after chaos: the invariant scan is clean, no unit
// is DONE without exactly one checkpoint, no checkpoint was committed
// twice, no unit request was delivered more often than the one send plus
// the reviewed resends, and the project completed once.
func p18ChaosInvariants(t *testing.T, r *p17Rig, resends int) {
	t.Helper()
	rep, err := r.h.App.Integrity().Scan(context.Background(), nil, nil)
	if err != nil || !rep.OK() {
		t.Fatalf("invariant scan after chaos: %v %v", err, p18Violations(rep))
	}
	pid := r.pid.String()
	if r.execStatus(t) != "COMPLETED" || r.count(t, "SELECT count(*) FROM execution_completions WHERE project_id = '"+pid+"'") != 1 {
		t.Fatalf("execution %s", r.execStatus(t))
	}
	done := r.count(t, "SELECT count(*) FROM work_units WHERE project_id = '"+pid+"' AND status = 'DONE'")
	if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE project_id = '"+pid+"' AND kind = 'UNIT' AND status = 'CREATED'"); n != done {
		t.Fatalf("%d unit checkpoints for %d DONE units", n, done)
	}
	if n := r.count(t, `SELECT COALESCE(max(n), 0) FROM (SELECT count(*) AS n FROM worker_dispatches WHERE project_id = '`+pid+`'
		AND work_unit_id IS NOT NULL AND status = 'COMPLETED' GROUP BY work_unit_id) x`); n > 1+resends {
		t.Fatalf("a unit's request completed %d times with %d reviewed resends", n, resends)
	}
	c := exec.Command("git", "log", "--all", "--format=%B")
	c.Dir = r.dir
	b, _ := c.Output()
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "Overseer-Checkpoint:") {
			if seen[l] {
				t.Fatalf("checkpoint committed twice: %s", l)
			}
			seen[l] = true
		}
	}
}

// T18.152 — provider chaos (seeded): 500 errors, disconnects after
// receipt, malformed replies, wrong correlation ids and slow replies are
// interleaved with the real answers; the project still completes without
// duplicated work and with a clean invariant scan.
func TestT18152ProviderChaos(t *testing.T) {
	for _, seed := range []int64{3, 11, 29} {
		seed := seed
		t.Run("seed"+itoa(int(seed)), func(t *testing.T) {
			r := p17NewRig(t)
			rng := rand.New(rand.NewSource(seed))
			bad := []hermestest.Behavior{
				{Status: 500, ErrorCode: "internal", Message: "upstream exploded"},
				{Drop: true},
				{Raw: `{"not": "a reply"`},
				{NoChoices: true},
			}
			plan := p16Reply("complete", "Planned the phase.", `"plan":`+r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
			unit := p16Reply("complete", "Worker reports complete.", "")
			for _, good := range []string{plan, unit, unit} {
				for k := rng.Intn(3); k > 0; k-- {
					r.g.Script(bad[rng.Intn(len(bad))])
				}
				g := good
				b := hermestest.Behavior{Content: &g, Usage: p14Usage(1000)}
				if rng.Intn(3) == 0 {
					b.Delay = 200 * time.Millisecond
				}
				r.g.Script(b)
			}
			for i := 0; i < 6; i++ { // replies for reviewed resends / repairs
				r.queue("complete")
			}
			resends := p18Operator(t, r, nil, 15)
			p18ChaosInvariants(t, r, resends)
			raw, _ := json.Marshal(map[string]int{"requests": len(r.g.Messages()), "resends": resends})
			t.Logf("seed %d: %s", seed, raw)
		})
	}
}

// T18.153 — database chaos (seeded): latency, connection cuts and
// injected serialization failures / deadlocks (40001 / 40P01) while the
// project runs; nothing is lost or duplicated and the scan is clean.
func TestT18153DatabaseChaos(t *testing.T) {
	for _, seed := range []int64{5, 17} {
		seed := seed
		t.Run("seed"+itoa(int(seed)), func(t *testing.T) {
			px := newPGProxy(t)
			base := p16NewRigWith(t, p16Options{config: p17Config(func(c *config.Config) { px.configure(c) })})
			r := &p17Rig{p16Rig: base, b: newBrowser(t, base.h.BaseURL)}
			// Slow worker replies stretch the run over the chaos.
			plan := p16Reply("complete", "Planned the phase.", `"plan":`+r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
			r.g.Script(hermestest.Behavior{Content: &plan, Delay: 400 * time.Millisecond})
			for i := 0; i < 2; i++ {
				c := p16Reply("complete", "Worker reports complete.", "")
				r.g.Script(hermestest.Behavior{Content: &c, Usage: p14Usage(1000), Delay: 400 * time.Millisecond})
			}
			for i := 0; i < 6; i++ { // replies for reviewed resends / repairs
				r.queue("complete")
			}
			rng := rand.New(rand.NewSource(seed))
			reg := faults.NewRegistry()
			for i := 0; i < 4; i++ {
				code := "40001"
				if rng.Intn(2) == 0 {
					code = "40P01"
				}
				reg.Arm(faults.Action{Point: faults.BeforeDBCommit, Nth: 1 + rng.Intn(60), Times: 1, Kind: faults.KindError,
					Err: &pgconn.PgError{Code: code, Message: "injected"}})
			}
			cuts := 0
			stop := make(chan struct{})
			chaosDone := make(chan struct{})
			go func() {
				defer close(chaosDone)
				for {
					select {
					case <-stop:
						px.SetLatency(0)
						px.Restore()
						return
					case <-time.After(time.Duration(50+rng.Intn(150)) * time.Millisecond):
					}
					switch rng.Intn(4) {
					case 0:
						cuts++
						px.Cut()
						time.Sleep(time.Duration(50+rng.Intn(200)) * time.Millisecond)
						px.Restore()
					case 1:
						px.SetLatency(time.Duration(rng.Intn(20)) * time.Millisecond)
					default:
						px.SetLatency(0)
					}
				}
			}()
			ctx, done := faults.WithRegistry(context.Background(), reg)
			defer done()
			r.ctx = ctx
			resends := p18Operator(t, r, nil, 15)
			close(stop)
			<-chaosDone
			r.ctx = nil
			if cuts == 0 || reg.Hits(faults.BeforeDBCommit) < 2 {
				t.Fatalf("no chaos happened: %d cuts, %d commits", cuts, reg.Hits(faults.BeforeDBCommit))
			}
			t.Logf("seed %d: %d cuts, %d commits under injection", seed, cuts, reg.Hits(faults.BeforeDBCommit))
			p18ChaosInvariants(t, r, resends)
		})
	}
}

// T18.154 — filesystem chaos (seeded): the repository disappears, is
// swapped for a symlink to an unrelated repository, refuses writes
// (EACCES), runs out of space (ENOSPC) and collects unexpected files
// while the project runs. Each stop is a classified recovery; after the
// operator repairs the environment the project completes without
// duplicated work.
func TestT18154FilesystemChaos(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 4, 5} {
		seed := seed
		t.Run("seed"+itoa(int(seed)), func(t *testing.T) {
			r := p17NewRig(t)
			r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
			r.queue("complete")
			r.queue("complete")
			for i := 0; i < 4; i++ {
				r.queue("complete")
			}
			rng := rand.New(rand.NewSource(seed))
			reg := faults.NewRegistry()
			away := r.dir + ".away"
			other := p15Repo(t)
			kinds := []string{"missing", "symlink", "eacces", "enospc", "junk"}
			// Every kind is the first event of one seed.
			at := map[int]string{2 + rng.Intn(6): kinds[(seed-1)%int64(len(kinds))]}
			for len(at) < 3 {
				at[2+rng.Intn(14)] = kinds[rng.Intn(len(kinds))]
			}
			intents, applied := 0, []string{}
			r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
				if s != "intent" {
					return nil
				}
				intents++
				kind, ok := at[intents]
				if !ok {
					return nil
				}
				applied = append(applied, kind)
				switch kind {
				case "missing":
					_ = os.Rename(r.dir, away)
				case "symlink":
					if os.Rename(r.dir, away) == nil {
						_ = os.Symlink(other, r.dir)
					}
				case "eacces":
					reg.Arm(faults.Action{Point: faults.FSWrite, Times: 1, Kind: faults.KindError, Err: syscall.EACCES})
				case "enospc":
					reg.Arm(faults.Action{Point: faults.FSWrite, Times: 1, Kind: faults.KindError, Err: syscall.ENOSPC})
				case "junk":
					_ = os.WriteFile(filepath.Join(r.dir, "unexpected-"+itoa(intents)+".tmp"), []byte("junk"), 0o644)
				}
				return nil
			})
			defer r.eng().SetStepHook(nil)
			fix := func(string) {
				if _, err := os.Lstat(away); err == nil {
					_ = os.Remove(r.dir) // the symlink, if any
					_ = os.Rename(away, r.dir)
				}
			}
			ctx, done := faults.WithRegistry(context.Background(), reg)
			defer done()
			r.ctx = ctx
			resends := p18Operator(t, r, fix, 15)
			r.ctx = nil
			t.Logf("seed %d: applied %v", seed, applied)
			if len(applied) == 0 {
				t.Fatal("no filesystem chaos was applied")
			}
			p18ChaosInvariants(t, r, resends)
		})
	}
}

// T18.155 — git chaos (seeded): a held index lock (released later by
// "another git process"), a failing commit, a branch switch, an external
// commit and a rejecting hook (hooks enabled) hit the run; the stops are
// classified, the operator repairs / accepts, and the project completes
// with every checkpoint committed once.
func TestT18155GitChaos(t *testing.T) {
	kinds := []string{"lock", "commitfail", "branch", "extcommit", "hook"}
	for _, seed := range []int64{1, 2, 3, 4, 5} {
		seed := seed
		t.Run("seed"+itoa(int(seed)), func(t *testing.T) {
			r := p17NewRigWith(t, p16Options{config: func(c *config.Config) { c.Engine.RunGitHooks = true }})
			r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
			for i := 0; i < 6; i++ {
				r.queue("complete")
			}
			rng := rand.New(rand.NewSource(seed))
			reg := faults.NewRegistry()
			git := func(args ...string) { p15Git(t, r.dir, args...) }
			hook := filepath.Join(r.dir, ".git", "hooks", "pre-commit")
			lock := filepath.Join(r.dir, ".git", "index.lock")
			at := map[int]string{3 + rng.Intn(5): kinds[(seed-1)%int64(len(kinds))]}
			for len(at) < 3 {
				at[2+rng.Intn(14)] = kinds[rng.Intn(len(kinds))]
			}
			intents, applied := 0, []string{}
			r.eng().SetStepHook(func(_ context.Context, a execution.StepAction, s string) error {
				if s != "intent" {
					return nil
				}
				intents++
				kind, ok := at[intents]
				if !ok {
					return nil
				}
				applied = append(applied, kind)
				switch kind {
				case "lock":
					_ = os.WriteFile(lock, nil, 0o644)
					time.AfterFunc(300*time.Millisecond, func() { _ = os.Remove(lock) })
				case "commitfail":
					reg.Arm(faults.Action{Point: faults.BeforeGitCommit, Times: 1, Kind: faults.KindError})
				case "branch":
					git("checkout", "-q", "-b", "chaos-"+itoa(intents))
				case "extcommit":
					_ = os.WriteFile(filepath.Join(r.dir, "human-"+itoa(intents)+".txt"), []byte("by hand"), 0o644)
					git("add", "-A")
					git("commit", "-q", "-m", "a human commit")
				case "hook":
					_ = os.WriteFile(hook, []byte("#!/bin/sh\necho rejected by policy >&2\nexit 1\n"), 0o755)
				}
				return nil
			})
			defer r.eng().SetStepHook(nil)
			fix := func(cond string) {
				_ = os.Remove(hook)
				if out, _ := exec.Command("git", "-C", r.dir, "rev-parse", "--abbrev-ref", "HEAD").Output(); strings.TrimSpace(string(out)) != "main" {
					git("checkout", "-q", "main")
				}
			}
			ctx, done := faults.WithRegistry(context.Background(), reg)
			defer done()
			r.ctx = ctx
			resends := p18Operator(t, r, fix, 15)
			r.ctx = nil
			t.Logf("seed %d: applied %v", seed, applied)
			p18ChaosInvariants(t, r, resends)
		})
	}
}
