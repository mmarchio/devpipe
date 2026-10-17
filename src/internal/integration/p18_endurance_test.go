package integration

import (
	"bufio"
	"context"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/audit"
	"overseer/internal/config"
	"overseer/internal/faults"
	"overseer/internal/gateway/hermes/hermestest"
)

func p18FDs() int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(ents)
}

func p18Heap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// T18.156–T18.160 + T18.224 — endurance: several projects run concurrently
// under the scheduler (mock worker, slow replies, periodic injected
// database serialization failures, SSE clients attached). Every project
// completes with no duplicated DONE, no skipped unit, no unsafe retry and
// a valid phase order; the audit chain verifies; the invariant / isolation
// scan is clean; heap, goroutines, file descriptors and database
// connections return to bounded levels. OVERSEER_ENDURANCE=full scales
// the workload up (Decision 14).
func TestT18156Endurance(t *testing.T) {
	projects, units, deadline := 3, 3, 3*time.Minute
	if os.Getenv("OVERSEER_ENDURANCE") == "full" {
		projects, units, deadline = 100, 10, 60*time.Minute
	}
	phases := []p15Phase{{1, "One", "PENDING", 1}, {2, "Two", "PENDING", 1}}
	a := p17NewRigWith(t, p16Options{phases: phases, config: func(c *config.Config) {
		c.Engine.SchedulerDisabled = false
		c.Engine.Interval = 20 * time.Millisecond
		c.Engine.Workers = 4
	}})
	rigs := []*p16Rig{a.p16Rig}
	for i := 1; i < projects; i++ {
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
	var rngMu sync.Mutex
	rng := rand.New(rand.NewSource(18156))
	a.g.Responder = func(m hermestest.Message) *hermestest.Behavior {
		rngMu.Lock()
		delay := time.Duration(rng.Intn(25)) * time.Millisecond
		rngMu.Unlock()
		if m.Metadata["state_key"] == "PHASE_PLAN" {
			return &hermestest.Behavior{Content: &plan, Delay: delay}
		}
		return &hermestest.Behavior{Content: &done, Usage: p14Usage(1000), Delay: delay}
	}
	// Periodic database faults (transient serialization failures / deadlocks).
	reg := faults.NewRegistry()
	for _, act := range faults.Random(18156, []faults.Point{faults.BeforeDBCommit}, 12, 40*projects*units, faults.KindError) {
		act.Err = &pgconn.PgError{Code: "40001", Message: "injected serialization failure"}
		reg.Arm(act)
	}
	defer faults.Enable(reg)()

	baseG, baseFD, baseHeap := runtime.NumGoroutine(), p18FDs(), p18Heap()
	// SSE clients for the run.
	ctx, cancelSSE := context.WithCancel(context.Background())
	var sse sync.WaitGroup
	for _, r := range rigs {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, a.h.BaseURL+"/api/v1/projects/"+r.pid.String()+"/events/stream", nil)
		req.Header.Set("Accept", "text/event-stream")
		a.b.page("/")
		resp, err := a.b.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		sse.Add(1)
		go func() {
			defer sse.Done()
			defer resp.Body.Close()
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
			}
		}()
	}
	for _, r := range rigs {
		r.exec(t, "UPDATE project_settings SET auto_continue = true WHERE project_id = '"+r.pid.String()+"'")
	}
	a.eng().Kick()
	end := time.Now().Add(deadline)
	var midHeap uint64
	for {
		completed := 0
		for _, r := range rigs {
			switch s := r.execStatus(t); s {
			case "COMPLETED":
				completed++
			case "RECOVERY_REQUIRED", "FAILED":
				t.Fatalf("project %s: %s (%s)", r.pid, s, r.recoveryReason(t))
			}
		}
		if completed == len(rigs) {
			break
		}
		if midHeap == 0 && completed*2 >= len(rigs) {
			midHeap = p18Heap()
		}
		if time.Now().After(end) {
			t.Fatalf("%d / %d projects completed within %v", completed, len(rigs), deadline)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if reg.Hits(faults.BeforeDBCommit) == 0 {
		t.Fatal("no commit passed the fault point")
	}

	// Correctness.
	for _, r := range rigs {
		pid := r.pid.String()
		if n := r.count(t, "SELECT count(*) FROM work_units WHERE project_id = '"+pid+"' AND status = 'DONE'"); n != 2*units {
			t.Fatalf("project %s: %d DONE units, want %d (skipped units)", pid, n, 2*units)
		}
		if n := r.count(t, "SELECT count(*) FROM repository_checkpoints WHERE project_id = '"+pid+"' AND kind = 'UNIT' AND status = 'CREATED'"); n != 2*units {
			t.Fatalf("project %s: %d unit checkpoints", pid, n)
		}
		if n := r.count(t, `SELECT COALESCE(max(n), 0) FROM (SELECT count(*) AS n FROM worker_dispatches WHERE project_id = '`+pid+`'
			AND work_unit_id IS NOT NULL AND delivery IS DISTINCT FROM 'DEFINITELY_NOT_SENT' GROUP BY work_unit_id) x`); n != 1 {
			t.Fatalf("project %s: a unit request was delivered %d times", pid, n)
		}
		if n := r.count(t, "SELECT count(*) FROM phases WHERE project_id = '"+pid+"' AND status = 'DONE'"); n != 2 {
			t.Fatalf("project %s: %d phases DONE", pid, n)
		}
	}
	rep, err := a.h.App.Integrity().Scan(context.Background(), nil, nil)
	if err != nil || !rep.OK() {
		t.Fatalf("invariant / isolation scan: %v %v", err, p18Violations(rep))
	}
	if v, err := a.h.App.AuditVerifier().Verify(context.Background(), nil); err != nil || v.Status != audit.ChainValid {
		t.Fatalf("audit chain: %v %+v", err, v)
	}

	// Resources return to bounded levels.
	cancelSSE()
	sse.Wait()
	var pgConns int
	_ = a.h.DBScan(context.Background(), "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database()", &pgConns)
	if max := a.h.App.Config().Database.MaxConns; max > 0 && pgConns > max+2 {
		t.Fatalf("%d database connections (pool max %d)", pgConns, max)
	}
	// Idle worker keep-alive connections close after 15s.
	settle := time.Now().Add(25 * time.Second)
	for runtime.NumGoroutine() > baseG+25 && time.Now().Before(settle) {
		time.Sleep(100 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > baseG+25 {
		t.Fatalf("goroutines %d, baseline %d\n%s", g, baseG, p18GoroutineSummary())
	}
	if fd := p18FDs(); baseFD > 0 && fd > baseFD+30 {
		t.Fatalf("file descriptors %d, baseline %d", fd, baseFD)
	}
	if h := p18Heap(); h > baseHeap+64<<20 || (midHeap > 0 && h > midHeap+32<<20) {
		t.Fatalf("heap grew: baseline %d, mid %d, end %d", baseHeap, midHeap, h)
	}
	t.Logf("endurance: %d projects × %d units × 2 phases; %d commits through the fault point; goroutines %d→%d; fds %d→%d; heap %d→%d KiB",
		projects, units, reg.Hits(faults.BeforeDBCommit), baseG, runtime.NumGoroutine(), baseFD, p18FDs(), baseHeap>>10, p18Heap()>>10)
}

// p18GoroutineSummary groups live goroutines by their creating function
// and top frame.
func p18GoroutineSummary() string {
	buf := make([]byte, 8<<20)
	n := runtime.Stack(buf, true)
	counts := map[string]int{}
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		lines := strings.Split(g, "\n")
		top, created := "", ""
		if len(lines) > 1 {
			top = lines[1]
		}
		for i, l := range lines {
			if strings.HasPrefix(l, "created by ") && i+1 < len(lines) {
				created = l + " @ " + strings.TrimSpace(lines[i+1])
			}
		}
		counts[top+" | "+created]++
	}
	var out []string
	for k, v := range counts {
		out = append(out, itoa(v)+"× "+k)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}
