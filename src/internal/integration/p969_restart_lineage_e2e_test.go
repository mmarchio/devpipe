package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"overseer/internal/compression"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/config"
	"overseer/internal/dbrepo"
)

// P9-69 (WBS 9.58), P9-70 (WBS 9.59), P9-71 (WBS 9.60 + Phase 9 DoD).

// p9RestartRig creates a caller-owned database and boots the app on it with
// the "fake" compression provider (the p834RestartRig shape).
func p9RestartRig(t *testing.T, srv *providertest.Server) (*Harness, string, func() *Harness) {
	t.Helper()
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })
	t.Setenv("P9_FAKE_KEY", p9SentinelKey)
	mutate := func(cfg *config.Config) {
		cfg.CompressionProviders = map[string]config.CompressionProviderConfig{
			"fake": {Kind: config.CompressionKindOpenAICompatible, BaseURL: srv.URL + "/v1", APIKeyEnv: "P9_FAKE_KEY"},
		}
	}
	boot := func() *Harness {
		h := NewHarnessOnWith(t, base, dbName, mutate)
		t.Cleanup(h.Close)
		return h
	}
	return boot(), dbName, boot
}

// p9Lineage captures every artifact of a canonical prompt as stable JSON
// (all rows, all columns that must survive a restart).
func p9Lineage(t *testing.T, h *Harness, canonicalID string) string {
	t.Helper()
	var out string
	q := `SELECT json_build_object(
		'canonical', (SELECT row_to_json(c) FROM canonical_prompts c WHERE c.id = '` + canonicalID + `'),
		'components', (SELECT json_agg(x ORDER BY x.ordinal) FROM canonical_prompt_components x WHERE x.canonical_prompt_id = '` + canonicalID + `'),
		'manifest', (SELECT json_agg(x ORDER BY x.ordinal) FROM canonical_prompt_preservation_items x WHERE x.canonical_prompt_id = '` + canonicalID + `'),
		'runs', (SELECT json_agg(x ORDER BY x.created_at, x.id) FROM compression_runs x WHERE x.canonical_prompt_id = '` + canonicalID + `'),
		'attempts', (SELECT json_agg(x ORDER BY x.created_at, x.number) FROM compression_attempts x WHERE x.canonical_prompt_id = '` + canonicalID + `'),
		'compressed', (SELECT json_agg(x ORDER BY x.attempt) FROM compressed_prompts x WHERE x.canonical_prompt_id = '` + canonicalID + `'),
		'worker', (SELECT json_agg(x ORDER BY x.created_at) FROM worker_prompts x WHERE x.canonical_prompt_id = '` + canonicalID + `'))::text`
	if err := h.DBScan(context.Background(), q, &out); err != nil {
		t.Fatalf("lineage snapshot: %v", err)
	}
	return out
}

// p9OrphanCount counts any compression artifact that is orphaned or points
// at the wrong canonical source (WBS 9.59).
func p9OrphanCount(t *testing.T, h *Harness) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(), `SELECT
		(SELECT count(*) FROM compression_attempts a JOIN compression_runs r ON r.id = a.run_id WHERE a.canonical_prompt_id <> r.canonical_prompt_id) +
		(SELECT count(*) FROM compressed_prompts c JOIN compression_attempts a ON a.id = c.attempt_id WHERE c.canonical_prompt_id <> a.canonical_prompt_id OR c.attempt <> a.number) +
		(SELECT count(*) FROM worker_prompts w JOIN compression_runs r ON r.id = w.run_id WHERE w.canonical_prompt_id <> r.canonical_prompt_id) +
		(SELECT count(*) FROM worker_prompts w JOIN compressed_prompts c ON c.id = w.compressed_prompt_id WHERE w.canonical_prompt_id <> c.canonical_prompt_id OR c.validation_status <> 'PASSED' OR w.content <> c.content) +
		(SELECT count(*) FROM worker_prompts w JOIN compressed_prompts c ON c.id = w.compressed_prompt_id JOIN compression_attempts a ON a.id = c.attempt_id WHERE a.run_id <> w.run_id OR a.status <> 'ACCEPTED') +
		(SELECT count(*) FROM compression_runs r WHERE r.outcome IN ('BYPASSED','COMPRESSED','FALLBACK_CANONICAL') AND NOT EXISTS (SELECT 1 FROM worker_prompts w WHERE w.run_id = r.id)) +
		(SELECT count(*) FROM compression_runs r WHERE r.outcome IN ('FAILED','PENDING') AND EXISTS (SELECT 1 FROM worker_prompts w WHERE w.run_id = r.id)) +
		(SELECT count(*) FROM compression_runs r WHERE r.attempts <> (SELECT count(*) FROM compression_attempts a WHERE a.run_id = r.id))`, &n); err != nil {
		t.Fatalf("orphan scan: %v", err)
	}
	return n
}

// WBS 9.58 — Canonical 101 / attempt 1 FAILED / attempt 2 ACCEPTED / worker
// prompt; restart; everything (content, attempt history, accepted attempt,
// worker prompt, hashes, metrics) reconstructs identically.
func TestP969RestartRecovery(t *testing.T) {
	srv := providertest.New(providertest.Reply{Edit: p9DropAC3}, providertest.Reply{})
	defer srv.Close()
	h, _, boot := p9RestartRig(t, srv)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)
	if _, err := h.DBExec(ctx, "UPDATE project_settings SET compression_provider = 'fake', compression_model = 'compressor-7b', context_window = "+itoa(n*4)+" WHERE project_id = '"+pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	base := h.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	promptURL := base + "/prompts/" + rec.ID.String()
	p9HTTP(t, http.MethodPut, base+"/settings/compression", `{"mode":"AUTO","target_tokens":`+itoa(n/2)+`,"max_attempts":2}`)
	code, raw, body := p9HTTP(t, http.MethodPost, promptURL+"/worker-prompt", "")
	if code != 200 {
		t.Fatalf("prepare: %d %s", code, raw)
	}
	wpID := body["worker_prompt"].(map[string]any)["id"].(string)
	_, attBefore, _ := p9HTTP(t, http.MethodGet, promptURL+"/compression-attempts", "")
	_, linBefore, _ := p9HTTP(t, http.MethodGet, base+"/worker-prompts/"+wpID+"/lineage", "")
	_, metBefore, _ := p9HTTP(t, http.MethodGet, base+"/compression-metrics", "")
	rowsBefore := p9Lineage(t, h, rec.ID.String())
	if !strings.Contains(attBefore, `"status":"VALIDATION_FAILED"`) || !strings.Contains(attBefore, `"status":"ACCEPTED"`) {
		t.Fatalf("fixture: expected FAILED then ACCEPTED: %s", attBefore)
	}

	h.Close()
	h2 := boot()
	base2 := h2.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	promptURL2 := base2 + "/prompts/" + rec.ID.String()
	_, attAfter, _ := p9HTTP(t, http.MethodGet, promptURL2+"/compression-attempts", "")
	_, linAfter, _ := p9HTTP(t, http.MethodGet, base2+"/worker-prompts/"+wpID+"/lineage", "")
	_, metAfter, _ := p9HTTP(t, http.MethodGet, base2+"/compression-metrics", "")
	if attAfter != attBefore {
		t.Fatal("attempt history differs after restart")
	}
	if linAfter != linBefore {
		t.Fatal("worker prompt / hashes / accepted attempt differ after restart")
	}
	if metAfter != metBefore {
		t.Fatal("metrics not reconstructable after restart")
	}
	if p9Lineage(t, h2, rec.ID.String()) != rowsBefore {
		t.Fatal("persisted lineage rows differ after restart")
	}
	calls := srv.Calls()
	_, _, body = p9HTTP(t, http.MethodPost, promptURL2+"/worker-prompt", "")
	if body["reused"] != true || srv.Calls() != calls || body["worker_prompt"].(map[string]any)["id"] != wpID {
		t.Fatal("restart lost the accepted result (re-compressed)")
	}
}

// WBS 9.59 — Context Snapshot → Canonical Prompt → Attempt 1 (FAILED) →
// Attempt 2 (PASS) → Compressed Prompt → Worker Prompt: every edge is
// queryable and no artifact is orphaned or references the wrong source.
func TestP970AuditLineage(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)
	srv := providertest.New(providertest.Reply{Edit: p9DropAC3}, providertest.Reply{})
	defer srv.Close()
	svc := p9Service(h, p9FakeFactory(srv, ""))
	p9Configure(t, h, svc, pos.ProjectID, n*4, p9Settings(n/2, 2))
	res, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A second canonical prompt with its own run, to prove no cross-talk.
	rec2 := p9Compose(t, h, pos)
	if _, err := svc.PrepareWorkerPrompt(ctx, pos.ProjectID, rec2.ID); err != nil {
		t.Fatal(err)
	}

	l, err := svc.Lineage(ctx, pos.ProjectID, res.WorkerPrompt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if l.ContextSnapshotID != pos.SnapID || l.Canonical.ID != rec.ID {
		t.Fatal("T9.34.1/T9.34.2 worker → canonical → snapshot")
	}
	if len(l.Attempts) != 2 || l.Attempts[0].Status != compression.AttemptValidationFailed || l.AcceptedAttempt == nil || l.AcceptedAttempt.Number != 2 {
		t.Fatal("T9.34.3/T9.34.4 attempts + accepted attempt")
	}
	if l.CompressedPrompt == nil || l.CompressedPrompt.AttemptID != l.AcceptedAttempt.ID || l.CompressedPrompt.CanonicalPromptID != rec.ID {
		t.Fatal("accepted attempt → compressed prompt edge")
	}
	if l.WorkerPrompt.CompressedPromptID == nil || *l.WorkerPrompt.CompressedPromptID != l.CompressedPrompt.ID {
		t.Fatal("compressed → worker edge")
	}
	// Every edge also queryable by SQL.
	var snap string
	if err := h.DBScan(ctx, `SELECT c.context_snapshot_id::text FROM worker_prompts w
		JOIN compressed_prompts cp ON cp.id = w.compressed_prompt_id
		JOIN compression_attempts a ON a.id = cp.attempt_id
		JOIN compression_runs r ON r.id = a.run_id
		JOIN canonical_prompts c ON c.id = r.canonical_prompt_id
		JOIN context_snapshots s ON s.id = c.context_snapshot_id
		WHERE w.id = '`+res.WorkerPrompt.ID.String()+`'`, &snap); err != nil || snap != pos.SnapID.String() {
		t.Fatalf("SQL lineage walk: %v %s", err, snap)
	}
	if o := p9OrphanCount(t, h); o != 0 {
		t.Fatalf("%d orphaned / mis-sourced artifacts", o)
	}
	// Project isolation on lineage.
	other, _, _ := p705Rig(t, h)
	if _, err := svc.Lineage(ctx, other, res.WorkerPrompt.ID); err == nil {
		t.Fatal("lineage readable from a foreign project")
	}
}

// WBS 9.60 + Phase 9 DoD — end-to-end acceptance through the app's REST API
// and the real OpenAI-compatible adapter: an ~18,000-token canonical
// implementation prompt compressed to <= 8,000 with every execution-critical
// item preserved, full persistence, and identical reconstruction after a
// restart.
func TestP971Phase9EndToEndAcceptance(t *testing.T) {
	srv := providertest.New(providertest.Reply{Edit: p9DropAC3}, providertest.Reply{})
	defer srv.Close()
	h, _, boot := p9RestartRig(t, srv)
	ctx := context.Background()

	const archRule = "Domain packages never import infrastructure packages; adapters live behind interfaces."
	spec := p9DefaultSpec()
	spec.ProjectState = p9Background("PROJECT_STATE", 220)
	spec.Architecture = "ARCHITECTURE: layered.\n<preserve>" + archRule + "</preserve>\n" + p9Background("ARCHITECTURE", 200)
	spec.PhaseState = p9Background("PHASE_STATE", 60)
	spec.ActiveTask = p9ActiveTask + "\ndependencies: WU-9.10, WU-9.16"
	pos := p9Rig(t, h, spec)
	rec := p9Compose(t, h, pos)
	n := p9Tokens(rec)
	if n < 15000 || n > 21000 {
		t.Fatalf("fixture: canonical estimate %d not ~18,000", n)
	}
	if _, err := h.DBExec(ctx, "UPDATE project_settings SET compression_provider = 'fake', compression_model = 'compressor-7b', context_window = 64000 WHERE project_id = '"+pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	base := h.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	promptURL := base + "/prompts/" + rec.ID.String()
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/settings/compression", `{"mode":"AUTO","target_tokens":8000,"max_attempts":2,"minimum_savings":0.15}`); code != 200 {
		t.Fatalf("settings: %s", raw)
	}
	code, raw, body := p9HTTP(t, http.MethodPost, promptURL+"/worker-prompt", "")
	if code != 200 {
		t.Fatalf("prepare: %d %s", code, raw)
	}
	run := body["run"].(map[string]any)
	wp := body["worker_prompt"].(map[string]any)
	content := wp["content"].(string)
	if run["outcome"] != "COMPRESSED" || wp["source"] != "COMPRESSED" {
		t.Fatalf("not compressed: %s", raw)
	}
	if tok := wp["estimated_tokens"].(float64); tok > 8000 {
		t.Fatalf("compressed %v > acceptable 8,000", tok)
	}
	p9MustContainAll(t, "e2e worker prompt", content,
		"name: 9.17", pos.UnitID.String(), // Work Unit 9.17
		p9AC1, p9AC2, p9AC3, p9AC4, // AC-1..AC-4
		p9T1, p9T2, p9T3, // TEST-1..TEST-3
		p9C1, p9C2, // CONSTRAINT-1..2
		"WU-9.10", "WU-9.16", // dependency identifiers
		p9Protocol,                                                                      // response protocol
		archRule,                                                                        // architecture semantics (<preserve>)
		"Implement only the active work unit.", "Do not skip or weaken required tests.", // role + state instructions
		p9StateInstr,
	)
	atts := body["attempts"].([]any)
	if len(atts) != 2 || atts[0].(map[string]any)["status"] != "VALIDATION_FAILED" || atts[1].(map[string]any)["accepted"] != true {
		t.Fatalf("attempt history: %s", raw)
	}
	val := atts[1].(map[string]any)["validation"].(map[string]any)
	sem := val["semantic"].(map[string]any)
	if sem["passed"] != true || sem["added_requirements"] != nil {
		t.Fatal("new implementation requirements introduced")
	}
	// The provider saw a retry built from the canonical source with the
	// AC-3 feedback, authenticated with the configured key.
	reqs := srv.Requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1].User, "AC-3 missing") || reqs[1].Authorization != "Bearer "+p9SentinelKey ||
		reqs[0].System != compression.DefaultInstructionContent {
		t.Fatal("provider requests")
	}

	// Persistence checklist.
	repo := dbrepo.NewCompressionRepository(h.App.DB().Raw())
	runs, _ := repo.ListRunsByCanonical(ctx, rec.ID)
	r := runs[0]
	if len(runs) != 1 || r.ConfigHash == "" || r.InstructionVersion != 1 || r.CanonicalTokens != n || r.TargetTokens != 8000 || r.RequestIdentity == "" {
		t.Fatalf("run checklist %+v", r)
	}
	for _, q := range []string{
		"SELECT count(*) FROM canonical_prompts WHERE id = '" + rec.ID.String() + "'",
		"SELECT count(*) FROM project_compression_settings WHERE project_id = '" + pos.ProjectID.String() + "' AND target_tokens = 8000",
		"SELECT count(*) FROM compression_instructions WHERE version = 1",
		"SELECT count(*) FROM compressed_prompts WHERE canonical_prompt_id = '" + rec.ID.String() + "' AND validation_status = 'PASSED'",
		"SELECT count(*) FROM worker_prompts WHERE canonical_prompt_id = '" + rec.ID.String() + "' AND source = 'COMPRESSED'",
	} {
		var c int
		if err := h.DBScan(ctx, q, &c); err != nil || c != 1 {
			t.Fatalf("persistence checklist %q: %d %v", q, c, err)
		}
	}
	if p9Count(t, h, "compression_attempts", "canonical_prompt_id = '"+rec.ID.String()+"' AND validation IS NOT NULL") != 2 {
		t.Fatal("validation results not persisted for every attempt")
	}
	if o := p9OrphanCount(t, h); o != 0 {
		t.Fatalf("%d orphaned artifacts", o)
	}
	if wp["hash"] != compression.WorkerPromptHash(content, compression.SourceCompressed) {
		t.Fatal("worker hash")
	}
	_, provBefore, _ := p9HTTP(t, http.MethodGet, promptURL+"/provenance", "")
	_, linBefore, _ := p9HTTP(t, http.MethodGet, base+"/worker-prompts/"+wp["id"].(string)+"/lineage", "")
	_, metBefore, mb := p9HTTP(t, http.MethodGet, base+"/compression-metrics", "")
	if mb["prompts_compressed"] != float64(1) || mb["average_reduction_percent"].(float64) < 50 {
		t.Fatalf("token metrics: %s", metBefore)
	}
	rowsBefore := p9Lineage(t, h, rec.ID.String())

	// Restart and reconstruct the entire lineage identically.
	h.Close()
	h2 := boot()
	base2 := h2.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	_, provAfter, _ := p9HTTP(t, http.MethodGet, base2+"/prompts/"+rec.ID.String()+"/provenance", "")
	_, linAfter, _ := p9HTTP(t, http.MethodGet, base2+"/worker-prompts/"+wp["id"].(string)+"/lineage", "")
	_, metAfter, _ := p9HTTP(t, http.MethodGet, base2+"/compression-metrics", "")
	if provAfter != provBefore || linAfter != linBefore || metAfter != metBefore || p9Lineage(t, h2, rec.ID.String()) != rowsBefore {
		t.Fatal("lineage not reconstructed identically after restart")
	}
	var lin map[string]any
	_ = json.Unmarshal([]byte(linAfter), &lin)
	if lin["worker_prompt"].(map[string]any)["content"] != content || lin["context_snapshot_id"] != pos.SnapID.String() {
		t.Fatal("restored worker prompt / snapshot edge")
	}
}
