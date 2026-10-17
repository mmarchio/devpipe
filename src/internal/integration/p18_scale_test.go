package integration

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"overseer/internal/config"
	"overseer/internal/gateway/hermes/hermestest"
)

// p18Fleet builds n scheduler-driven projects (units chained units per
// phase) answered by a request-aware mock worker (reply delay `delay`).
func p18Fleet(t *testing.T, n, units int, delay time.Duration, mut func(*config.Config)) (*p17Rig, []*p16Rig) {
	t.Helper()
	phases := []p15Phase{{1, "One", "PENDING", 1}}
	a := p17NewRigWith(t, p16Options{phases: phases, config: func(c *config.Config) {
		c.Engine.SchedulerDisabled = false
		c.Engine.Interval = 20 * time.Millisecond
		if mut != nil {
			mut(c)
		}
	}})
	rigs := []*p16Rig{a.p16Rig}
	for i := 1; i < n; i++ {
		r := &p16Rig{p15Rig: p15Build(t, a.h, a.g, a.srv, phases)}
		p16Configure(t, r, p16Options{})
		rigs = append(rigs, r)
	}
	var us []p16Unit
	for i := 0; i < units; i++ {
		u := p16Unit{key: "u" + itoa(i)}
		if i > 0 {
			u.deps = []string{"u" + itoa(i-1)}
		}
		us = append(us, u)
	}
	plan := p16Reply("complete", "Planned the phase.", `"plan":`+a.planJSON(us...))
	done := p16Reply("complete", "Worker reports complete.", "")
	a.g.Responder = func(m hermestest.Message) *hermestest.Behavior {
		if m.Metadata["state_key"] == "PHASE_PLAN" {
			return &hermestest.Behavior{Content: &plan, Delay: delay}
		}
		return &hermestest.Behavior{Content: &done, Usage: p14Usage(1000), Delay: delay}
	}
	return a, rigs
}

// p18Go enables every project and waits for all to complete.
func p18Go(t *testing.T, a *p17Rig, rigs []*p16Rig, deadline time.Duration) {
	t.Helper()
	for _, r := range rigs {
		r.exec(t, "UPDATE project_settings SET auto_continue = true WHERE project_id = '"+r.pid.String()+"'")
	}
	a.eng().Kick()
	end := time.Now().Add(deadline)
	for {
		n := 0
		for _, r := range rigs {
			switch s := r.execStatus(t); s {
			case "COMPLETED":
				n++
			case "RECOVERY_REQUIRED", "FAILED":
				t.Fatalf("project %s: %s (%s)", r.pid, s, r.recoveryReason(t))
			}
		}
		if n == len(rigs) {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%d / %d projects completed within %v", n, len(rigs), deadline)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// p18MaxOverlap is the largest number of [start, end) intervals open at
// once (rows of start / end timestamps).
func p18MaxOverlap(t *testing.T, a *p17Rig, sql string) int {
	t.Helper()
	rows, err := a.h.App.DB().Raw().Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	type ev struct {
		at time.Time
		d  int
	}
	var evs []ev
	for rows.Next() {
		var s, e time.Time
		if err := rows.Scan(&s, &e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev{s, 1}, ev{e, -1})
	}
	rows.Close()
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].at.Equal(evs[j].at) {
			return evs[i].d < evs[j].d
		}
		return evs[i].at.Before(evs[j].at)
	})
	cur, max := 0, 0
	for _, e := range evs {
		cur += e.d
		if cur > max {
			max = cur
		}
	}
	return max
}

// T18.161 — fairness: with more runnable projects than scheduler
// workers, every project starts promptly and none waits unboundedly
// between its steps; all complete.
func TestT18161SchedulerFairness(t *testing.T) {
	a, rigs := p18Fleet(t, 8, 2, 20*time.Millisecond, func(c *config.Config) { c.Engine.Workers = 2 })
	start := time.Now()
	p18Go(t, a, rigs, 3*time.Minute)
	for _, r := range rigs {
		var first time.Time
		_ = r.h.DBScan(context.Background(), "SELECT min(intent_at) FROM execution_steps WHERE execution_id = '"+r.execID.String()+"' AND intent_at > now() - interval '1 hour'", &first)
		var gap float64
		_ = r.h.DBScan(context.Background(), `SELECT COALESCE(max(EXTRACT(EPOCH FROM g)), 0) FROM (
			SELECT intent_at - LAG(COALESCE(finished_at, intent_at)) OVER (ORDER BY seq) AS g FROM execution_steps WHERE execution_id = '`+r.execID.String()+`') x`, &gap)
		if first.Sub(start) > 30*time.Second || gap > 15 {
			t.Fatalf("project %s starved: first step after %v, max gap %.1fs", r.pid, first.Sub(start), gap)
		}
	}
}

// T18.162 — saturation: with more runnable work than worker and
// validation capacity, requests queue — at most max_concurrent_dispatches
// worker calls and max_concurrent_validations validation runs are ever in
// flight — and everything completes.
func TestT18162SaturationQueues(t *testing.T) {
	a, rigs := p18Fleet(t, 4, 2, 150*time.Millisecond, func(c *config.Config) {
		c.Engine.Workers = 4
		c.Engine.MaxConcurrentDispatches = 1
		c.Engine.MaxConcurrentValidations = 1
	})
	lock, overlaps := filepath.Join(t.TempDir(), "lock"), filepath.Join(t.TempDir(), "overlaps")
	for _, r := range rigs {
		p18SlowChecks(t, r, "0.15", lock, overlaps)
	}
	p18Go(t, a, rigs, 3*time.Minute)
	p18NoOverlap(t, overlaps)
	if n := p18MaxOverlap(t, a, "SELECT started_at, completed_at FROM worker_dispatches WHERE started_at IS NOT NULL AND completed_at IS NOT NULL"); n > 1 {
		t.Fatalf("%d worker calls in flight at once (limit 1)", n)
	}
}

// T18.163 — backpressure: worker results arrive faster than validation
// capacity drains them; executions wait in a bounded queue (validation
// capacity reached) instead of piling up runs, and all complete.
func TestT18163ValidationBackpressure(t *testing.T) {
	a, rigs := p18Fleet(t, 4, 2, 0, func(c *config.Config) {
		c.Engine.Workers = 4
		c.Engine.MaxConcurrentValidations = 1
	})
	lock, overlaps := filepath.Join(t.TempDir(), "lock"), filepath.Join(t.TempDir(), "overlaps")
	for _, r := range rigs {
		p18SlowChecks(t, r, "0.3", lock, overlaps)
	}
	p18Go(t, a, rigs, 3*time.Minute)
	// Commands never exceed the capacity; runs waiting for a slot form a
	// queue bounded by the scheduler's drive workers.
	p18NoOverlap(t, overlaps)
	queued := p18MaxOverlap(t, a, "SELECT started_at, finished_at FROM validation_runs WHERE finished_at IS NOT NULL")
	if queued > 4 {
		t.Fatalf("%d validation runs pending at once (bounded by 4 drive workers)", queued)
	}
	if queued < 2 {
		t.Fatal("no backpressure happened (validations never queued)")
	}
}

// p18SlowChecks makes the rig's validation commands take secs seconds and
// detect concurrency themselves: an atomic mkdir lock that is already held
// means another command is running — recorded in overlapFile.
func p18SlowChecks(t *testing.T, r *p16Rig, secs, lockDir, overlapFile string) {
	t.Helper()
	raw, err := os.ReadFile(r.script)
	if err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nheld=0\nif mkdir " + lockDir + " 2>/dev/null; then held=1; else echo overlap >> " + overlapFile + "; fi\n" +
		"sleep " + secs + "\nif [ $held = 1 ]; then rmdir " + lockDir + "; fi\n" + string(raw)[len("#!/bin/sh\n"):]
	if err := os.WriteFile(r.script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func p18NoOverlap(t *testing.T, overlapFile string) {
	t.Helper()
	if b, err := os.ReadFile(overlapFile); err == nil && len(b) > 0 {
		t.Fatalf("validation commands ran concurrently (%d overlaps) beyond the limit of 1", strings.Count(string(b), "overlap"))
	}
}

// T18.164 — SSE endurance: 200 live clients — slow readers, clients that
// disconnect, normal readers — do not slow the engine down materially,
// and their connections are released.
func TestT18164SSEEndurance(t *testing.T) {
	a, rigs := p18Fleet(t, 2, 3, 10*time.Millisecond, func(c *config.Config) { c.Engine.Workers = 1 })
	// Baseline: the first project alone, no clients.
	t0 := time.Now()
	p18Go(t, a, rigs[:1], 2*time.Minute)
	base := time.Since(t0)

	baseG := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	a.b.page("/")
	url := a.h.BaseURL + "/api/v1/projects/" + rigs[1].pid.String() + "/events/stream"
	for i := 0; i < 200; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := a.b.c.Do(req)
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer resp.Body.Close()
			switch i % 4 {
			case 0: // slow: never reads
				<-ctx.Done()
			case 1: // disconnects early
				time.Sleep(100 * time.Millisecond)
			default:
				sc := bufio.NewScanner(resp.Body)
				for sc.Scan() {
				}
			}
		}(i)
	}
	t1 := time.Now()
	p18Go(t, a, rigs[1:], 2*time.Minute)
	loaded := time.Since(t1)
	cancel()
	wg.Wait()
	if loaded > 3*base+5*time.Second {
		t.Fatalf("engine slowed by SSE clients: %v with 200 clients vs %v alone", loaded, base)
	}
	settle := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > baseG+20 && time.Now().Before(settle) {
		time.Sleep(100 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > baseG+20 {
		t.Fatalf("goroutines %d after the clients left (baseline %d)", g, baseG)
	}
	t.Logf("project alone %v; with 200 SSE clients %v", base, loaded)
}
