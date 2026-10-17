package integration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/resilience"
	"overseer/internal/service"
)

// p18ForceCompression makes every preparation call the compression
// provider (mode ALWAYS, fast bounded retries).
func p18ForceCompression(t *testing.T, r *p16Rig) {
	t.Helper()
	st := compression.DefaultSettings()
	st.Mode = compression.ModeAlways
	st.MaxAttempts = 2
	st.BackoffBase, st.BackoffMax = time.Millisecond, time.Millisecond
	st.Timeout = 5 * time.Second
	if _, err := r.h.App.Compression().UpdateCompressionSettings(context.Background(), r.pid, st); err != nil {
		t.Fatalf("compression settings: %v", err)
	}
}

func p18CompressionCircuit(failures int, cooldown time.Duration) func(*config.Config) {
	return func(c *config.Config) {
		p := c.CompressionProviders["fake"]
		p.Circuit = config.CircuitConfig{Failures: failures, Window: time.Minute, Cooldown: cooldown}
		c.CompressionProviders["fake"] = p
	}
}

// T18.29 / T18.27 / T18.31 — the compression provider is down: after the
// circuit opens no further compression request is sent, every prompt
// falls back to the canonical prompt (it fits the worker context), and
// execution completes. Precompute meets the same outage without failing
// the current work unit.
func TestT1829CompressionOutageFallback(t *testing.T) {
	r := p16NewRigWith(t, p16Options{config: p18CompressionCircuit(2, time.Hour)})
	p18ForceCompression(t, r)
	r.srv.Script(providertest.Reply{Status: 503, Body: `{"error":{"message":"overloaded"}}`})
	before := r.srv.Calls()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 300)

	if n := r.srv.Calls() - before; n != 2 {
		t.Fatalf("compression requests during the outage: %d (circuit should stop them at 2)", n)
	}
	if got := r.h.App.Compression().ProviderAvailability()["fake"]; got != resilience.Unavailable {
		t.Fatalf("availability %v", got)
	}
	pid := r.pid.String()
	if n := r.count(t, "SELECT count(*) FROM compression_runs WHERE project_id = '"+pid+"' AND outcome = 'FALLBACK_CANONICAL'"); n < 3 {
		t.Fatalf("canonical fallbacks %d", n)
	}
	if n := r.count(t, "SELECT count(*) FROM compression_runs WHERE project_id = '"+pid+"' AND reason LIKE '%circuit open%'"); n < 1 {
		t.Fatal("no preparation short-circuited by the open circuit")
	}
	if n := r.count(t, "SELECT count(*) FROM worker_prompts WHERE project_id = '"+pid+"' AND source <> 'CANONICAL'"); n != 0 {
		t.Fatalf("%d non-canonical worker prompts during the outage", n)
	}
	if s := r.unitStatuses(t, 1); strings.Count(s, "DONE") != 2 {
		t.Fatalf("units %s", s)
	}
	// Precompute ran into the same outage, yet the units completed.
	if n := r.count(t, "SELECT count(*) FROM precompute_jobs WHERE project_id = '"+pid+"'"); n == 0 {
		t.Fatal("precompute never ran during the outage")
	}
}

// T18.30 — compression is down and the canonical prompt does not fit the
// worker context: preparation stops (PROMPT_TOO_LARGE), nothing is
// truncated, nothing is dispatched.
func TestT1830UnsafeFallbackStops(t *testing.T) {
	r := p16NewRigWith(t, p16Options{config: p18CompressionCircuit(2, time.Hour)})
	p18ForceCompression(t, r)
	r.srv.Script(providertest.Reply{Status: 503, Body: `{"error":{"message":"overloaded"}}`})
	r.exec(t, "UPDATE project_settings SET context_window = 200 WHERE project_id = '"+r.pid.String()+"'")
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	sent := len(r.g.Messages())
	res, _ := r.eng().Run(context.Background(), r.execID, 50)
	if len(r.g.Messages()) != sent {
		t.Fatalf("a prompt was dispatched: %s", p16Trace(res))
	}
	if r.execStatus(t) != "RECOVERY_REQUIRED" {
		t.Fatalf("status %s: %s", r.execStatus(t), p16Trace(res))
	}
	var cond string
	_ = r.h.DBScan(context.Background(), "SELECT COALESCE(recovery_evidence->>'condition', '') FROM executions WHERE id = '"+r.execID.String()+"'", &cond)
	if cond != "PROMPT_TOO_LARGE" {
		t.Fatalf("condition %q reason %q", cond, r.recoveryReason(t))
	}
	if n := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"'"); n != 0 {
		t.Fatalf("%d dispatch rows", n)
	}
}

// T18.94 / T18.95 — with one compression slot, active preparation
// preempts a running speculative (precompute) preparation, which is
// deferred rather than failed; new speculative work is deferred while the
// slot is busy and records no run.
func TestT1895SpeculativeCompressionYields(t *testing.T) {
	r := p16NewRigWith(t, p16Options{config: func(c *config.Config) { c.Engine.MaxConcurrentCompressions = 1 }})
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 100)
	p18ForceCompression(t, r)

	var ids []uuid.UUID
	rows, err := r.h.App.DB().Raw().Query(context.Background(), "SELECT id FROM canonical_prompts WHERE project_id = $1 ORDER BY created_at LIMIT 3", r.pid)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) < 3 {
		t.Fatalf("canonical prompts %d", len(ids))
	}
	svc := r.h.App.Compression()
	fresh := func(uuid.UUID) bool { return true } // never reuse: force a provider run
	r.srv.Script(providertest.Reply{Delay: 3 * time.Second}, providertest.Reply{})
	calls := r.srv.Calls()

	spec := resilience.WithSpeculative(context.Background())
	specErr := make(chan error, 1)
	go func() {
		_, err := svc.PrepareWorkerPromptExcluding(spec, r.pid, ids[0], fresh)
		specErr <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for r.srv.Calls() == calls && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// The slot is busy: more speculative work is deferred, recording nothing.
	runs := r.count(t, "SELECT count(*) FROM compression_runs WHERE project_id = '"+r.pid.String()+"'")
	if _, err := svc.PrepareWorkerPromptExcluding(spec, r.pid, ids[1], fresh); !errors.Is(err, service.ErrCompressionDeferred) {
		t.Fatalf("second speculative: %v", err)
	}
	if n := r.count(t, "SELECT count(*) FROM compression_runs WHERE project_id = '"+r.pid.String()+"'"); n != runs {
		t.Fatalf("deferred speculative work recorded %d runs", n-runs)
	}
	// Active work preempts the speculative holder and proceeds.
	start := time.Now()
	res, err := svc.PrepareWorkerPromptExcluding(context.Background(), r.pid, ids[2], fresh)
	if err != nil || res.WorkerPrompt == nil {
		t.Fatalf("active preparation: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("active waited for the speculative run (%s)", time.Since(start))
	}
	if err := <-specErr; !errors.Is(err, service.ErrCompressionDeferred) {
		t.Fatalf("preempted speculative: %v", err)
	}
}
