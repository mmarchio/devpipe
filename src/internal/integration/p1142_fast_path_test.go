package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"overseer/internal/cache"
	"overseer/internal/compression"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/precompute"
	"overseer/internal/service"
	"overseer/internal/statefile"
)

// P11-42 (WBS 11.62/11.65/11.78/11.31): while IMPLEMENT 11.3 executes, the
// warm-up discovers CONTINUE / VALIDATE / FIX 11.3 (partial: worker status,
// implementation result + test output, validation failures) and IMPLEMENT
// 11.4 (fully known — its ACTIVE_TASK already exists here). After 11.3 is
// DONE and the normal machine moves to IMPLEMENT 11.4 — advancing the
// runtime version — the authoritative key equals the precomputed key
// (schema v2) and the worker prompt is served from the cache with no
// compression call.
func TestP1142FullyKnownNextUnitFastPath(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	r := p11NewRig(t, srv, true)
	before := r.workflowRows(t)

	res := r.warm(t, precompute.TriggerWorkerDispatch)
	if res.Status != precompute.JobCompleted || res.FullPrompts != 1 || res.Fragments != 3 {
		t.Fatalf("warm result %+v", res)
	}
	cands := r.candidates(t, res.JobID)
	for _, k := range []string{"CONTINUE", "VALIDATE", "FIX"} {
		c, ok := cands[k]
		if !ok || c.Readiness != precompute.PartiallyKnown || c.Result != precompute.ResultFragment || c.WorkUnitID == nil || *c.WorkUnitID != r.units["11.3"] {
			t.Fatalf("T11.61.1 %s candidate %+v", k, c)
		}
	}
	if m := cands["VALIDATE"].Missing; len(m) != 2 || m[0] != precompute.InputImplementationResult || m[1] != precompute.InputTestResults {
		t.Fatalf("T11.25.1/T11.29 validation awaits implementation + tests: %v", m)
	}
	if m := cands["CONTINUE"].Missing; len(m) != 1 || m[0] != precompute.InputWorkerStatus {
		t.Fatalf("T11.26 continuation awaits worker status: %v", m)
	}
	if m := cands["FIX"].Missing; len(m) != 1 || m[0] != precompute.InputValidationFailures || cands["FIX"].Depth != 2 {
		t.Fatalf("T11.30 fix awaits validation failures: %+v", cands["FIX"])
	}
	next := cands["IMPLEMENT+next"]
	if next.Readiness != precompute.FullyKnown || next.Result != precompute.ResultBuilt || *next.WorkUnitID != r.units["11.4"] || next.CacheEntryID == nil {
		t.Fatalf("T11.31.1 next unit %+v", next)
	}
	if done, ok := cands["DONE"]; !ok || done.Result != precompute.ResultSkipped || done.Readiness != precompute.Unknown {
		t.Fatalf("terminal state must be skipped as UNKNOWN: %+v", done)
	}
	if _, ok := cands["IMPLEMENT"]; ok {
		t.Fatal("the current state itself was a candidate")
	}
	for _, c := range cands {
		if c.WorkUnitID != nil && *c.WorkUnitID == r.units["11.5"] {
			t.Fatal("T11.17.2 dependency-blocked 11.5 became a candidate")
		}
	}
	if r.workflowRows(t) != before {
		t.Fatal("T11.11.4/T11.35/T11.75 precomputation changed workflow state")
	}
	calls := srv.Calls()

	// Worker completes 11.3, validation passes: normal machine marks 11.3
	// DONE and selects 11.4 (IMPLEMENT); runtime version advances.
	r.moveTo(t, "VALIDATE", r.units["11.3"])
	r.markDone(t, r.units["11.3"])
	r.moveTo(t, "IMPLEMENT", r.units["11.4"])
	received := time.Now().UTC()
	got := r.prepareNext(t, service.NextRequest{ResultReceivedAt: &received})
	if !got.Hit || got.Entry == nil || got.Entry.ID != *next.CacheEntryID || got.Entry.Source != cache.SourcePrecomputed {
		t.Fatalf("T11.62.1/T11.65.1 precomputed prompt not served: hit=%v entry=%+v", got.Hit, got.Entry)
	}
	if srv.Calls() != calls {
		t.Fatal("T11.62.2 compression call on the fast path")
	}
	if !strings.Contains(got.WorkerPrompt.Content, r.units["11.4"].String()) || strings.Contains(got.WorkerPrompt.Content, r.units["11.3"].String()) {
		t.Fatal("served prompt is not 11.4's")
	}
	var src string
	if err := r.h.DBScan(context.Background(), "SELECT source FROM transition_latencies WHERE project_id = '"+r.pos.ProjectID.String()+"' ORDER BY created_at DESC LIMIT 1", &src); err != nil || src != precompute.LatencyPrecomputed {
		t.Fatalf("T11.53.1/T11.53.2 latency source %q %v", src, err)
	}
	m, _ := r.svc().Metrics(context.Background(), r.pos.ProjectID)
	if m.Overall.Used < 1 {
		t.Fatalf("T11.51.2 used precompute not counted: %+v", m.Overall)
	}
}

// Decision 5 (user requirement) + WBS 11.84/11.86: 11.4 has no ACTIVE_TASK
// yet, so IMPLEMENT 11.4 is precomputed as a placeholder template (one
// compression call, never a worker prompt). When 11.4 actually starts, the
// real ACTIVE_TASK is calculated, the placeholder replaced with the real
// value, the result fully re-validated and cached — with no compression
// call. A later ARCHITECTURE change makes the cached prompt unusable.
func TestP1142PlaceholderNextUnitResolution(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	r := p11NewRig(t, srv, false)
	res := r.warm(t, precompute.TriggerWorkerDispatch)
	next := r.candidates(t, res.JobID)["IMPLEMENT+next"]
	if next.Result != precompute.ResultTemplate || next.TemplateID == nil || len(next.Missing) != 1 || next.Missing[0] != precompute.InputActiveTask {
		t.Fatalf("U11.P.6 placeholder template not built: %+v", next)
	}
	ctx := context.Background()
	pid := r.pos.ProjectID.String()
	if n := p9Count(t, r.h, "worker_prompts", "project_id = '"+pid+"' AND content LIKE '%"+compression.PlaceholderMarker+"%'"); n != 0 {
		t.Fatal("U11.P.7 a worker prompt carries a placeholder")
	}
	if n := p9Count(t, r.h, "compression_runs", "project_id = '"+pid+"' AND outcome = 'TEMPLATE'"); n != 1 {
		t.Fatalf("template compression run count %d", n)
	}
	if n := p9Count(t, r.h, "state_files", "work_unit_id = '"+r.units["11.4"].String()+"'"); n != 0 {
		t.Fatal("precompute created 11.4's ACTIVE_TASK (authoritative state written)")
	}
	calls := srv.Calls()

	r.moveTo(t, "VALIDATE", r.units["11.3"])
	r.markDone(t, r.units["11.3"])
	r.moveTo(t, "IMPLEMENT", r.units["11.4"])
	received := time.Now().UTC()
	got := r.prepareNext(t, service.NextRequest{ResultReceivedAt: &received})
	if srv.Calls() != calls {
		t.Fatal("U11.P.10 resolution called the compression provider")
	}
	if !hasLayer(got, "TEMPLATE") || got.WorkerPrompt == nil || got.WorkerPrompt.Source != compression.SourceCompressed {
		t.Fatalf("U11.P.10 template not resolved: layers=%v", got.Layers)
	}
	u4, _ := r.units["11.4"], 0
	if n := p9Count(t, r.h, "state_files", "work_unit_id = '"+u4.String()+"'"); n != 1 {
		t.Fatal("U11.P.9 real ACTIVE_TASK not calculated")
	}
	if compression.ContainsPlaceholder(got.WorkerPrompt.Content) || !strings.Contains(got.WorkerPrompt.Content, "# Active Task") ||
		!strings.Contains(got.WorkerPrompt.Content, u4.String()) {
		t.Fatal("U11.P.13 resolved prompt lacks the real ACTIVE_TASK")
	}
	var status string
	if err := r.h.DBScan(ctx, "SELECT status FROM precomputed_templates WHERE id = '"+next.TemplateID.String()+"'", &status); err != nil || status != "RESOLVED" {
		t.Fatalf("template status %q", status)
	}
	if got.Entry == nil || got.Entry.Source != cache.SourcePrecomputed {
		t.Fatal("resolved entry not marked PRECOMPUTED")
	}
	// Second preparation: plain Layer 3 hit.
	again := r.prepareNext(t, service.NextRequest{})
	if !again.Hit || again.WorkerPrompt.ID != got.WorkerPrompt.ID {
		t.Fatal("resolved prompt not reused")
	}
	// ARCHITECTURE changes: the cached prompt no longer matches; recompute.
	e := &p10Env{h: r.h, pos: r.pos}
	e.writeFile(t, "ARCHITECTURE", "ARCHITECTURE v2\n"+p9Background("ARCHITECTURE-V2", 120))
	changed := r.prepareNext(t, service.NextRequest{})
	if changed.Hit || changed.WorkerPrompt.ID == got.WorkerPrompt.ID || srv.Calls() == calls {
		t.Fatal("T11.24.1 architecture change did not force recomputation")
	}
	var lat string
	if err := r.h.DBScan(ctx, "SELECT source FROM transition_latencies WHERE project_id = '"+pid+"' ORDER BY created_at ASC LIMIT 1", &lat); err != nil || lat != precompute.LatencyPlaceholder {
		t.Fatalf("latency source %q", lat)
	}
	_ = statefile.RenderActiveTask
}
