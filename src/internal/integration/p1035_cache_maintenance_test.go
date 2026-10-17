package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/cache"
	"overseer/internal/dbrepo"
)

// P10-35/P10-36/P10-37 + WBS 10.17/10.28: explicit and project-wide
// invalidation, collision protection, integrity scan, retention + GC.
func TestP1035InvalidationIntegrityGC(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	repo := dbrepo.NewPromptCacheRepository(h.App.DB().Raw())
	e := p10Setup(t, h, p9DefaultSpec())
	first := e.prepare(t, e.req(e.pos.SnapID))

	// T10.41.1-T10.41.4: explicit invalidation.
	inv, err := e.cache.Invalidate(ctx, e.pos.ProjectID, first.Entry.ID, "bad compression result")
	if err != nil || inv.Status != cache.StatusInvalidated || inv.InvalidationReason != "bad compression result" {
		t.Fatalf("invalidate: %v %+v", err, inv)
	}
	second := e.prepare(t, e.req(e.pos.SnapID))
	if second.Hit || second.Entry.ID == first.Entry.ID || e.sc.Calls() != 2 {
		t.Fatal("T10.41.3/T10.18.3 invalidated entry reused")
	}
	if p9Count(t, h, "worker_prompts", "id = '"+first.WorkerPrompt.ID.String()+"'") != 1 {
		t.Fatal("T10.41.4/T10.18.4 artifact history lost")
	}

	// T10.28.2/T10.28.3: same key, mismatched stored metadata → corruption,
	// never dispatched, quarantined.
	if _, err := h.DBExec(ctx, `UPDATE prompt_cache_entries SET metadata = jsonb_set(metadata, '{inputs,1,hash}', '"tampered"') WHERE id = '`+second.Entry.ID.String()+`'`); err != nil {
		t.Fatal(err)
	}
	third := e.prepare(t, e.req(e.pos.SnapID))
	if third.Hit || third.WorkerPrompt.ID == second.WorkerPrompt.ID && third.Entry.ID == second.Entry.ID {
		t.Fatal("T10.28.3 corrupt entry dispatched")
	}
	got, _ := repo.Get(ctx, second.Entry.ID)
	if got.Status != cache.StatusInvalidated || !strings.HasPrefix(got.InvalidationReason, cache.ReasonCorrupt) || e.events(t, "CACHE_CORRUPT") != 1 {
		t.Fatalf("T10.28.2 corrupt entry not quarantined: %+v", got)
	}

	// Integrity scan: healthy passes; a hash mismatch and a cross-project
	// reference are detected and quarantined (T10.58.1-T10.58.4, T10.17.3/4).
	rep, err := e.cache.IntegrityScan(ctx, e.pos.ProjectID)
	if err != nil || rep.Scanned != 1 || rep.Healthy != 1 {
		t.Fatalf("T10.58.1 healthy scan: %v %+v", err, rep)
	}
	for _, q := range []string{
		"ALTER TABLE worker_prompts DISABLE TRIGGER worker_prompts_immutable",
		"UPDATE worker_prompts SET hash = 'tampered' WHERE id = '" + third.WorkerPrompt.ID.String() + "'",
		"ALTER TABLE worker_prompts ENABLE TRIGGER worker_prompts_immutable",
	} {
		if _, err := h.DBExec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	rep, _ = e.cache.IntegrityScan(ctx, e.pos.ProjectID)
	if len(rep.Corrupt) != 1 || rep.Corrupt[0].Reason != "worker prompt hash mismatch" {
		t.Fatalf("T10.58.3 %+v", rep)
	}
	if rep2, _ := e.cache.IntegrityScan(ctx, e.pos.ProjectID); rep2.Scanned != 0 {
		t.Fatal("T10.58.4 corrupt entry still VALID after scan")
	}
	fourth := e.prepare(t, e.req(e.pos.SnapID))
	other := p10Setup(t, h, p9DefaultSpec())
	foreign := other.prepare(t, other.req(other.pos.SnapID))
	for _, q := range []string{
		"ALTER TABLE prompt_cache_entries DISABLE TRIGGER prompt_cache_entries_guard",
		"UPDATE prompt_cache_entries SET worker_prompt_id = '" + foreign.WorkerPrompt.ID.String() + "', canonical_prompt_id = '" + foreign.CanonicalPromptID.String() + "' WHERE id = '" + fourth.Entry.ID.String() + "'",
		"ALTER TABLE prompt_cache_entries ENABLE TRIGGER prompt_cache_entries_guard",
	} {
		if _, err := h.DBExec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if res := e.prepare(t, e.req(e.pos.SnapID)); res.Hit || res.WorkerPrompt.ID == foreign.WorkerPrompt.ID {
		t.Fatal("T10.17.4/T10.63 cross-project reference served")
	}

	// T10.42.1-T10.42.3: project-wide invalidation.
	n, err := e.cache.InvalidateProject(ctx, e.pos.ProjectID, "systematic bug")
	if err != nil || n != 1 {
		t.Fatalf("project invalidation: %v %d", err, n)
	}
	if p9Count(t, h, "prompt_cache_entries", "project_id = '"+other.pos.ProjectID.String()+"' AND status = 'VALID'") != 1 {
		t.Fatal("T10.42.2 other project affected")
	}
	if p9Count(t, h, "canonical_prompts", "project_id = '"+e.pos.ProjectID.String()+"'") == 0 {
		t.Fatal("T10.42.3 historical prompts gone")
	}

	// Retention + GC (T10.35/T10.36): RETAIN_ALL deletes nothing; latest-1
	// per slot deletes the older history but keeps the newest VALID entry
	// and every artifact; GC is idempotent.
	if n, _ := e.cache.GarbageCollect(ctx, e.pos.ProjectID); n != 0 {
		t.Fatal("RETAIN_ALL deleted entries")
	}
	fresh := e.prepare(t, e.req(e.pos.SnapID))
	st, _, _ := e.cache.CacheSettings(ctx, e.pos.ProjectID)
	st.Retention = cache.Retention{Policy: cache.RetainLatest, Value: 1}
	if _, err := e.cache.UpdateCacheSettings(ctx, e.pos.ProjectID, st); err != nil {
		t.Fatal(err)
	}
	workers := p9Count(t, h, "worker_prompts", "project_id = '"+e.pos.ProjectID.String()+"'")
	deleted, err := e.cache.GarbageCollect(ctx, e.pos.ProjectID)
	if err != nil || deleted == 0 {
		t.Fatalf("T10.36.1 nothing collected: %v", err)
	}
	if left, _ := repo.Get(ctx, fresh.Entry.ID); left == nil || left.Status != cache.StatusValid {
		t.Fatal("T10.35.3 newest valid entry collected")
	}
	if p9Count(t, h, "worker_prompts", "project_id = '"+e.pos.ProjectID.String()+"'") != workers {
		t.Fatal("T10.36.2/T10.36.3 GC removed prompt artifacts")
	}
	if again, _ := e.cache.GarbageCollect(ctx, e.pos.ProjectID); again != 0 {
		t.Fatal("T10.36.4 GC not idempotent")
	}
	if hit := e.prepare(t, e.req(e.pos.SnapID)); !hit.Hit {
		t.Fatal("GC broke the live entry")
	}
}

// P10-52 (WBS 10.62/10.63): unit identity is part of cache identity; unit
// B never resolves unit A's entry; hits never mutate WBS or runtime state;
// project isolation holds for direct entry ids.
func TestP1062ProgressSafeAndIsolation(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	e := p10Setup(t, h, p9DefaultSpec())
	unitA := e.pos.UnitID
	a := e.prepare(t, e.req(e.pos.SnapID))

	unitB := p710UnitWithSpec(t, h, e.pos.PlanID, 2, "9.18", "PENDING", []string{p9AC1}, []string{p9T1}, []string{p9C1})
	fb := p710ActiveTaskFile(t, h, p708Service(t, h), e.pos.ProjectID, unitB)
	if _, err := p708Service(t, h).UpdateContent(ctx, fb.String(), p9ActiveTask, 0); err != nil {
		t.Fatal(err)
	}
	unitsBefore := p10Rows(t, h, "work_units", "execution_plan_id = '"+e.pos.PlanID.String()+"'")
	if hit := e.prepare(t, e.req(e.pos.SnapID)); !hit.Hit {
		t.Fatal("A no longer hits")
	}
	if p10Rows(t, h, "work_units", "execution_plan_id = '"+e.pos.PlanID.String()+"'") != unitsBefore {
		t.Fatal("T10.62.4 a cache hit mutated work-unit rows")
	}
	if _, err := h.DBExec(ctx, "UPDATE runtime_state SET work_unit_id = '"+unitB.String()+"' WHERE project_id = '"+e.pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	runtimeBefore := p9RuntimeRow(t, h, e.pos.ProjectID)
	b := e.prepare(t, e.req(e.snapshot(t)))
	if b.Hit || b.CacheKey == a.CacheKey || b.WorkerPrompt.ID == a.WorkerPrompt.ID || strings.Contains(b.WorkerPrompt.Content, unitA.String()) {
		t.Fatal("T10.62.1/T10.62.2 unit B resolved unit A's prompt")
	}
	if !strings.Contains(b.WorkerPrompt.Content, unitB.String()) || b.Entry.WorkUnitID == nil || *b.Entry.WorkUnitID != unitB {
		t.Fatal("unit B entry not bound to unit B")
	}
	if p9RuntimeRow(t, h, e.pos.ProjectID) != runtimeBefore {
		t.Fatal("T10.62.3 preparation changed runtime state")
	}
	other := p10Setup(t, h, p9DefaultSpec())
	if _, err := e.cache.GetEntry(ctx, e.pos.ProjectID, other.prepare(t, other.req(other.pos.SnapID)).Entry.ID); err == nil {
		t.Fatal("T10.63.2 direct entry id bypassed ownership")
	}
	if rs, _ := other.cache.PrepareForContext(ctx, other.pos.ProjectID, e.req(e.pos.SnapID)); rs != nil {
		t.Fatal("T10.26.1 project B prepared project A's snapshot")
	}
}

func p10Rows(t *testing.T, h *Harness, table, where string) string {
	t.Helper()
	var s string
	if err := h.DBScan(context.Background(), "SELECT coalesce(string_agg(row_to_json(x)::text, '|' ORDER BY x.id), '') FROM "+table+" x WHERE "+where, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// P10-57 (WBS 10.67/10.68): with ~1,000 entries the lookup is an index scan
// on (project_id, cache_key) that touches no prompt content; the unique
// authoritative key is enforced; migrations are idempotent.
func TestP1067IndexesAndPerformance(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	e := p10Setup(t, h, p9DefaultSpec())
	res := e.prepare(t, e.req(e.pos.SnapID))
	pid := e.pos.ProjectID.String()
	if _, err := h.DBExec(ctx, `INSERT INTO prompt_cache_entries (id, project_id, cache_key, layer, status, state_key, created_at, last_used_at)
		SELECT gen_random_uuid(), '`+pid+`', 'prompt-cache:v1:' || md5(i::text), 'WORKER', 'INVALIDATED', 'S' || (i % 100), now(), now()
		FROM generate_series(1, 1000) i`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx, "ANALYZE prompt_cache_entries"); err != nil {
		t.Fatal(err)
	}
	var plan string
	q := `EXPLAIN SELECT id, worker_prompt_id FROM prompt_cache_entries WHERE project_id = '` + pid + `' AND cache_key = '` + res.CacheKey + `' AND layer = 'WORKER' AND status = 'VALID'`
	conn := h.connect(ctx)
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var l string
		_ = rows.Scan(&l)
		plan += l + "\n"
	}
	rows.Close()
	if !strings.Contains(plan, "Index") || !strings.Contains(plan, "prompt_cache_entries_") {
		t.Fatalf("lookup is not an index scan:\n%s", plan)
	}
	if strings.Contains(plan, "content") {
		t.Fatal("lookup touches prompt content")
	}
	for _, idx := range []string{"prompt_cache_entries_authoritative_key", "prompt_cache_entries_project_status_idx", "prompt_cache_entries_project_last_used_idx",
		"prompt_cache_entries_canonical_idx", "prompt_cache_entries_worker_idx", "canonical_prompts_cache_key_idx"} {
		if p9Count(t, h, "pg_indexes", "indexname = '"+idx+"'") != 1 {
			t.Fatalf("T10.68.2 index %s missing", idx)
		}
	}
	// T10.68.1: a second authoritative row for the same key is rejected.
	if _, err := h.DBExec(ctx, `INSERT INTO prompt_cache_entries (id, project_id, cache_key, layer, status, worker_prompt_id, created_at, last_used_at)
		VALUES ('`+uuid.NewString()+`', '`+pid+`', '`+res.CacheKey+`', 'WORKER', 'VALID', '`+res.WorkerPrompt.ID.String()+`', now(), now())`); err == nil {
		t.Fatal("duplicate authoritative key accepted")
	}
	// T10.68.3: re-applying the Phase 10 migrations is a no-op.
	for _, f := range []string{"00054_prompt_cache_entries.up.sql", "00055_canonical_prompt_cache_key.up.sql", "00056_prompt_cache_events.up.sql",
		"00057_prompt_fragments.up.sql", "00058_project_prompt_cache_settings.up.sql"} {
		sql, err := os.ReadFile(filepath.Join(p10ModuleRoot(t), "migrations", f))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.DBExec(ctx, string(sql)); err != nil {
			t.Fatalf("re-apply %s: %v", f, err)
		}
	}
	if hit := e.prepare(t, e.req(e.pos.SnapID)); !hit.Hit {
		t.Fatal("cache broken after migration re-apply")
	}
}

func p10ModuleRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	for d := wd; d != "/"; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatal("module root not found")
	return ""
}
