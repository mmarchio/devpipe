package dbrepo

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/metrics"
)

// Durable metrics (P18-59, WBS 18.113 / 18.121–18.124). Every series below
// is recomputed from database rows at scrape time inside one read-only
// snapshot: a restart neither resets nor double counts it, and two
// instances report the same values.

type countMetric struct {
	desc metrics.Desc
	// sql returns the label values (len(desc.Labels) text columns) then the
	// value (numeric).
	sql string
}

type histogramMetric struct {
	desc metrics.Desc
	// src returns the label values then the observed value v (seconds);
	// the collector aggregates it into the descriptor's buckets in SQL.
	src string
}

// workerOutcome classifies a finished dispatch; a timeout is its own
// series (WBS 18.121).
const workerOutcome = `CASE WHEN d.error_code = 'REQUEST_TIMEOUT' THEN 'timeout' WHEN d.status = 'COMPLETED' THEN 'completed'
	WHEN d.status = 'CANCELLED' THEN 'cancelled' WHEN d.status = 'UNKNOWN' THEN 'unknown' ELSE 'failed' END`

var durableCounts = []countMetric{
	{metrics.Desc{Name: "overseer_projects", Help: "Projects by status.", Category: "workflow", Kind: metrics.KindGauge, Labels: []string{"status"}},
		`SELECT status, count(*) FROM projects GROUP BY 1`},
	{metrics.Desc{Name: "overseer_executions", Help: "Executions by status.", Category: "workflow", Kind: metrics.KindGauge, Labels: []string{"status"}},
		`SELECT status, count(*) FROM executions GROUP BY 1`},
	{metrics.Desc{Name: "overseer_work_units", Help: "Work units by status.", Category: "workflow", Kind: metrics.KindGauge, Labels: []string{"status"}},
		`SELECT status, count(*) FROM work_units GROUP BY 1`},
	{metrics.Desc{Name: "overseer_execution_steps_total", Help: "Engine steps by action and outcome.", Category: "scheduler", Kind: metrics.KindCounter,
		Labels: []string{"action", "status"}}, `SELECT action, status, count(*) FROM execution_steps GROUP BY 1, 2`},
	{metrics.Desc{Name: "overseer_scheduler_waiting_executions", Help: "Executions waiting, by whether new work is blocked.", Category: "scheduler",
		Kind: metrics.KindGauge, Labels: []string{"blocked"}},
		`SELECT CASE WHEN COALESCE(wait_reason, '') LIKE 'new work blocked%' THEN 'true' ELSE 'false' END, count(*) FROM executions WHERE status = 'WAITING' GROUP BY 1`},
	{metrics.Desc{Name: "overseer_worker_dispatches_total", Help: "Worker dispatches by outcome (timeouts distinct).", Category: "worker",
		Kind: metrics.KindCounter, Labels: []string{"outcome"}},
		`SELECT CASE WHEN d.status IN ('PENDING', 'SENDING', 'WAITING') THEN 'in_flight' ELSE ` + workerOutcome + ` END, count(*) FROM worker_dispatches d GROUP BY 1`},
	{metrics.Desc{Name: "overseer_worker_timeouts_total", Help: "Worker requests that timed out.", Category: "worker", Kind: metrics.KindCounter},
		`SELECT count(*) FROM worker_dispatches WHERE error_code = 'REQUEST_TIMEOUT'`},
	{metrics.Desc{Name: "overseer_worker_prompts_total", Help: "Worker prompts prepared, by origin (precomputed projection or live).", Category: "prompt",
		Kind: metrics.KindCounter, Labels: []string{"origin"}},
		`SELECT CASE WHEN cp.projection_id IS NULL THEN 'live' ELSE 'precomputed' END, count(*) FROM worker_prompts wp
			JOIN canonical_prompts cp ON cp.id = wp.canonical_prompt_id GROUP BY 1`},
	{metrics.Desc{Name: "overseer_compression_runs_total", Help: "Compression runs by outcome.", Category: "compression", Kind: metrics.KindCounter,
		Labels: []string{"outcome"}}, `SELECT outcome, count(*) FROM compression_runs GROUP BY 1`},
	{metrics.Desc{Name: "overseer_prompt_cache_entries", Help: "Prompt cache entries.", Category: "cache", Kind: metrics.KindGauge},
		`SELECT count(*) FROM prompt_cache_entries`},
	{metrics.Desc{Name: "overseer_prompt_cache_hits_total", Help: "Prompt cache hits.", Category: "cache", Kind: metrics.KindCounter},
		`SELECT COALESCE(sum(hit_count), 0) FROM prompt_cache_entries`},
	{metrics.Desc{Name: "overseer_precompute_jobs_total", Help: "Precompute jobs by status.", Category: "precompute", Kind: metrics.KindCounter,
		Labels: []string{"status"}}, `SELECT status, count(*) FROM precompute_jobs GROUP BY 1`},
	{metrics.Desc{Name: "overseer_validation_runs_total", Help: "Validation runs by status.", Category: "validation", Kind: metrics.KindCounter,
		Labels: []string{"status"}}, `SELECT status, count(*) FROM validation_runs GROUP BY 1`},
	{metrics.Desc{Name: "overseer_validation_commands_total", Help: "Validation commands by outcome.", Category: "validation", Kind: metrics.KindCounter,
		Labels: []string{"outcome"}}, `SELECT outcome, count(*) FROM validation_command_results GROUP BY 1`},
	{metrics.Desc{Name: "overseer_repository_checkpoints_total", Help: "Repository checkpoints by kind and status.", Category: "repository",
		Kind: metrics.KindCounter, Labels: []string{"kind", "status"}}, `SELECT kind, status, count(*) FROM repository_checkpoints GROUP BY 1, 2`},
	{metrics.Desc{Name: "overseer_recovery_required_executions", Help: "Executions in RECOVERY_REQUIRED by condition.", Category: "recovery",
		Kind: metrics.KindGauge, Labels: []string{"condition"}},
		`SELECT COALESCE(NULLIF(recovery_evidence->>'condition', ''), 'UNCLASSIFIED'), count(*) FROM executions WHERE status = 'RECOVERY_REQUIRED' GROUP BY 1`},
	{metrics.Desc{Name: "overseer_recovery_entries_total", Help: "Times executions entered recovery.", Category: "recovery", Kind: metrics.KindCounter},
		`SELECT count(*) FROM execution_events WHERE type = 'EXECUTION_RECOVERY_REQUIRED'`},
	{metrics.Desc{Name: "overseer_recovery_resolutions_total", Help: "Recoveries resolved, automatically (engine) or manually (operator action).",
		Category: "recovery", Kind: metrics.KindCounter, Labels: []string{"mode"}},
		`SELECT 'automatic', count(*) FROM execution_events WHERE type = 'EXECUTION_RECOVERY_RESOLVED' AND COALESCE(payload->>'by', '') = ''
		UNION ALL SELECT 'manual', count(*) FROM recovery_actions WHERE status = 'SUCCEEDED'`},
	{metrics.Desc{Name: "overseer_recovery_action_failures_total", Help: "Operator recovery actions that failed.", Category: "recovery",
		Kind: metrics.KindCounter}, `SELECT count(*) FROM recovery_actions WHERE status = 'FAILED'`},
	{metrics.Desc{Name: "overseer_recovery_oldest_seconds", Help: "Time the longest-waiting execution has spent in recovery.", Category: "recovery",
		Kind: metrics.KindGauge}, `SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(updated_at)), 0) FROM executions WHERE status = 'RECOVERY_REQUIRED'`},
	{metrics.Desc{Name: "overseer_storage_bytes", Help: "Database storage by category (WBS 18.97).", Category: "database", Kind: metrics.KindGauge,
		Labels: []string{"category"}},
		`SELECT c.category, COALESCE(sum(pg_total_relation_size(to_regclass('public.' || c.t))), 0) FROM (VALUES
			('logs', 'execution_events'), ('logs', 'audit_events'), ('logs', 'worker_events'), ('logs', 'execution_steps'),
			('prompts', 'canonical_prompts'), ('prompts', 'canonical_prompt_components'), ('prompts', 'compressed_prompts'), ('prompts', 'worker_prompts'),
			('validation', 'validation_command_results'), ('validation', 'validation_runs'),
			('cache', 'prompt_cache_entries'), ('cache', 'precomputed_templates'),
			('state', 'state_file_versions'), ('state', 'materialized_state_files'), ('state', 'context_snapshot_files')) c(category, t)
			GROUP BY 1`},
	{metrics.Desc{Name: "overseer_database_size_bytes", Help: "Size of the overseer database.", Category: "database", Kind: metrics.KindGauge},
		`SELECT pg_database_size(current_database())`},
}

var durableHistograms = []histogramMetric{
	{metrics.Desc{Name: "overseer_worker_request_duration_seconds", Help: "Worker request → response latency by outcome (timeouts distinct).",
		Category: "worker", Labels: []string{"outcome"}},
		`SELECT ` + workerOutcome + `, EXTRACT(EPOCH FROM d.completed_at - d.started_at) FROM worker_dispatches d
			WHERE d.started_at IS NOT NULL AND d.completed_at IS NOT NULL`},
	{metrics.Desc{Name: "overseer_transition_latency_seconds", Help: "Worker result → next worker prompt ready, by precompute hit / miss.",
		Category: "precompute", Labels: []string{"precompute"}},
		`SELECT CASE WHEN cp.projection_id IS NULL THEN 'miss' ELSE 'hit' END, GREATEST(0, EXTRACT(EPOCH FROM wp.created_at - prev.result_at))
			FROM (SELECT d.project_id, d.worker_prompt_id, d.created_at,
				LAG(r.created_at) OVER (PARTITION BY d.project_id ORDER BY d.created_at, d.id) AS result_at
				FROM worker_dispatches d LEFT JOIN worker_results r ON r.dispatch_id = d.id) prev
			JOIN worker_prompts wp ON wp.id = prev.worker_prompt_id JOIN canonical_prompts cp ON cp.id = wp.canonical_prompt_id
			WHERE prev.result_at IS NOT NULL`},
	{metrics.Desc{Name: "overseer_validation_run_duration_seconds", Help: "Validation run duration by status.", Category: "validation",
		Labels: []string{"status"}},
		`SELECT status, EXTRACT(EPOCH FROM finished_at - started_at) FROM validation_runs WHERE finished_at IS NOT NULL`},
	{metrics.Desc{Name: "overseer_validation_command_duration_seconds", Help: "Validation command duration by outcome.", Category: "validation",
		Labels: []string{"outcome"}}, `SELECT outcome, duration_ms / 1000.0 FROM validation_command_results`},
}

// DurableMetricsCollector computes the durable metrics.
type DurableMetricsCollector struct {
	pool  *pgxpool.Pool
	hists map[string]*metrics.Histogram
}

// NewDurableMetricsCollector registers the durable metrics in reg.
func NewDurableMetricsCollector(pool *pgxpool.Pool, reg *metrics.Registry) *DurableMetricsCollector {
	c := &DurableMetricsCollector{pool: pool, hists: map[string]*metrics.Histogram{}}
	for _, m := range durableCounts {
		if m.desc.Kind == metrics.KindGauge {
			reg.Gauge(m.desc)
		} else {
			reg.Counter(m.desc)
		}
	}
	for _, h := range durableHistograms {
		c.hists[h.desc.Name] = reg.Histogram(h.desc)
	}
	return c
}

// Collect refreshes every durable series in reg from one read-only
// snapshot.
func (c *DurableMetricsCollector) Collect(ctx context.Context, reg *metrics.Registry) error {
	tx, err := c.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return TranslateDBError("metrics snapshot", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, m := range durableCounts {
		rows, err := tx.Query(ctx, m.sql)
		if err != nil {
			return TranslateDBError("metric "+m.desc.Name, err)
		}
		n := len(m.desc.Labels)
		for rows.Next() {
			labels := make([]string, n)
			var v float64
			dest := make([]any, 0, n+1)
			for i := range labels {
				dest = append(dest, &labels[i])
			}
			dest = append(dest, &v)
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return TranslateDBError("metric "+m.desc.Name, err)
			}
			if m.desc.Kind == metrics.KindGauge {
				reg.Gauge(m.desc).Set(v, labels...)
			} else {
				reg.Counter(m.desc).SetTotal(v, labels...)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return TranslateDBError("metric "+m.desc.Name, err)
		}
	}
	for _, h := range durableHistograms {
		if err := c.collectHistogram(ctx, tx, h); err != nil {
			return err
		}
	}
	return nil
}

func (c *DurableMetricsCollector) collectHistogram(ctx context.Context, tx pgx.Tx, h histogramMetric) error {
	hist := c.hists[h.desc.Name]
	buckets := hist.Buckets()
	// One pass per label set: cumulative counts per bucket, sum, count.
	parts := make([]string, len(buckets))
	for i := range buckets {
		parts[i] = fmt.Sprintf("count(*) FILTER (WHERE v <= ($1::float8[])[%d])", i+1)
	}
	q := `SELECT l, ARRAY[` + strings.Join(parts, ", ") + `]::bigint[], sum(v), count(*) FROM (` + h.src + `) s(l, v) GROUP BY l`
	rows, err := tx.Query(ctx, q, buckets)
	if err != nil {
		return TranslateDBError("metric "+h.desc.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var label string
		var counts []int64
		var sum *float64
		var count int64
		if err := rows.Scan(&label, &counts, &sum, &count); err != nil {
			return TranslateDBError("metric "+h.desc.Name, err)
		}
		cum := make([]uint64, len(counts))
		for i, n := range counts {
			cum[i] = uint64(n)
		}
		s := 0.0
		if sum != nil {
			s = *sum
		}
		hist.SetCumulative(cum, s, uint64(count), label)
	}
	if err := rows.Err(); err != nil {
		return TranslateDBError("metric "+h.desc.Name, err)
	}
	return nil
}
