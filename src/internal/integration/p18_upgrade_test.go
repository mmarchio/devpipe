package integration

import (
	"context"
	"net/http"
	"testing"

	"overseer/internal/audit"
	"overseer/internal/config"
	"overseer/internal/dbrepo"
	"overseer/internal/execution"
)

// p18Retarget returns a copy of r driving the same project through h.
func p18Retarget(r *p16Rig, h *Harness) *p16Rig {
	base := *r.p15Rig
	base.h = h
	cp := *r
	cp.p15Rig = &base
	return &cp
}

// T18.131 / T18.135 / T18.136 — history written by schema 106 (a
// completed, a paused and a running project) survives the upgrade to the
// latest schema: the invariant scan is clean, every pre-existing row is
// unchanged, the console renders; the paused project resumes and the
// running one is reconciled by the new instance — both complete without
// duplicated work.
func TestT18131UpgradeFrom106(t *testing.T) { p18UpgradeScenario(t, 106) }

// T18.227 — upgrade acceptance from a schema that already had the audit
// log but no hash chain (119): the large audit history is linked at
// startup and verifies VALID; the prompt cache survives.
func TestT18227UpgradeFrom119WithAudit(t *testing.T) { p18UpgradeScenario(t, 119) }

func p18UpgradeScenario(t *testing.T, from int) {
	ctx := context.Background()
	done := p17NewRig(t)
	done.queuePlan(done.planJSON(p16Unit{key: "a"}))
	done.queue("complete")
	done.runUntil(t, execution.StepCompleted, 200)

	mk := func() *p16Rig {
		r := &p16Rig{p15Rig: p15Build(t, done.h, done.g, done.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
		p16Configure(t, r, p16Options{})
		return r
	}
	paused, running := mk(), mk()
	paused.queuePlan(paused.planJSON(p16Unit{key: "p"}))
	if _, err := paused.eng().Run(ctx, paused.execID, 2); err != nil {
		t.Fatal(err)
	}
	if code, body := done.b.api(http.MethodPost, "/api/v1/projects/"+paused.pid.String()+"/pause", nil); code != http.StatusOK {
		t.Fatalf("pause: %d %s", code, body)
	}
	running.queuePlan(running.planJSON(p16Unit{key: "x"}, p16Unit{key: "y", deps: []string{"x"}}))
	running.queue("complete")
	for i := 0; i < 40 && running.count(t, "SELECT count(*) FROM work_units WHERE project_id = '"+running.pid.String()+"' AND status = 'DONE'") == 0; i++ {
		if _, err := running.eng().Step(ctx, running.execID); err != nil {
			t.Fatal(err)
		}
	}
	if running.execStatus(t) != "RUNNING" {
		t.Fatalf("setup: running project %s", running.execStatus(t))
	}

	// A large audit history (18.227).
	arepo := dbrepo.NewAuditRepository(done.h.App.DB().Raw())
	for i := 0; i < 1500; i++ {
		pid := done.pid
		if i%3 == 0 {
			pid = paused.pid
		}
		if err := arepo.AppendAudit(ctx, nil, p18AuditEvent(&pid, i)); err != nil {
			t.Fatal(err)
		}
	}
	cacheEntries := done.count(t, "SELECT count(*) FROM prompt_cache_entries")

	// The "old installation": schema 106 holding that history.
	dst, name := p18OldSchemaDB(t, from)
	copied, skipped := p18ProjectInto(t, done.h.App.DB().Raw(), dst)
	if copied["executions"] != 3 || copied["worker_dispatches"] == 0 {
		t.Fatalf("setup: copied %v", copied)
	}
	for tb, n := range skipped {
		t.Logf("not representable at schema %d: %s × %d", from, tb, n)
	}
	// History, not live operational state: the new instance legitimately
	// reconciles worker sessions / leases at startup.
	live := map[string]bool{"worker_sessions": true, "worker_session_leases": true}
	var oldTables []string
	for tb := range copied {
		if !live[tb] {
			oldTables = append(oldTables, tb)
		}
	}
	before := map[string]map[string]bool{}
	oldCols := map[string][]string{}
	for _, tb := range oldTables {
		oldCols[tb] = p18Columns(t, ctx, dst, tb)
		before[tb] = p18TableRows(t, ctx, dst, tb, oldCols[tb])
	}
	done.h.Close()

	// Upgrade: a new instance on the old database migrates it.
	h := NewHarnessOnWith(t, baseDSN(), name, p15Mutate(done.srv, proxyOf(t, done.g).URL(), p17Config(func(c *config.Config) {
		c.Engine.RecoveryDisabled = false
	})))
	t.Cleanup(h.Close)
	rep, err := h.App.Integrity().Scan(ctx, nil, nil)
	if err != nil || !rep.OK() {
		t.Fatalf("invariant scan after upgrade: %v %v", err, p18Violations(rep))
	}
	for _, tb := range oldTables {
		after := p18TableRows(t, ctx, h.App.DB().Raw(), tb, oldCols[tb])
		for row := range before[tb] {
			if !after[row] {
				t.Errorf("history of %s changed by the upgrade (a pre-existing row is gone or altered)", tb)
				break
			}
		}
	}
	// The audit history written before the chain existed is linked at
	// startup and verifies; the prompt cache survived.
	if v, err := h.App.AuditVerifier().Verify(ctx, nil); err != nil || v.Status != audit.ChainValid {
		t.Fatalf("audit after upgrade: %v %+v", err, v)
	}
	if n := p18CountOn(t, h, "SELECT count(*) FROM audit_events WHERE event_hash IS NULL"); n != 0 {
		t.Fatalf("%d audit rows left unlinked", n)
	}
	if copied["audit_events"] > 0 {
		if n := p18CountOn(t, h, "SELECT count(*) FROM audit_events"); n < 1500 {
			t.Fatalf("audit history lost: %d rows", n)
		}
	} else if from >= 107 {
		t.Fatal("setup: no audit history at a schema that has the audit log")
	}
	if n := p18CountOn(t, h, "SELECT count(*) FROM prompt_cache_entries"); n != cacheEntries {
		t.Fatalf("prompt cache entries %d, before %d", n, cacheEntries)
	}
	b := newBrowser(t, h.BaseURL)
	if code, _ := b.api(http.MethodGet, "/api/v1/projects/"+done.pid.String()+"/prompt-cache", nil); code != http.StatusOK {
		t.Fatalf("prompt cache after upgrade: %d", code)
	}
	for _, path := range []string{"/", "/projects", "/projects/" + done.pid.String(), "/projects/" + paused.pid.String(),
		"/projects/" + running.pid.String(), "/audit", "/system"} {
		if code, _ := b.page(path); code != http.StatusOK {
			t.Errorf("page %s: %d after upgrade", path, code)
		}
	}

	// 18.135: the paused project resumes and completes; 18.136: the
	// running one is reconciled by the new instance and completes.
	p2, r2 := p18Retarget(paused, h), p18Retarget(running, h)
	if code, body := b.api(http.MethodPost, "/api/v1/projects/"+paused.pid.String()+"/resume", nil); code != http.StatusOK {
		t.Fatalf("resume: %d %s", code, body)
	}
	p2.queue("complete")
	p2.runUntil(t, execution.StepCompleted, 200)
	r2.queue("complete")
	r2.runUntil(t, execution.StepCompleted, 200)
	for _, r := range []*p16Rig{p2, r2} {
		if n := r.count(t, `SELECT count(*) FROM (SELECT work_unit_id FROM worker_dispatches WHERE project_id = '`+r.pid.String()+`'
			AND work_unit_id IS NOT NULL AND status = 'COMPLETED' GROUP BY work_unit_id HAVING count(*) > 1) d`); n != 0 {
			t.Fatalf("project %s: %d units dispatched twice", r.pid, n)
		}
	}
}

func p18CountOn(t *testing.T, h *Harness, sql string) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(), sql, &n); err != nil {
		t.Fatal(err)
	}
	return n
}
