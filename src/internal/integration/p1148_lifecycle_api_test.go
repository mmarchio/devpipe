package integration

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/cache"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/precompute"
	"overseer/internal/service"
)

func p11SetStatus(t *testing.T, r *p11Rig, status string) {
	t.Helper()
	if _, err := r.h.DBExec(context.Background(), "UPDATE projects SET status = '"+status+"' WHERE id = '"+r.pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
}

// P11-48 + P11-31 (WBS 11.41, 11.42, 11.71, 11.72): pausing or cancelling
// a project during an active job stops new candidate work, cancels the job,
// keeps the project status and every completed artifact, and causes no
// workflow transition or dispatch.
func TestP1148PauseAndCancelDuringJob(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		t.Run(action, func(t *testing.T) {
			srv := providertest.New(providertest.Reply{Delay: 400 * time.Millisecond})
			defer srv.Close()
			r := p11NewRig(t, srv, true)
			ctx := context.Background()
			p11SetStatus(t, r, "RUNNING")
			st, _, _ := r.svc().PrecomputeSettings(ctx, r.pos.ProjectID)
			st.Concurrency = 1
			r.svc().UpdatePrecomputeSettings(ctx, r.pos.ProjectID, st)
			before := p10Rows(t, r.h, "runtime_state", "project_id = '"+r.pos.ProjectID.String()+"'") +
				p10Rows(t, r.h, "work_units", "execution_plan_id = '"+r.pos.PlanID.String()+"'")
			// The status change lands while the job is between candidates
			// (deterministic: the hook performs it before the 2nd one).
			var once sync.Once
			n := 0
			r.svc().WithBeforeCandidate(func(_ context.Context, _ *precompute.CandidateRecord) {
				n++
				if n == 2 {
					once.Do(func() {
						if code, raw, _ := p9HTTP(t, http.MethodPost, r.h.BaseURL+"/api/v1/projects/"+r.pos.ProjectID.String()+"/"+action, ""); code != 200 {
							t.Errorf("%s: %d %s", action, code, raw)
						}
					})
				}
			})
			defer r.svc().WithBeforeCandidate(nil)
			job := r.svc().NotifyWorkerDispatched(ctx, r.pos.ProjectID)
			r.svc().Wait()
			j, recs, _ := r.svc().GetJob(ctx, r.pos.ProjectID, job.ID)
			if j.Status != precompute.JobCancelled {
				t.Fatalf("job status %s", j.Status)
			}
			skippedAfter := 0
			for _, c := range recs {
				if c.Result == precompute.ResultSkipped && strings.Contains(c.Reason, "cancelled") {
					skippedAfter++
				}
			}
			if skippedAfter == 0 {
				t.Fatal("11.71 new candidate work started after the status change")
			}
			want := map[string]string{"pause": "PAUSED", "cancel": "CANCELLED"}[action]
			var status string
			r.h.DBScan(ctx, "SELECT status FROM projects WHERE id = '"+r.pos.ProjectID.String()+"'", &status)
			if status != want {
				t.Fatalf("project status %s", status)
			}
			after := p10Rows(t, r.h, "runtime_state", "project_id = '"+r.pos.ProjectID.String()+"'") +
				p10Rows(t, r.h, "work_units", "execution_plan_id = '"+r.pos.PlanID.String()+"'")
			if before != after {
				t.Fatal("11.72 workflow transition / unit change during cancellation")
			}
			if _, err := r.svc().Trigger(ctx, r.pos.ProjectID, precompute.TriggerManual); err == nil {
				t.Fatal("T11.42.1 job started while paused/cancelled")
			}
			if n := p9Count(t, r.h, "precompute_projections", "project_id = '"+r.pos.ProjectID.String()+"'"); n == 0 {
				t.Fatal("T11.42.3/T11.41.3 completed artifacts lost")
			}
			if action == "pause" {
				if code, _, _ := p9HTTP(t, http.MethodPost, r.h.BaseURL+"/api/v1/projects/"+r.pos.ProjectID.String()+"/resume", ""); code != 200 {
					t.Fatal("resume")
				}
				if _, err := r.svc().Trigger(ctx, r.pos.ProjectID, precompute.TriggerManual); err != nil {
					t.Fatalf("T11.42.2 resume did not re-enable precompute: %v", err)
				}
				r.svc().Wait()
			}
		})
	}
}

// P11-52 (WBS 11.73): a job RUNNING on an instance that died (with a
// pending candidate and a stale cache build lease) is recovered at
// startup: the job is reclassified, the candidate marked interrupted, the
// stale lease reclaimed by the next build, no duplicate authoritative
// entry, runtime position unchanged.
func TestP1173RestartRecovery(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	h, _, boot := p9RestartRig(t, srv)
	r := p11Build(t, h, srv, true)
	ctx := context.Background()
	res := r.warm(t, precompute.TriggerWorkerDispatch)
	next := r.candidates(t, res.JobID)["IMPLEMENT+next"]
	// A crashed job from another instance: RUNNING, stale heartbeat, a
	// fragment candidate done and one still pending; plus a stale BUILDING
	// cache entry for a new key (another instance died mid-build).
	dead := uuid.New()
	pid := r.pos.ProjectID.String()
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	for _, q := range []string{
		`INSERT INTO precompute_jobs (id, project_id, trigger, status, owner, created_at, started_at, heartbeat_at) VALUES ('` + dead.String() + `', '` + pid + `', 'WORKER_DISPATCH', 'RUNNING', 'dead-instance', '` + old + `', '` + old + `', '` + old + `')`,
		`INSERT INTO precompute_candidates (id, job_id, project_id, state_key, readiness, priority, result, created_at, updated_at) VALUES ('` + uuid.NewString() + `', '` + dead.String() + `', '` + pid + `', 'CONTINUE', 'PARTIALLY_KNOWN', 2, 'FRAGMENT', now(), now())`,
		`INSERT INTO precompute_candidates (id, job_id, project_id, state_key, readiness, priority, result, created_at, updated_at) VALUES ('` + uuid.NewString() + `', '` + dead.String() + `', '` + pid + `', 'VALIDATE', 'PARTIALLY_KNOWN', 2, 'PENDING', now(), now())`,
	} {
		if _, err := h.DBExec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	(&p10Env{h: h, pos: r.pos}).writeFile(t, "PHASE_STATE", "PHASE_STATE v2\n"+p9Background("PHASE_STATE-V2", 40))
	runtimeBefore := p9RuntimeRow(t, h, r.pos.ProjectID)
	templatesBefore := p10Rows(t, h, "precompute_projections", "project_id = '"+pid+"'")

	h.Close()
	h2 := boot()
	r.h = h2
	j, recs, err := r.svc().GetJob(ctx, r.pos.ProjectID, dead)
	if err != nil || j.Status != precompute.JobPartial || !strings.Contains(j.Reason, "recovered") {
		t.Fatalf("11.73 job not reclassified: %v %+v", err, j)
	}
	for _, c := range recs {
		if c.StateKey == "VALIDATE" && (c.Result != precompute.ResultFailed || !strings.Contains(c.Reason, "interrupted")) {
			t.Fatalf("pending candidate not marked interrupted: %+v", c)
		}
	}
	if p9RuntimeRow(t, h2, r.pos.ProjectID) != runtimeBefore || p10Rows(t, h2, "precompute_projections", "project_id = '"+pid+"'") != templatesBefore {
		t.Fatal("11.73 runtime position or precompute history changed by restart")
	}
	// Completed artifacts retained and still usable; a new job works.
	if e, err := h2.App.PromptCache().GetEntry(ctx, r.pos.ProjectID, *next.CacheEntryID); err != nil || e.Status != cache.StatusValid {
		t.Fatal("completed precomputed entry lost across restart")
	}
	res2 := r.warm(t, precompute.TriggerManual)
	if res2.Status != precompute.JobCompleted {
		t.Fatalf("post-restart job %+v", res2)
	}
	if n := p9Count(t, h2, "prompt_cache_entries", "project_id = '"+pid+"' AND status = 'BUILDING'"); n != 0 {
		t.Fatal("stranded build lease")
	}
}

// P11-52 (WBS 11.45, 11.74): two overseer instances warm the same project
// concurrently — one authoritative cache entry per key, one pending
// template per normalized key, no corruption.
func TestP1174MultiInstance(t *testing.T) {
	srv := providertest.New(providertest.Reply{Delay: 200 * time.Millisecond})
	defer srv.Close()
	hA, _, boot := p9RestartRig(t, srv)
	r := p11Build(t, hA, srv, false)
	hB := boot()
	ctx := context.Background()
	rt, _ := r.svc().RuntimeID(ctx, r.pos.ProjectID)
	var wg sync.WaitGroup
	for _, h := range []*Harness{hA, hB, hA, hB} {
		wg.Add(1)
		go func(h *Harness) {
			defer wg.Done()
			_, _ = h.App.Precompute().WarmUpcomingPrompts(ctx, precompute.Request{ProjectID: r.pos.ProjectID, RuntimeStateID: rt, Trigger: precompute.TriggerManual})
		}(h)
	}
	wg.Wait()
	pid := r.pos.ProjectID.String()
	var dupEntries, dupTemplates int
	hA.DBScan(ctx, `SELECT count(*) FROM (SELECT cache_key FROM prompt_cache_entries WHERE project_id = '`+pid+`' AND status IN ('VALID','BUILDING') GROUP BY cache_key, layer HAVING count(*) > 1) x`, &dupEntries)
	hA.DBScan(ctx, `SELECT count(*) FROM (SELECT normalized_key FROM precomputed_templates WHERE project_id = '`+pid+`' AND status = 'PENDING' GROUP BY normalized_key HAVING count(*) > 1) x`, &dupTemplates)
	if dupEntries != 0 || dupTemplates != 0 {
		t.Fatalf("T11.74.2 duplicates: entries=%d templates=%d", dupEntries, dupTemplates)
	}
	if p9Count(t, hA, "precomputed_templates", "project_id = '"+pid+"' AND status = 'PENDING'") != 1 {
		t.Fatal("T11.74.1 expected exactly one pending template for IMPLEMENT 11.4")
	}
	// The shared result is usable from either instance.
	r.markDone(t, r.units["11.3"])
	r.moveTo(t, "IMPLEMENT", r.units["11.4"])
	r.h = hB
	got := r.prepareNext(t, service.NextRequest{})
	if !hasLayer(got, "TEMPLATE") {
		t.Fatalf("T11.74.3 template not resolvable cross-instance: %v", got.Layers)
	}
}

// P11-53 + P11-36..P11-40 (WBS 11.54-11.57, 11.85, 11.3): the precompute
// REST surface end to end; nothing in the project / work-unit state
// changes.
func TestP1185PrecomputeAPI(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	r := p11NewRig(t, srv, true)
	before := r.workflowRows(t)
	code, raw, body := p9HTTP(t, http.MethodGet, r.base+"/settings/precompute", "")
	if code != 200 || body["enabled"] != true || body["depth"] != float64(2) || body["max_candidates"] != float64(8) || body["concurrency"] != float64(2) || body["stored"] != false {
		t.Fatalf("T11.3.1 defaults: %s", raw)
	}
	if code, _, b := p9HTTP(t, http.MethodPut, r.base+"/settings/precompute", `{"depth":0,"concurrency":99}`); code != 400 || b["error"].(map[string]any)["fields"] == nil {
		t.Fatal("T11.3.3/T11.3.4 invalid settings accepted")
	}
	if code, _, _ := p9HTTP(t, http.MethodPut, r.base+"/settings/precompute", `{"depth":2,"concurrency":2}`); code != 200 {
		t.Fatal("settings update")
	}
	// Candidate preview: no compression call, no projection rows.
	calls := srv.Calls()
	code, raw, body = p9HTTP(t, http.MethodGet, r.base+"/precompute/candidates", "")
	cands := body["candidates"].([]any)
	if code != 200 || len(cands) < 4 || srv.Calls() != calls || p9Count(t, r.h, "precompute_projections", "project_id = '"+r.pos.ProjectID.String()+"'") != 0 {
		t.Fatalf("T11.56.1/T11.56.4 preview: %d %s", code, raw)
	}
	seen := map[string]map[string]any{}
	for _, c := range cands {
		cm := c.(map[string]any)
		seen[cm["state_key"].(string)+map[bool]string{true: "+next"}[cm["next_unit"] == true]] = cm
	}
	if seen["VALIDATE"]["readiness"] != "PARTIALLY_KNOWN" || !strings.Contains(seen["VALIDATE"]["status"].(string), "IMPLEMENTATION_RESULT") ||
		seen["IMPLEMENT+next"]["readiness"] != "FULLY_KNOWN" || seen["IMPLEMENT+next"]["status"] != "READY" {
		t.Fatalf("T11.56.2/T11.56.3 readiness / missing deps: %s", raw)
	}
	if strings.Contains(strings.ToLower(raw), "probab") {
		t.Fatal("T11.57 probability language in the read model")
	}
	// Manual job (synchronous) and async job.
	code, raw, job := p9HTTP(t, http.MethodPost, r.base+"/precompute?wait=true", "")
	if code != 200 || job["status"] != "COMPLETED" || job["candidate_count"].(float64) < 4 || job["full_prompts"] != float64(1) || job["fragments"] != float64(3) {
		t.Fatalf("T11.55.1/11.85 job: %s", raw)
	}
	code, _, async := p9HTTP(t, http.MethodPost, r.base+"/precompute", "")
	if code != 202 || async["status"] != "PENDING" {
		t.Fatal("async manual job")
	}
	r.svc().Wait()
	_, raw, detail := p9HTTP(t, http.MethodGet, r.base+"/precompute/jobs/"+async["id"].(string), "")
	if detail["status"] != "COMPLETED" || detail["existing_hits"] != float64(1) || len(detail["candidates"].([]any)) < 4 {
		t.Fatalf("T11.54.2/T11.43.2 second job: %s", raw)
	}
	for _, c := range detail["candidates"].([]any) {
		cm := c.(map[string]any)
		if cm["readiness"] == nil || cm["result"] == nil || cm["priority"] == nil {
			t.Fatal("candidate details incomplete")
		}
	}
	_, _, list := p9HTTP(t, http.MethodGet, r.base+"/precompute/jobs?limit=1&offset=1", "")
	if list["total"] != float64(2) || len(list["jobs"].([]any)) != 1 || list["jobs"].([]any)[0].(map[string]any)["id"] != job["id"] {
		t.Fatalf("T11.54.1/T11.54.4 pagination: %v", list)
	}
	other, _, _ := p705Rig(t, r.h)
	if code, _, _ := p9HTTP(t, http.MethodGet, r.h.BaseURL+"/api/v1/projects/"+other.String()+"/precompute/jobs/"+job["id"].(string), ""); code != 404 {
		t.Fatal("T11.54.3 project isolation")
	}
	_, raw, sum := p9HTTP(t, http.MethodGet, r.base+"/precompute/summary", "")
	if sum["current_job"] == nil || len(sum["candidates"].([]any)) == 0 {
		t.Fatalf("T11.57.1 summary: %s", raw)
	}
	_, raw, m := p9HTTP(t, http.MethodGet, r.base+"/precompute/metrics", "")
	if m["jobs"] != float64(2) || m["full_prompts"] != float64(1) || m["existing_hits"].(float64) < 1 || m["utilization_by_depth"] == nil {
		t.Fatalf("T11.51 metrics: %s", raw)
	}
	if strings.Contains(raw, p9SentinelKey) {
		t.Fatal("secret in metrics")
	}
	if r.workflowRows(t) != before {
		t.Fatal("T11.55.3/11.85 project or work-unit state changed")
	}
	if code, _, _ := p9HTTP(t, http.MethodDelete, r.base+"/precompute/jobs", ""); code != 405 {
		t.Fatal("read-only jobs endpoint accepted DELETE")
	}
	var evs string
	r.h.DBScan(context.Background(), "SELECT string_agg(DISTINCT event_type, ',') FROM prompt_cache_events WHERE project_id = '"+r.pos.ProjectID.String()+"' AND layer = 'PRECOMPUTE'", &evs)
	for _, e := range []string{"PRECOMPUTE_STARTED", "PRECOMPUTE_CANDIDATE_DISCOVERED", "PRECOMPUTE_CACHE_HIT", "PRECOMPUTE_FRAGMENT_CREATED", "PRECOMPUTE_PROMPT_CREATED", "PRECOMPUTE_SKIPPED", "PRECOMPUTE_COMPLETED"} {
		if !strings.Contains(evs, e) {
			t.Fatalf("T11.50 %s not emitted (%s)", e, evs)
		}
	}
	var completion string
	r.h.DBScan(context.Background(), "SELECT reason FROM prompt_cache_events WHERE project_id = '"+r.pos.ProjectID.String()+"' AND event_type = 'PRECOMPUTE_COMPLETED' LIMIT 1", &completion)
	if !strings.Contains(completion, "candidates=") {
		t.Fatal("T11.50.3 completion event lacks counts")
	}
}
