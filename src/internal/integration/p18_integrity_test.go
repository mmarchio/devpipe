package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"overseer/internal/execution"
)

// p18FKlessColumnsSQL lists uuid "*_id" columns of base tables that no
// foreign key covers.
const p18FKlessColumnsSQL = `
SELECT c.table_name || '.' || c.column_name FROM information_schema.columns c
JOIN information_schema.tables t ON t.table_name = c.table_name AND t.table_schema = 'public' AND t.table_type = 'BASE TABLE'
WHERE c.table_schema = 'public' AND c.column_name LIKE '%\_id' AND c.data_type = 'uuid'
AND NOT EXISTS (SELECT 1 FROM pg_constraint k JOIN pg_attribute a ON a.attrelid = k.conrelid AND a.attnum = ANY (k.conkey)
	WHERE k.contype = 'f' AND k.conrelid = (quote_ident(c.table_name))::regclass AND a.attname = c.column_name)
ORDER BY 1`

// p18UnreferencedByDesign are uuid reference columns deliberately left
// without a foreign key (WBS 18.38 review). Adding a column to this list
// is a conscious decision recorded with its reason.
var p18UnreferencedByDesign = map[string]string{
	// Audit history outlives the records it describes (WBS 18.102).
	"audit_events.execution_id": "audit", "audit_events.phase_id": "audit",
	"audit_events.project_id": "audit", "audit_events.work_unit_id": "audit",
	// Correlation identifiers, not row references.
	"worker_dispatches.request_id": "correlation id", "worker_results.request_id": "correlation id",
	// Polymorphic targets (a work unit or a phase), checked by the engine.
	"validation_runs.target_id": "polymorphic", "repository_checkpoints.target_id": "polymorphic",
	"fix_attempts.target_id": "polymorphic", "canonical_prompt_components.source_id": "polymorphic source",
	// Immutable historical snapshots keep their identifiers even if a
	// draft phase / unit is later deleted.
	"context_snapshots.phase_id": "historical", "context_snapshots.runtime_state_id": "historical",
	"context_snapshots.state_machine_id": "historical", "context_snapshots.work_unit_id": "historical",
	"context_snapshot_files.state_file_id": "historical", "state_machines.family_id": "family key",
	// Speculative / cache / metric records: garbage-collected
	// independently (WBS 18.98–18.104); never workflow authority.
	"precompute_candidates.cache_entry_id": "speculative", "precompute_candidates.phase_id": "speculative",
	"precompute_candidates.projection_id": "speculative", "precompute_candidates.template_id": "speculative",
	"precompute_candidates.work_unit_id": "speculative", "precompute_jobs.base_snapshot_id": "speculative",
	"precompute_jobs.work_unit_id": "speculative", "precompute_projections.phase_id": "speculative",
	"precompute_projections.projected_work_unit_id": "speculative", "precomputed_templates.resolved_entry_id": "speculative",
	"precomputed_templates.work_unit_id": "speculative", "prompt_cache_entries.phase_id": "cache",
	"prompt_cache_entries.work_unit_id": "cache", "prompt_cache_events.entry_id": "cache",
	"prompt_cache_events.work_unit_id": "cache", "transition_latencies.cache_entry_id": "metric",
	"transition_latencies.work_unit_id": "metric", "transition_latencies.worker_prompt_id": "metric",
	// Lease rows point at the dispatch they protect but outlive nothing.
	"worker_session_leases.dispatch_id": "lease", "restore_state.backup_id": "restore bookkeeping",
}

// T18.38 (catalog) — the required constraints exist, and every uuid
// reference column either has a foreign key or is excluded by design.
func TestT1838ConstraintCatalog(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	conn := h.connect(ctx)
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, p18FKlessColumnsSQL)
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for rows.Next() {
		var col string
		_ = rows.Scan(&col)
		if _, ok := p18UnreferencedByDesign[col]; !ok {
			missing = append(missing, col)
		}
	}
	rows.Close()
	if len(missing) > 0 {
		t.Fatalf("reference columns without a foreign key (add one, or record why not): %v", missing)
	}
	required := []string{
		"phases_project_id_phase_order_key", "execution_events_execution_seq_key", "execution_events_project_seq_key",
		"audit_events_seq_key", "executions_one_active_per_project", "work_units_one_active_claim",
		"prompt_cache_entries_authoritative_key", "execution_steps_idempotency_key_key", "worker_dispatches_idempotency",
		"recovery_actions_idempotency_key_key", "repository_checkpoints_checkpoint_key_key", "continuations_one_active_per_unit",
		"work_units_project_fk", "worker_dispatches_unit_owner_fk", "worker_dispatches_phase_owner_fk",
		"worker_results_dispatch_owner_fk", "worker_results_session_owner_fk", "worker_results_unit_owner_fk",
		"execution_steps_execution_owner_fk", "execution_steps_unit_owner_fk", "execution_steps_validation_run_fk",
		"execution_steps_checkpoint_fk", "execution_events_execution_owner_fk", "validation_runs_execution_owner_fk",
		"validation_runs_unit_owner_fk", "repository_checkpoints_execution_owner_fk", "continuations_unit_owner_fk",
	}
	var have []string
	rows, err = conn.Query(ctx, `SELECT conname FROM pg_constraint WHERE connamespace = 'public'::regnamespace
		UNION SELECT indexname FROM pg_indexes WHERE schemaname = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		have = append(have, n)
	}
	rows.Close()
	sort.Strings(have)
	for _, n := range required {
		if i := sort.SearchStrings(have, n); i == len(have) || have[i] != n {
			t.Errorf("required constraint %s missing", n)
		}
	}
}

// p18CloneInsert inserts a copy of one existing row of table (selected by
// where) with the given column overrides — a direct insert that is valid
// in every respect except the overridden values.
func p18CloneInsert(ctx context.Context, h *Harness, table, where string, overrides map[string]any) error {
	conn := h.connect(ctx)
	defer conn.Close(ctx)
	raw, _ := json.Marshal(overrides)
	_, err := conn.Exec(ctx, `INSERT INTO `+table+` SELECT (jsonb_populate_record(NULL::`+table+`, to_jsonb(s) || $1::jsonb)).* FROM `+
		table+` s WHERE `+where+` LIMIT 1`, string(raw))
	return err
}

func p18SQLState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// T18.38 — direct invalid inserts fail for each listed invariant: unique
// phase ordering, unique cache keys, unique event sequences, one active
// execution, one claimed unit, valid references and project ownership.
func TestT1838DirectInvalidInsertsFail(t *testing.T) {
	a := p16NewRig(t)
	a.queuePlan(a.planJSON(p16Unit{key: "a"}))
	a.queue("complete")
	a.runUntil(t, execution.StepCompleted, 100)
	b := &p16Rig{p15Rig: p15Build(t, a.h, a.g, a.srv, []p15Phase{{1, "Foundation", "PENDING", 1}})}
	p16Configure(t, b, p16Options{})
	b.queuePlan(b.planJSON(p16Unit{key: "a"}))
	b.queue("complete")
	b.runUntil(t, execution.StepCompleted, 100)
	ctx := context.Background()
	h := a.h
	id := func(sql string) string {
		var s string
		if err := h.DBScan(ctx, sql, &s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	pa, pb := a.pid.String(), b.pid.String()
	unitB := id("SELECT id::text FROM work_units WHERE project_id = '" + pb + "' LIMIT 1")
	phaseB := id("SELECT id::text FROM phases WHERE project_id = '" + pb + "' LIMIT 1")
	execB := id("SELECT id::text FROM executions WHERE project_id = '" + pb + "' LIMIT 1")
	sessB := id("SELECT id::text FROM worker_sessions WHERE project_id = '" + pb + "' LIMIT 1")
	planB := id("SELECT ep.id::text FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE p.project_id = '" + pb + "' LIMIT 1")
	dispA := "project_id = '" + pa + "' AND work_unit_id IS NOT NULL"
	stepA := "project_id = '" + pa + "' AND work_unit_id IS NOT NULL"
	newID := map[string]any{"id": "00000000-0000-4000-8000-000000000001"}
	seq := 0x10
	with := func(extra map[string]any) map[string]any {
		seq++
		out := map[string]any{"id": "00000000-0000-4000-8000-0000000000" + strconv.FormatInt(int64(seq), 16)}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	// Positive control: a clone differing only in its unique keys is valid.
	if err := p18CloneInsert(ctx, h, "worker_dispatches", dispA, with(map[string]any{
		"request_id": "00000000-0000-4000-8000-0000000000aa", "attempt": 99})); err != nil {
		t.Fatalf("control insert rejected: %v", err)
	}
	// One active execution per project: the first RUNNING copy of the
	// completed execution is valid, a second one is not.
	if err := p18CloneInsert(ctx, h, "executions", "project_id = '"+pa+"'", with(map[string]any{"status": "RUNNING", "number": 98})); err != nil {
		t.Fatalf("control execution rejected: %v", err)
	}

	cases := []struct {
		name, table, where string
		over               map[string]any
		code               string // accepted SQLSTATEs, "|"-separated
	}{
		{"duplicate phase order", "phases", "project_id = '" + pa + "'", newID, "23505"},
		{"duplicate execution event sequence", "execution_events", "project_id = '" + pa + "'", newID, "23505"},
		{"duplicate step idempotency key", "execution_steps", stepA, newID, "23505"},
		{"second active execution", "executions", "project_id = '" + pa + "'", with(map[string]any{"status": "RUNNING", "number": 99}), "23505"},
		{"duplicate dispatch request", "worker_dispatches", dispA, with(map[string]any{"attempt": 98}), "23505"},
		{"work unit of another project's plan", "work_units", "project_id = '" + pa + "'",
			with(map[string]any{"execution_plan_id": planB, "sequence": 99}), "23503"},
		{"dispatch for another project's unit", "worker_dispatches", dispA,
			with(map[string]any{"work_unit_id": unitB, "request_id": "00000000-0000-4000-8000-0000000000ab", "attempt": 97}), "23503"},
		{"dispatch for another project's phase", "worker_dispatches", dispA,
			with(map[string]any{"phase_id": phaseB, "request_id": "00000000-0000-4000-8000-0000000000ac", "attempt": 96}), "23503"},
		{"step of another project's execution", "execution_steps", stepA,
			with(map[string]any{"execution_id": execB, "idempotency_key": "p18-cross", "seq": 9999}), "23503"},
		{"step with a missing validation run", "execution_steps", stepA,
			with(map[string]any{"validation_run_id": "00000000-0000-4000-8000-0000000000ff", "idempotency_key": "p18-orphan", "seq": 9998}), "23503"},
		{"worker result in another project's session", "worker_results", "project_id = '" + pa + "'",
			with(map[string]any{"session_id": sessB}), "23503|23514"},
		{"validation run for another project's unit", "validation_runs", "project_id = '" + pa + "' AND work_unit_id IS NOT NULL",
			with(map[string]any{"work_unit_id": unitB}), "23503"},
		{"execution event of another project's execution", "execution_events", "project_id = '" + pa + "' AND execution_id IS NOT NULL",
			with(map[string]any{"execution_id": execB, "exec_seq": 9999, "project_seq": 9999, "seq": 999999}), "23503"},
	}
	for _, c := range cases {
		err := p18CloneInsert(ctx, h, c.table, c.where, c.over)
		if got := p18SQLState(err); got == "" || !strings.Contains("|"+c.code+"|", "|"+got+"|") {
			t.Errorf("%s: want SQLSTATE %s, got %q (%v)", c.name, c.code, got, err)
		}
	}
	// A work unit inserted without its project (the column default) takes
	// its plan's project; it can never be stored under another one.
	derived := with(map[string]any{"project_id": "00000000-0000-0000-0000-000000000000", "sequence": 97})
	if err := p18CloneInsert(ctx, h, "work_units", "project_id = '"+pa+"'", derived); err != nil {
		t.Fatalf("defaulted project rejected: %v", err)
	}
	if got := id("SELECT project_id::text FROM work_units WHERE id = '" + derived["id"].(string) + "'"); got != pa {
		t.Fatalf("defaulted project stored as %s, want %s", got, pa)
	}
	// Nothing was written by the rejected inserts.
	if n := a.count(t, "SELECT count(*) FROM worker_dispatches WHERE work_unit_id::text = '"+unitB+"' AND project_id::text = '"+pa+"'"); n != 0 {
		t.Fatal("cross-project dispatch stored")
	}

}
