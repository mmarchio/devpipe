package integration

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/metrics"
)

// p18Series returns the value of one exposition line (name{labels}), -1
// when absent.
func p18Series(text, series string) float64 {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`)
	m := re.FindStringSubmatch(text)
	if m == nil {
		return -1
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v
}

// T18.121–T18.124 — /metrics is derived from durable rows: values equal
// the database aggregates; worker timeouts are their own series; the
// worker-result → next-prompt transition latency is split by precompute
// hit / miss; repeated scrapes and a fresh process (new registry) report
// the same durable values — nothing is double counted.
func TestT18121DurableMetrics(t *testing.T) {
	r := p17NewRig(t)
	ctx := context.Background()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 200)
	pid := r.pid.String()

	// 18.121 fixture: one finished dispatch timed out.
	r.exec(t, `SET session_replication_role = replica; UPDATE worker_dispatches SET status = 'FAILED', error_code = 'REQUEST_TIMEOUT'
		WHERE id = (SELECT id FROM worker_dispatches WHERE project_id = '`+pid+`' AND status = 'COMPLETED' ORDER BY created_at DESC LIMIT 1); SET session_replication_role = origin`)
	// 18.122 fixture: the last unit's prompt came from a precomputed
	// projection (history is immutable; the fixture bypasses triggers).
	r.exec(t, `SET session_replication_role = replica;
		INSERT INTO precompute_projections (id, project_id, base_snapshot_id, base_semantic_hash, projected_state_key, projection_hash, created_at)
			SELECT gen_random_uuid(), project_id, context_snapshot_id, 'h', 'IMPLEMENT', 'p', now() FROM canonical_prompts WHERE project_id = '`+pid+`' LIMIT 1;
		UPDATE canonical_prompts SET projection_id = (SELECT id FROM precompute_projections WHERE project_id = '`+pid+`' LIMIT 1)
			WHERE id = (SELECT cp.id FROM worker_dispatches d JOIN worker_prompts wp ON wp.id = d.worker_prompt_id
				JOIN canonical_prompts cp ON cp.id = wp.canonical_prompt_id WHERE d.project_id = '`+pid+`' ORDER BY d.created_at DESC LIMIT 1);
		SET session_replication_role = origin`)

	code, body := r.b.page("/metrics")
	if code != http.StatusOK || !strings.Contains(body, "# TYPE overseer_worker_request_duration_seconds histogram") {
		t.Fatalf("/metrics: %d %.300s", code, body)
	}
	completed := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE status = 'COMPLETED' AND COALESCE(error_code, '') <> 'REQUEST_TIMEOUT'")
	if completed == 0 {
		t.Fatal("setup: no completed dispatch")
	}
	checks := map[string]float64{
		`overseer_worker_dispatches_total{outcome="timeout"}`:                 1,
		`overseer_worker_timeouts_total`:                                      1,
		`overseer_worker_dispatches_total{outcome="completed"}`:               float64(completed),
		`overseer_worker_request_duration_seconds_count{outcome="timeout"}`:   1,
		`overseer_worker_request_duration_seconds_count{outcome="completed"}`: float64(completed),
		`overseer_transition_latency_seconds_count{precompute="hit"}`:         1,
		`overseer_validation_runs_total{status="PASS"}`:                       float64(r.count(t, "SELECT count(*) FROM validation_runs WHERE status = 'PASS'")),
		`overseer_work_units{status="DONE"}`:                                  float64(r.count(t, "SELECT count(*) FROM work_units WHERE status = 'DONE'")),
		`overseer_executions{status="COMPLETED"}`:                             1,
	}
	for series, want := range checks {
		if got := p18Series(body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if p18Series(body, `overseer_transition_latency_seconds_count{precompute="miss"}`) < 1 {
		t.Error("no precompute-miss transition series")
	}
	// The +Inf bucket equals the count (cumulative buckets).
	if p18Series(body, `overseer_worker_request_duration_seconds_bucket{outcome="completed",le="+Inf"}`) != float64(completed) {
		t.Error("histogram buckets are not cumulative")
	}

	// 18.124: repeated scrapes and a fresh registry (a restarted process)
	// report the same durable values.
	durable := func(text string) string {
		var keep []string
		for _, l := range strings.Split(text, "\n") {
			if l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "overseer_http_") && !strings.HasPrefix(l, "overseer_database_") &&
				!strings.HasPrefix(l, "overseer_recovery_oldest") && !strings.HasPrefix(l, "overseer_metrics_") &&
				!strings.HasPrefix(l, "overseer_storage_bytes") && !strings.HasPrefix(l, "overseer_alert_") {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	_, again := r.b.page("/metrics")
	if p18Series(again, `overseer_http_requests_total{method="GET",class="2xx"}`) < 1 {
		t.Error("HTTP requests not counted")
	}
	if durable(again) != durable(body) {
		t.Fatal("a second scrape changed durable values")
	}
	fresh := metrics.NewRegistry()
	col := dbrepo.NewDurableMetricsCollector(r.h.App.DB().Raw(), fresh)
	if err := col.Collect(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_ = fresh.WriteText(&buf)
	if durable(buf.String()) != durable(body) {
		t.Fatalf("a fresh process reports different values:\n%s\n---\n%s", durable(buf.String()), durable(body))
	}
}
