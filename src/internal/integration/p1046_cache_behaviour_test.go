package integration

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/cache"
	"overseer/internal/compression"
	"overseer/internal/compression/compressiontest"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

// P10-46 (WBS 10.20/10.24/10.49): planning prompt — MISS then HIT without
// a compression call; WBS v3→v4 misses (v3 entry historical); planning
// policy change misses; another project is unaffected; implementation
// prompts ignore planning-policy changes.
func TestP1046PlanningPromptCache(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, snapID, templateID := p843Rig(t, h)
	p843SetPlanningSettings(t, h, projectID, p843AtomicPolicy, "atomic")
	p9GrowFile(t, h, projectID, "PROJECT_STATE", "PROJECT_STATE pinned content\n"+p9Background("PROJECT_STATE", 150))
	snap, err := p718Svc(t, h).CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		t.Fatal(err)
	}
	snapID = snap.ID
	sc := compressiontest.NewScripted()
	cacheSvc, comp := p10Service(h, p9Source{compressor: sc})
	p9Configure(t, h, comp, projectID, 64000, p9Settings(2000, 2))
	req := service.PrepareRequest{SnapshotID: snapID, TemplateID: templateID, TemplateVersion: 1}
	prep := func() *service.PrepareResult {
		r, err := cacheSvc.PrepareForContext(ctx, projectID, req)
		if err != nil {
			t.Fatalf("prepare planning: %v", err)
		}
		return r
	}
	first := prep()
	second := prep()
	if first.Hit || !second.Hit || sc.Calls() != 1 || first.WorkerPrompt.Source != compression.SourceCompressed {
		t.Fatalf("T10.20.1/10.49: hit1=%v hit2=%v calls=%d", first.Hit, second.Hit, sc.Calls())
	}
	var phaseID uuid.UUID
	if err := h.DBScan(ctx, "SELECT phase_id FROM context_snapshots WHERE id = '"+snapID.String()+"'", &phaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := dbrepo.NewWBSVersionRepository(h.App.DB().Raw()).AppendVersion(ctx, phaseID.String(), p843WBSv3Content+"\nPlan v4 unit 4: hardening."); err != nil {
		t.Fatal(err)
	}
	v4 := prep()
	if v4.Hit || v4.CacheKey == first.CacheKey || sc.Calls() != 2 {
		t.Fatal("T10.20.2 WBS v4 did not miss")
	}
	if old, _ := dbrepo.NewPromptCacheRepository(h.App.DB().Raw()).Get(ctx, first.Entry.ID); old.Status != cache.StatusValid {
		t.Fatal("T10.20.3 historical v3 entry not queryable")
	}
	p843SetPlanningSettings(t, h, projectID, "COMPONENT: decompose into components", "component")
	pol := prep()
	if pol.Hit || pol.CacheKey == v4.CacheKey {
		t.Fatal("T10.24.1/T10.24.2 planning policy change did not miss")
	}

	// An implementation prompt in another project: unaffected by this
	// project's WBS changes and by its own planning-policy changes.
	e := p10Setup(t, h, p9DefaultSpec())
	impl := e.prepare(t, e.req(e.pos.SnapID))
	p843SetPlanningSettings(t, h, e.pos.ProjectID, "COMPONENT", "component")
	if again := e.prepare(t, e.req(e.pos.SnapID)); !again.Hit || again.CacheKey != impl.CacheKey {
		t.Fatal("T10.24.3 implementation prompt invalidated by a planning-policy change")
	}
	if _, err := dbrepo.NewWBSVersionRepository(h.App.DB().Raw()).AppendVersion(ctx, phaseID.String(), "wbs v5"); err != nil {
		t.Fatal(err)
	}
	if again := e.prepare(t, e.req(e.pos.SnapID)); !again.Hit {
		t.Fatal("T10.20.4 unrelated project affected")
	}
}

// P10-29/P10-30/P10-54 (WBS 10.29/10.30/10.32): concurrent identical misses
// in one instance make one compression call; two instances (distinct lease
// owners) racing the same key leave one authoritative entry; an expired
// BUILDING lease is reclaimed; a wrong owner cannot finalize.
func TestP1029ConcurrencyAndLeases(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var mu sync.Mutex
	calls := 0
	slow := compressiontest.Step(func(ctx context.Context, req compression.CompressionRequest) (compression.CompressionResponse, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(200 * time.Millisecond)
		return compressiontest.Heuristic(ctx, req)
	})
	pos := p9Rig(t, h, p9DefaultSpec())
	src := p9Source{compressor: compressiontest.NewScripted(slow)}
	a, comp := p10Service(h, src)
	b, _ := p10Service(h, src)
	p9Configure(t, h, comp, pos.ProjectID, 64000, p9Settings(4000, 2))
	req := service.PrepareRequest{SnapshotID: pos.SnapID, TemplateID: pos.TemplateID, TemplateVersion: 1}

	var wg sync.WaitGroup
	results := make([]*service.PrepareResult, 6)
	errs := make([]error, 6)
	for i := range results {
		svc := a
		if i%2 == 1 {
			svc = b // second "instance" (own owner id, own single-flight)
		}
		wg.Add(1)
		go func(i int, svc *service.PromptCacheService) {
			defer wg.Done()
			results[i], errs[i] = svc.PrepareForContext(ctx, pos.ProjectID, req)
		}(i, svc)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].WorkerPrompt == nil {
			t.Fatalf("T10.29.2 caller %d: %v", i, errs[i])
		}
		if results[i].WorkerPrompt.ID != results[0].WorkerPrompt.ID {
			t.Fatal("T10.30.3 inconsistent worker prompt association")
		}
	}
	if calls != 1 {
		t.Fatalf("T10.29.1/T10.30.1 compression calls = %d", calls)
	}
	where := "project_id = '" + pos.ProjectID.String() + "'"
	if p9Count(t, h, "prompt_cache_entries", where+" AND status = 'VALID'") != 1 || p9Count(t, h, "prompt_cache_entries", where+" AND status = 'BUILDING'") != 0 {
		t.Fatal("T10.30.1 duplicate or stranded entries")
	}

	// Stale lease: a BUILDING entry whose lease expired (crashed builder)
	// is reclaimed; a different key is unaffected by it.
	e2 := p10Setup(t, h, p9DefaultSpec())
	repo := dbrepo.NewPromptCacheRepository(h.App.DB().Raw())
	key := p10KeyOf(t, e2)
	stale, _ := cache.NewBuildingEntry(e2.pos.ProjectID, key, cache.LayerWorker, "crashed-instance", time.Minute, time.Now().Add(-2*time.Minute))
	if _, created, err := repo.Insert(ctx, &stale); err != nil || !created {
		t.Fatalf("seed stale lease: %v", err)
	}
	if ok, _ := repo.FinalizeValid(ctx, stale.ID, "someone-else", time.Now(), persistence.BuildResult{}); ok {
		t.Fatal("T10.32.3 wrong owner finalized")
	}
	res := e2.prepare(t, e2.req(e2.pos.SnapID))
	if res.Entry == nil || res.Entry.ID != stale.ID || res.Entry.Status != cache.StatusValid {
		t.Fatalf("T10.32.2/T10.32.4 expired lease not reclaimed: %+v", res.Entry)
	}

	// Active foreign lease: the contender waits for it, then reclaims after
	// expiry (never stranded, never duplicated).
	e3 := p10Setup(t, h, p9DefaultSpec())
	key3 := p10KeyOf(t, e3)
	held, _ := cache.NewBuildingEntry(e3.pos.ProjectID, key3, cache.LayerWorker, "busy-instance", 600*time.Millisecond, time.Now())
	if _, created, err := repo.Insert(ctx, &held); err != nil || !created {
		t.Fatal("seed active lease")
	}
	start := time.Now()
	r3 := e3.prepare(t, e3.req(e3.pos.SnapID))
	if time.Since(start) < 400*time.Millisecond || r3.Entry == nil || r3.Entry.ID != held.ID {
		t.Fatalf("T10.32.1 active lease not respected (waited %v)", time.Since(start))
	}
}

// p10KeyOf derives the cache key of the env's base request (explain is
// read-only).
func p10KeyOf(t *testing.T, e *p10Env) string {
	t.Helper()
	ex, err := e.cache.Explain(context.Background(), e.pos.ProjectID, e.req(e.pos.SnapID))
	if err != nil {
		t.Fatal(err)
	}
	return ex.CacheKey
}

// P10-31 (WBS 10.33/10.34): a failed preparation leaves a FAILED entry
// (never a hit); a transient provider failure opens the negative window
// (no provider call while it lasts); expiry allows a retry; a configuration
// change bypasses the obsolete window; a later success supersedes.
func TestP1033FailureAndNegativeCache(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	srv := providertest.New(providertest.Reply{Status: http.StatusServiceUnavailable, Body: `{"error":"down"}`})
	defer srv.Close()
	pos := p9Rig(t, h, p9DefaultSpec())
	cacheSvc, comp := p10Service(h, p9FakeFactory(srv, ""))
	clock := time.Now().UTC()
	cacheSvc.WithClock(func() time.Time { return clock })
	// Canonical (~10k) cannot fit an 8k context → no canonical fallback.
	p9Configure(t, h, comp, pos.ProjectID, 8000, p9Settings(4000, 2))
	req := service.PrepareRequest{SnapshotID: pos.SnapID, TemplateID: pos.TemplateID, TemplateVersion: 1}

	_, err := cacheSvc.PrepareForContext(ctx, pos.ProjectID, req)
	if !errors.Is(err, compression.ErrPromptPreparationFailed) || srv.Calls() != 2 {
		t.Fatalf("expected preparation failure after 2 calls, got %v (%d calls)", err, srv.Calls())
	}
	where := "project_id = '" + pos.ProjectID.String() + "'"
	if p9Count(t, h, "prompt_cache_entries", where+" AND status = 'FAILED' AND error_kind = 'UNAVAILABLE' AND retry_after IS NOT NULL") != 1 {
		t.Fatal("T10.33.2 failed history not retained")
	}
	_, err = cacheSvc.PrepareForContext(ctx, pos.ProjectID, req)
	if !errors.Is(err, service.ErrCacheSuppressed) || srv.Calls() != 2 {
		t.Fatalf("T10.34.1 window did not suppress: %v calls=%d", err, srv.Calls())
	}
	clock = clock.Add(31 * time.Second)
	if _, err = cacheSvc.PrepareForContext(ctx, pos.ProjectID, req); errors.Is(err, service.ErrCacheSuppressed) || srv.Calls() != 4 {
		t.Fatalf("T10.34.2 expired window did not retry: %v calls=%d", err, srv.Calls())
	}
	// A configuration change is a different key: not suppressed.
	if _, err := h.DBExec(ctx, "UPDATE project_settings SET compression_model = 'model-C' WHERE project_id = '"+pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err = cacheSvc.PrepareForContext(ctx, pos.ProjectID, req); errors.Is(err, service.ErrCacheSuppressed) || srv.Calls() != 6 {
		t.Fatalf("T10.34.3 config change suppressed: %v calls=%d", err, srv.Calls())
	}
	// Provider recovers: a new valid preparation supersedes the FAILED state.
	ok := providertest.New()
	defer ok.Close()
	cacheSvc2, _ := p10Service(h, p9FakeFactory(ok, ""))
	cacheSvc2.WithClock(func() time.Time { return clock.Add(time.Hour) })
	res, err := cacheSvc2.PrepareForContext(ctx, pos.ProjectID, req)
	if err != nil || res.Entry == nil || res.Entry.Status != cache.StatusValid {
		t.Fatalf("T10.33.3 success did not supersede: %v", err)
	}
	if p9Count(t, h, "prompt_cache_entries", where+" AND status = 'FAILED'") < 2 {
		t.Fatal("failed history lost")
	}
	if hit, _ := cacheSvc2.PrepareForContext(ctx, pos.ProjectID, req); !hit.Hit || hit.WorkerPrompt.ID != res.WorkerPrompt.ID {
		t.Fatal("T10.33.1 FAILED entry shadowed the valid one")
	}
}

// failingEntries wraps the entry repository and fails lookups.
type failingEntries struct {
	persistence.PromptCacheRepository
}

func (f failingEntries) GetValid(ctx context.Context, p uuid.UUID, k string, l cache.Layer) (*cache.PromptCacheEntry, error) {
	return nil, &persistence.InfrastructureError{Op: "simulated cache outage", Cause: errors.New("connection refused")}
}

// P10-33 (WBS 10.69): a recoverable cache-layer failure bypasses the cache
// (recorded) and prepares normally; with bypass disabled it fails clearly.
func TestP1069CacheFailureIsolation(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	e := p10Setup(t, h, p9DefaultSpec())
	e.cache.WithEntries(failingEntries{dbrepo.NewPromptCacheRepository(h.App.DB().Raw())})
	res, err := e.cache.PrepareForContext(ctx, e.pos.ProjectID, e.req(e.pos.SnapID))
	if err != nil || !res.Bypassed || res.WorkerPrompt == nil || e.sc.Calls() != 1 {
		t.Fatalf("T10.69.1 bypass: %v %+v", err, res)
	}
	if e.events(t, "CACHE_BYPASS") != 1 || e.count(t, "prompt_cache_entries", "") != 0 {
		t.Fatal("T10.69.3 bypass not recorded / cache written while bypassing")
	}
	st, _, _ := e.cache.CacheSettings(ctx, e.pos.ProjectID)
	st.BypassOnCacheError = false
	if _, err := e.cache.UpdateCacheSettings(ctx, e.pos.ProjectID, st); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cache.PrepareForContext(ctx, e.pos.ProjectID, e.req(e.pos.SnapID)); !errors.Is(err, service.ErrCacheUnavailable) {
		t.Fatalf("bypass disabled: %v", err)
	}
}
