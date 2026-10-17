package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"overseer/internal/cache"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/dbrepo"
	"overseer/internal/precompute"
	"overseer/internal/service"
)

// P11-54 (WBS 11.86): Phase 11 end-to-end acceptance. Project with 11.1/
// 11.2 DONE, 11.3 EXECUTING under IMPLEMENT/ENGINEER, 11.4 READY, 11.5
// depending on 11.4; precompute enabled with depth 2, 8 candidates,
// concurrency 2. Dispatching IMPLEMENT 11.3 warms CONTINUE/VALIDATE/FIX
// fragments and the IMPLEMENT 11.4 worker prompt; the real transition hits
// it; an ARCHITECTURE change forces safe recomputation; a restart retains
// history, cached prompts, manifests and metrics, recovers a stale build
// lease and leaves the workflow position unchanged.
func TestP1186EndToEndAcceptance(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	h, _, boot := p9RestartRig(t, srv)
	r := p11Build(t, h, srv, true)
	ctx := context.Background()
	pid := r.pos.ProjectID.String()
	p9HTTP(t, http.MethodPut, r.base+"/settings/precompute", `{"enabled":true,"depth":2,"max_candidates":8,"concurrency":2,"compression_enabled":true,"static_fragments":true,"during_worker_execution":true,"resource_policy":"ALWAYS"}`)
	st, _, err := r.svc().PrecomputeSettings(ctx, r.pos.ProjectID)
	if err != nil || !st.Enabled || st.Depth != 2 || st.MaxCandidates != 8 || st.Concurrency != 2 {
		t.Fatalf("configured settings %+v %v", st, err)
	}

	// Dispatch IMPLEMENT 11.3: asynchronous warm-up.
	job := r.svc().NotifyWorkerDispatched(ctx, r.pos.ProjectID)
	if job == nil {
		t.Fatal("dispatch did not start a precompute job")
	}
	r.svc().Wait()
	cands := r.candidates(t, job.ID)
	want := map[string]struct {
		readiness precompute.Readiness
		result    precompute.CandidateResult
		missing   []precompute.DynamicInputType
	}{
		"CONTINUE":       {precompute.PartiallyKnown, precompute.ResultFragment, []precompute.DynamicInputType{precompute.InputWorkerStatus}},
		"VALIDATE":       {precompute.PartiallyKnown, precompute.ResultFragment, []precompute.DynamicInputType{precompute.InputImplementationResult, precompute.InputTestResults}},
		"FIX":            {precompute.PartiallyKnown, precompute.ResultFragment, []precompute.DynamicInputType{precompute.InputValidationFailures}},
		"IMPLEMENT+next": {precompute.FullyKnown, precompute.ResultBuilt, nil},
	}
	for k, w := range want {
		c, ok := cands[k]
		if !ok || c.Readiness != w.readiness || c.Result != w.result || fmt.Sprint(c.Missing) != fmt.Sprint(w.missing) {
			t.Fatalf("11.86 candidate %s = %+v", k, c)
		}
	}
	next := cands["IMPLEMENT+next"]
	if *next.WorkUnitID != r.units["11.4"] || next.CacheEntryID == nil {
		t.Fatalf("IMPLEMENT 11.4 not cached: %+v", next)
	}
	// Static fragments cached (one manifest per fragment candidate).
	if n := p9Count(t, h, "fragment_manifests", "project_id = '"+pid+"'"); n < 3 {
		t.Fatalf("fragment manifests %d", n)
	}
	for _, c := range cands {
		if c.WorkUnitID != nil && *c.WorkUnitID == r.units["11.5"] {
			t.Fatal("dependency-blocked 11.5 precomputed")
		}
	}

	// 11.3 completes, validation passes; the normal machine selects 11.4.
	calls := srv.Calls()
	r.moveTo(t, "VALIDATE", r.units["11.3"])
	r.markDone(t, r.units["11.3"])
	r.moveTo(t, "IMPLEMENT", r.units["11.4"])
	received := time.Now().UTC()
	hit := r.prepareNext(t, service.NextRequest{ResultReceivedAt: &received})
	if !hit.Hit || hit.Entry == nil || hit.Entry.ID != *next.CacheEntryID || srv.Calls() != calls {
		t.Fatalf("11.86 CACHE HIT expected: hit=%v entry=%+v", hit.Hit, hit.Entry)
	}

	// ARCHITECTURE changes: the precomputed prompt no longer matches.
	(&p10Env{h: h, pos: r.pos}).writeFile(t, "ARCHITECTURE", "ARCHITECTURE v2\n"+p9Background("ARCHITECTURE-E2E", 120))
	re := r.prepareNext(t, service.NextRequest{})
	if re.Hit || re.CacheKey == hit.CacheKey || re.WorkerPrompt == nil || re.WorkerPrompt.ID == hit.WorkerPrompt.ID {
		t.Fatal("11.86 stale precomputed prompt reused after ARCHITECTURE change")
	}
	if !strings.Contains(re.WorkerPrompt.Content, r.units["11.4"].String()) {
		t.Fatal("recomputed prompt is not 11.4's")
	}

	// A crashed builder leaves a stale BUILDING lease on the current key.
	repo := dbrepo.NewPromptCacheRepository(h.App.DB().Raw())
	if ok, err := repo.MarkInvalid(ctx, re.Entry.ID, "e2e: simulate rebuild"); err != nil || !ok {
		t.Fatalf("invalidate: %v", err)
	}
	stale, _ := cache.NewBuildingEntry(r.pos.ProjectID, re.CacheKey, cache.LayerWorker, "crashed-instance", time.Minute, time.Now().Add(-2*time.Minute))
	if _, created, err := repo.Insert(ctx, &stale); err != nil || !created {
		t.Fatalf("seed stale lease: %v", err)
	}

	jobsBefore := p10Rows(t, h, "precompute_jobs", "project_id = '"+pid+"'")
	candsBefore := p10Rows(t, h, "precompute_candidates", "project_id = '"+pid+"'")
	manifestsBefore := p10Rows(t, h, "fragment_manifests", "project_id = '"+pid+"'")
	entriesBefore := p9Count(t, h, "prompt_cache_entries", "project_id = '"+pid+"' AND status = 'VALID'")
	metricsBefore, _ := r.svc().Metrics(ctx, r.pos.ProjectID)
	runtimeBefore := p9RuntimeRow(t, h, r.pos.ProjectID)

	h.Close()
	h2 := boot()
	r.h = h2
	if p10Rows(t, h2, "precompute_jobs", "project_id = '"+pid+"'") != jobsBefore ||
		p10Rows(t, h2, "precompute_candidates", "project_id = '"+pid+"'") != candsBefore {
		t.Fatal("11.86 precompute history not retained")
	}
	if p10Rows(t, h2, "fragment_manifests", "project_id = '"+pid+"'") != manifestsBefore {
		t.Fatal("11.86 fragment manifests not retained")
	}
	if p9Count(t, h2, "prompt_cache_entries", "project_id = '"+pid+"' AND status = 'VALID'") != entriesBefore {
		t.Fatal("11.86 cached prompts not retained")
	}
	if e, err := h2.App.PromptCache().GetEntry(ctx, r.pos.ProjectID, *next.CacheEntryID); err != nil || e.Status != cache.StatusValid {
		t.Fatal("11.86 precomputed entry lost across restart")
	}
	if m, _ := r.svc().Metrics(ctx, r.pos.ProjectID); fmt.Sprintf("%+v", m) != fmt.Sprintf("%+v", metricsBefore) {
		t.Fatalf("11.86 usage metrics changed by restart:\n%+v\n%+v", metricsBefore, m)
	}
	if p9RuntimeRow(t, h2, r.pos.ProjectID) != runtimeBefore {
		t.Fatal("11.86 runtime workflow position changed by restart")
	}
	rec := r.prepareNext(t, service.NextRequest{})
	if rec.Entry == nil || rec.Entry.ID != stale.ID || rec.Entry.Status != cache.StatusValid {
		t.Fatalf("11.86 stale build lease not recovered: %+v", rec.Entry)
	}
	if n := p9Count(t, h2, "prompt_cache_entries", "project_id = '"+pid+"' AND status = 'BUILDING'"); n != 0 {
		t.Fatal("stranded build lease after recovery")
	}
}
