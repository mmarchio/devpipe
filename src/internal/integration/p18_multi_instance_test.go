package integration

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/hardening"
)

// T18.166–T18.168 — three instances (distinct identities and ports) share
// one database with their schedulers on: one driver at a time. The
// instance holding the lease freezes mid-step (as if killed — it never
// releases); another takes over after the TTL with a higher epoch and
// completes the project; when the frozen instance resumes it is fenced
// and changes nothing. Each unit request is delivered exactly once.
func TestT18166ThreeInstancesOneDriver(t *testing.T) {
	base15, boot := p15RestartRig(t, []p15Phase{{1, "Foundation", "PENDING", 1}}, nil)
	a := &p16Rig{p15Rig: base15}
	p16Configure(t, a, p16Options{engine: func(s *execution.EngineSettings, _ *hardening.Settings) { s.DriverLease = time.Second }})
	sched := func(c *config.Config) {
		c.Engine.SchedulerDisabled = false
		c.Engine.Interval = 30 * time.Millisecond
	}
	b, c := boot(sched), boot(sched)
	ids := map[string]bool{a.h.App.InstanceID(): true, b.App.InstanceID(): true, c.App.InstanceID(): true}
	urls := map[string]bool{a.h.BaseURL: true, b.BaseURL: true, c.BaseURL: true}
	if len(ids) != 3 || len(urls) != 3 {
		t.Fatalf("instances not distinct: %v %v", ids, urls)
	}
	a.queuePlan(a.planJSON(p16Unit{key: "x"}, p16Unit{key: "y", deps: []string{"x"}}))
	a.queue("complete")
	a.queue("complete")
	ctx := context.Background()
	epoch := func() int64 {
		var e int64
		_ = a.h.DBScan(ctx, "SELECT driver_epoch FROM executions WHERE id = '"+a.execID.String()+"'", &e)
		return e
	}

	var once sync.Once
	frozen, release := make(chan struct{}), make(chan struct{})
	var frozenEpoch int64
	freeze := func(_ context.Context, act execution.StepAction, stage string) error {
		if act == execution.ActionDispatch && stage == "dispatch" {
			hit := false
			once.Do(func() { hit = true })
			if hit {
				frozenEpoch = epoch()
				close(frozen)
				<-release
			}
		}
		return nil
	}
	b.App.Engine().SetStepHook(freeze)
	c.App.Engine().SetStepHook(freeze)
	defer b.App.Engine().SetStepHook(nil)
	defer c.App.Engine().SetStepHook(nil)
	a.exec(t, "UPDATE project_settings SET auto_continue = true WHERE project_id = '"+a.pid.String()+"'")
	b.App.Engine().Kick()
	c.App.Engine().Kick()
	select {
	case <-frozen:
	case <-time.After(30 * time.Second):
		t.Fatal("no instance reached the dispatch")
	}
	sendsAtFreeze := len(a.g.Messages())
	deadline := time.Now().Add(60 * time.Second)
	for a.execStatus(t) != "COMPLETED" {
		if s := a.execStatus(t); s == "RECOVERY_REQUIRED" || s == "FAILED" {
			close(release)
			t.Fatalf("execution %s (%s)", s, a.recoveryReason(t))
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatalf("no takeover: %s", a.execStatus(t))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if e := epoch(); e <= frozenEpoch {
		t.Fatalf("takeover without a higher epoch: %d → %d", frozenEpoch, e)
	}
	fp := p18Fingerprint(t, a)
	sends := len(a.g.Messages())
	close(release)
	time.Sleep(500 * time.Millisecond)
	if len(a.g.Messages()) != sends || p18Fingerprint(t, a) != fp {
		t.Fatal("the fenced instance acted after resuming")
	}
	if sends <= sendsAtFreeze {
		t.Fatal("setup: the takeover sent nothing")
	}
	if n := a.count(t, `SELECT COALESCE(max(n), 0) FROM (SELECT count(*) AS n FROM worker_dispatches WHERE project_id = '`+a.pid.String()+`'
		AND work_unit_id IS NOT NULL AND delivery IS DISTINCT FROM 'DEFINITELY_NOT_SENT' GROUP BY work_unit_id) x`); n != 1 {
		t.Fatalf("a unit request was delivered %d times", n)
	}
}

// T18.170 / T18.228 — the same recovery action submitted to two instances
// at once executes once: with one idempotency key both callers get the
// one recorded result; with different keys exactly one succeeds.
func TestT18170RecoveryActionOnceAcrossInstances(t *testing.T) {
	base15, boot := p15RestartRig(t, []p15Phase{{1, "Foundation", "PENDING", 1}}, p17Config(nil))
	a := &p16Rig{p15Rig: base15}
	p16Configure(t, a, p16Options{})
	b := boot(nil)
	a.queuePlan(a.planJSON(p16Unit{key: "x"}))
	a.queue("complete")
	away := a.dir + ".away"
	p18BreakRepoAt(t, a, execution.ActionDispatch, func() {
		if err := os.Rename(a.dir, away); err != nil {
			t.Fatal(err)
		}
	}, "REPOSITORY_MISSING", 1)
	if err := os.Rename(away, a.dir); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/" + a.pid.String() + "/executions/" + a.execID.String() + "/recovery/actions"
	browsers := []*browser{newBrowser(t, a.h.BaseURL), newBrowser(t, b.BaseURL)}
	post := func(key string) []int {
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i, br := range browsers {
			br.page("/")
			wg.Add(1)
			go func(i int, br *browser) {
				defer wg.Done()
				codes[i], _ = br.api(http.MethodPost, path, map[string]any{"action": "MANUAL_RESUME", "reason": "repository restored",
					"confirm": true, "idempotency_key": key + "-" + map[bool]string{true: "shared", false: itoa(i)}[key == "same"]})
			}(i, br)
		}
		wg.Wait()
		return codes
	}
	// Concurrently: one executes, the other is told the same action is in
	// progress (or gets the recorded result); afterwards both replay it.
	codes := post("same")
	if (codes[0] != http.StatusOK && codes[1] != http.StatusOK) || (codes[0] != http.StatusOK && codes[0] != http.StatusConflict) ||
		(codes[1] != http.StatusOK && codes[1] != http.StatusConflict) {
		t.Fatalf("same key: %v", codes)
	}
	if codes = post("same"); codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("same key replay: %v", codes)
	}
	if n := a.count(t, "SELECT count(*) FROM recovery_actions WHERE execution_id = '"+a.execID.String()+"' AND idempotency_key = 'same-shared'"); n != 1 {
		t.Fatalf("%d action rows for one idempotency key", n)
	}
	// Back into recovery, then two different keys at once.
	p18BreakRepoAt(t, a, execution.ActionDispatch, func() {
		if err := os.Rename(a.dir, away); err != nil {
			t.Fatal(err)
		}
	}, "REPOSITORY_MISSING", 1)
	if err := os.Rename(away, a.dir); err != nil {
		t.Fatal(err)
	}
	codes = post("different")
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("different keys: %v (want exactly one success)", codes)
	}
	if n := a.count(t, "SELECT count(*) FROM recovery_actions WHERE execution_id = '"+a.execID.String()+"' AND idempotency_key LIKE 'different-%' AND status = 'SUCCEEDED'"); n != 1 {
		t.Fatalf("%d succeeded actions for one recovery", n)
	}
}
