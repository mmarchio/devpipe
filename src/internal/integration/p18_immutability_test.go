package integration

import (
	"context"
	"fmt"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/execution"
)

// T18.204 — historical artifacts reject updates: snapshots, WBS / state
// file / template versions, canonical / compressed / worker prompts,
// worker results, final dispatches, validation runs and results (except
// the sanctioned log expiry), checkpoints, events, applied steps, audit.
// The one structural exception — a deleted projection clearing a
// prompt's projection link — still works.
func TestT18204HistoryImmutable(t *testing.T) {
	r := p16NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 200)
	ctx := context.Background()
	pid := r.pid.String()
	// Fixtures for artifacts this run does not produce.
	if err := dbrepo.NewAuditRepository(r.h.App.DB().Raw()).AppendAudit(ctx, nil, p18AuditEvent(&r.pid, 1)); err != nil {
		t.Fatal(err)
	}
	r.exec(t, `INSERT INTO context_snapshot_files (id, snapshot_id, state_file_id, type_key, scope, version, content_hash, content, created_at)
		SELECT gen_random_uuid(), id, gen_random_uuid(), 'FIXTURE', 'PROJECT', 1, 'h', 'c', now() FROM context_snapshots WHERE project_id = '`+pid+`' LIMIT 1`)
	updates := map[string]string{
		"context_snapshots":                 "UPDATE context_snapshots SET created_at = created_at + interval '1 s' WHERE project_id = '%s'",
		"context_snapshot_files":            "UPDATE context_snapshot_files SET content = content || 'x' WHERE snapshot_id IN (SELECT id FROM context_snapshots WHERE project_id = '%s')",
		"canonical_prompts":                 "UPDATE canonical_prompts SET content = content || 'x' WHERE project_id = '%s'",
		"canonical_prompt_components":       "UPDATE canonical_prompt_components SET content = content || 'x' WHERE canonical_prompt_id IN (SELECT id FROM canonical_prompts WHERE project_id = '%s')",
		"worker_prompts":                    "UPDATE worker_prompts SET content = content || 'x' WHERE project_id = '%s'",
		"worker_results":                    "UPDATE worker_results SET summary = summary || 'x' WHERE project_id = '%s'",
		"worker_dispatches (final)":         "UPDATE worker_dispatches SET error_message = 'x' WHERE project_id = '%s' AND status = 'COMPLETED'",
		"validation_runs (finished)":        "UPDATE validation_runs SET status = 'FAIL' WHERE project_id = '%s' AND status = 'PASS'",
		"validation_command_results":        "UPDATE validation_command_results SET exit_code = 1 WHERE run_id IN (SELECT id FROM validation_runs WHERE project_id = '%s')",
		"repository_checkpoints":            "UPDATE repository_checkpoints SET commit_sha = repeat('0', 40) WHERE project_id = '%s' AND status = 'CREATED'",
		"execution_events":                  "UPDATE execution_events SET payload = '{}' WHERE project_id = '%s'",
		"execution_steps (applied)":         "UPDATE execution_steps SET intent_at = intent_at + interval '1 s' WHERE project_id = '%s' AND status = 'APPLIED'",
		"execution_configuration_snapshots": "UPDATE execution_configuration_snapshots SET canonical = canonical || ' ' WHERE project_id = '%s'",
		"wbs_versions":                      "UPDATE wbs_versions SET content = content || 'x' WHERE phase_id IN (SELECT id FROM phases WHERE project_id = '%s')",
		"state_file_versions":               "UPDATE state_file_versions SET content = content || 'x' WHERE state_file_id IN (SELECT id FROM state_files WHERE project_id = '%s')",
		"prompt_template_versions":          "UPDATE prompt_template_versions SET content = content || 'x' WHERE '%s' <> ''",
		"audit_events":                      "UPDATE audit_events SET reason = 'x' WHERE '%s' <> ''",
	}
	for name, q := range updates {
		sql := fmt.Sprintf(q, pid)
		tag, err := r.h.DBExec(ctx, sql)
		if err == nil && tag.RowsAffected() > 0 {
			t.Errorf("%s: history updated", name)
		}
		if err == nil {
			t.Errorf("%s: no row matched — the update was not exercised", name)
		}
	}
	// A deleted projection still clears the prompt's link (and only it).
	r.exec(t, `SET session_replication_role = replica;
		INSERT INTO precompute_projections (id, project_id, base_snapshot_id, base_semantic_hash, projected_state_key, projection_hash, created_at)
			SELECT gen_random_uuid(), project_id, context_snapshot_id, 'h', 'IMPLEMENT', 'imm', now() FROM canonical_prompts WHERE project_id = '`+pid+`' LIMIT 1;
		UPDATE canonical_prompts SET projection_id = (SELECT id FROM precompute_projections WHERE projection_hash = 'imm')
			WHERE id = (SELECT id FROM canonical_prompts WHERE project_id = '`+pid+`' LIMIT 1);
		SET session_replication_role = origin`)
	if _, err := r.h.DBExec(ctx, "DELETE FROM precompute_projections WHERE projection_hash = 'imm'"); err != nil {
		t.Fatalf("deleting a referenced projection: %v", err)
	}
	if r.count(t, "SELECT count(*) FROM canonical_prompts WHERE project_id = '"+pid+"' AND projection_id IS NOT NULL") != 0 {
		t.Fatal("projection link not cleared")
	}
}
