package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"overseer/internal/execution"
)

// T18.99–T18.104 + decisions U1 / U2 — garbage collection removes only
// expired, unreferenced artifacts: prompts an execution used remain, the
// execution's history is unchanged, validation outcomes / exit codes stay
// (raw logs emptied, their hashes kept), audit is never collected, a run
// is idempotent, and a row a concurrent transaction is referencing
// survives.
func TestT1899GarbageCollection(t *testing.T) {
	r := p16NewRig(t)
	ctx := context.Background()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 200)
	pid := r.pid.String()
	q := func(sql string) int { return r.count(t, sql) }

	// Fixtures: everything is old (triggers bypassed: history is
	// immutable); one old projection is referenced by a canonical prompt,
	// two are not (one of them will be "in use" by a concurrent
	// transaction).
	r.exec(t, `SET session_replication_role = replica;
		UPDATE prompt_cache_entries SET last_used_at = now() - interval '8 days';
		UPDATE validation_command_results SET created_at = now() - interval '31 days', stdout = 'raw output', stderr = 'raw error';
		INSERT INTO audit_events (id, created_at, actor_id, actor_name, actor_type, action, resource_kind, result, request_id, chain_key,
			chain_seq, prev_hash, event_hash) VALUES (gen_random_uuid(), now() - interval '400 days', 'a', 'A', 'SYSTEM', 'MAINTENANCE', 'system',
			'SUCCEEDED', 'old', 'system', 999999, repeat('a', 64), repeat('b', 64));
		INSERT INTO precompute_projections (id, project_id, base_snapshot_id, base_semantic_hash, projected_state_key, projection_hash, created_at)
			SELECT gen_random_uuid(), project_id, context_snapshot_id, 'h', 'IMPLEMENT', 'p' || g, now() - interval '2 days'
			FROM canonical_prompts, generate_series(1, 3) g WHERE project_id = '`+pid+`' LIMIT 3;
		UPDATE canonical_prompts SET projection_id = (SELECT id FROM precompute_projections WHERE projection_hash = 'p1' LIMIT 1)
			WHERE id = (SELECT id FROM canonical_prompts WHERE project_id = '`+pid+`' LIMIT 1);
		SET session_replication_role = origin`)
	if q("SELECT count(*) FROM validation_command_results") == 0 {
		t.Fatal("setup: no validation results")
	}
	prompts := q(`SELECT count(*) FROM worker_dispatches d JOIN worker_prompts wp ON wp.id = d.worker_prompt_id
		JOIN canonical_prompts cp ON cp.id = wp.canonical_prompt_id WHERE d.project_id = '` + pid + `'`)
	history := dbFingerprintOf(t, r)
	outcomes := func() string {
		var s *string
		_ = r.h.DBScan(ctx, "SELECT string_agg(id::text || outcome || COALESCE(exit_code::text, '-') || duration_ms::text, ',' ORDER BY id) FROM validation_command_results", &s)
		if s == nil {
			return ""
		}
		return *s
	}()
	audits := q("SELECT count(*) FROM audit_events")

	// A concurrent transaction is referencing projection p3 (the key-share
	// lock a referencing insert takes) while GC runs.
	conn := r.h.connect(ctx)
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1 FROM precompute_projections WHERE projection_hash = 'p3' FOR KEY SHARE"); err != nil {
		t.Fatal(err)
	}
	res, err := r.h.App.GC().Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if !res.Complete || res.ValidationLogs == 0 || res.Projections != 1 {
		t.Fatalf("GC result %+v", res)
	}
	// 18.104: the referenced (p1) and the in-use (p3) projections survive.
	for _, h := range []string{"p1", "p3"} {
		if q("SELECT count(*) FROM precompute_projections WHERE projection_hash = '"+h+"'") != 1 {
			t.Fatalf("projection %s collected", h)
		}
	}
	if q("SELECT count(*) FROM precompute_projections WHERE projection_hash = 'p2'") != 0 {
		t.Fatal("expired unreferenced projection kept")
	}
	if q("SELECT count(*) FROM canonical_prompts WHERE projection_id IS NOT NULL AND project_id = '"+pid+"'") != 1 {
		t.Fatal("a prompt lost its projection lineage")
	}
	// 18.99 / 18.100: execution prompts and history unchanged.
	if q(`SELECT count(*) FROM worker_dispatches d JOIN worker_prompts wp ON wp.id = d.worker_prompt_id
		JOIN canonical_prompts cp ON cp.id = wp.canonical_prompt_id WHERE d.project_id = '`+pid+`'`) != prompts {
		t.Fatal("prompts an execution used were collected")
	}
	if dbFingerprintOf(t, r) != history {
		t.Fatal("the execution's history changed")
	}
	// 18.101: outcomes / exit codes kept, raw logs emptied with hashes.
	var after *string
	_ = r.h.DBScan(ctx, "SELECT string_agg(id::text || outcome || COALESCE(exit_code::text, '-') || duration_ms::text, ',' ORDER BY id) FROM validation_command_results", &after)
	if after == nil || *after != outcomes {
		t.Fatal("validation outcomes changed")
	}
	if q("SELECT count(*) FROM validation_command_results WHERE log_expired_at IS NULL OR stdout <> '' OR stdout_sha256 <> encode(sha256('raw output'::bytea), 'hex')") != 0 {
		t.Fatal("raw logs not expired with their hash")
	}
	// 18.102: audit untouched.
	if q("SELECT count(*) FROM audit_events") != audits {
		t.Fatal("GC collected audit events")
	}
	if q("SELECT count(*) FROM prompt_cache_entries WHERE last_used_at < now() - interval '7 days'") != 0 {
		t.Fatal("expired cache entries kept")
	}
	// Once released, p3 is an ordinary unreferenced expired projection:
	// the next run collects it and nothing else.
	again, err := r.h.App.GC().Run(ctx)
	if err != nil || again.Projections != 1 || again.CacheEntries+again.ValidationLogs+again.PrecomputedTemplates+again.PrecomputeJobs != 0 ||
		q("SELECT count(*) FROM precompute_projections WHERE projection_hash = 'p3'") != 0 {
		t.Fatalf("second run %+v %v", again, err)
	}
	// 18.103: idempotent.
	third, err := r.h.App.GC().Run(ctx)
	if err != nil || third.CacheEntries+third.Projections+third.ValidationLogs+third.PrecomputedTemplates+third.PrecomputeJobs != 0 {
		t.Fatalf("third run %+v %v", third, err)
	}
	// History mutation is still refused outside the sanctioned expiry.
	if _, err := r.h.DBExec(ctx, "UPDATE validation_command_results SET outcome = 'PASS'"); err == nil {
		t.Fatal("validation results became mutable")
	}
}

// dbFingerprintOf is dbFingerprint for a Phase 16 rig.
func dbFingerprintOf(t *testing.T, r *p16Rig) string {
	return dbFingerprint(t, &p17Rig{p16Rig: r})
}

// Decision U1 — GC runs immediately before a phase's planning step, and
// its outcome never blocks planning.
func TestT1899GCBeforePlanning(t *testing.T) {
	logs := captureLogs(t)
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	before := r.h.App.GC().Last()
	r.runUntil(t, execution.StepCompleted, 200)
	if after := r.h.App.GC().Last(); after == nil || after == before {
		t.Fatal("GC did not run before planning")
	}
	if !p18LogLine(logs.String(), "garbage collection finished") {
		t.Fatal("GC run not logged")
	}
}
