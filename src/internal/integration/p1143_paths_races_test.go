package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"overseer/internal/cache"
	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/precompute"
	"overseer/internal/prompt"
	"overseer/internal/service"
)

func p11FragmentHits(t *testing.T, r *p11Rig, kind string) int {
	t.Helper()
	return p9Count(t, r.h, "prompt_cache_events", "project_id = '"+r.pos.ProjectID.String()+"' AND event_type = 'CACHE_HIT' AND layer = 'FRAGMENT' AND reason = '"+kind+"'")
}

// P11-43/P11-44 (WBS 11.26-11.28, 11.63, 11.64, 11.79, 11.80): the static
// continuation material is ready while IMPLEMENT 11.3 runs; when the worker
// returns INCOMPLETE the final continuation prompt is composed with the
// real captured status (same unit, never fabricated) and reuses the static
// fragments — for both same-session (low context) and reset-session (high
// context) continuations.
func TestP1143ContinuationFastPaths(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	r := p11NewRig(t, srv, true)
	res := r.warm(t, precompute.TriggerWorkerDispatch)
	cont := r.candidates(t, res.JobID)["CONTINUE"]
	if cont.Result != precompute.ResultFragment || len(cont.FragmentIDs) == 0 {
		t.Fatalf("T11.26.1 static continuation fragment: %+v", cont)
	}
	var mf string
	if err := r.h.DBScan(context.Background(), "SELECT required_inputs::text FROM fragment_manifests WHERE fragment_id = '"+cont.FragmentIDs[0].String()+"' AND projection_hash = (SELECT projection_hash FROM precompute_projections WHERE id = '"+cont.ProjectionID.String()+"')", &mf); err != nil || !strings.Contains(mf, "WORKER_STATUS") {
		t.Fatalf("T11.13.1 manifest: %q %v", mf, err)
	}
	if n := p9Count(t, r.h, "canonical_prompts", "project_id = '"+r.pos.ProjectID.String()+"' AND content LIKE '%completed: %'"); n != 0 {
		t.Fatal("T11.26.2 a continuation status was fabricated")
	}
	r.moveTo(t, "CONTINUE", r.units["11.3"])
	status := &prompt.ContinuationStatusInput{WorkUnitID: r.units["11.3"], Completed: []string{"repository implementation"}, Remaining: []string{"integration test", "error mapping"}}

	// Low context (52% < 80%): same-session continuation.
	before := p11FragmentHits(t, r, "SYSTEM_ROLE_PREFIX")
	same := r.prepareNext(t, service.NextRequest{Continuation: status, ContinuationMode: service.ContinuationSameSession})
	if !strings.Contains(same.WorkerPrompt.Content, "remaining: integration test; error mapping") || !strings.Contains(same.WorkerPrompt.Content, r.units["11.3"].String()) {
		t.Fatal("T11.63.2 actual status not inserted / unit changed")
	}
	if strings.Contains(strings.ToLower(same.WorkerPrompt.Content), "new session") {
		t.Fatal("T11.63.3 session reset assumed")
	}
	if p11FragmentHits(t, r, "SYSTEM_ROLE_PREFIX") <= before {
		t.Fatal("T11.63.1/T11.27.1 static content not reused")
	}
	// High context (91% > 80%): reset-session continuation reconstructs
	// the full task from the precomputed static context + captured status.
	reset := r.prepareNext(t, service.NextRequest{Continuation: status, ContinuationMode: service.ContinuationResetSession})
	for _, want := range []string{"ARCHITECTURE", "PROJECT_STATE", "# Active Task", "AC-1: 11.3", "completed: repository implementation"} {
		if !strings.Contains(reset.WorkerPrompt.Content, want) {
			t.Fatalf("T11.64.3/T11.28.3 reconstruction lacks %q", want)
		}
	}
	if p11FragmentHits(t, r, "STATE_PREFIX") == 0 {
		t.Fatal("T11.64.1 static reset context not reused")
	}
}

// P11-45 (WBS 11.30, 11.66, 11.81): the static FIX context is ready before
// validation; on FAIL the FIX prompt reuses it and stays scoped to 11.3.
// Recorded limit: the Phase 8 composer has no validation-failure section
// yet (Phase 13 adds worker-result inputs), so "actual failures inserted"
// is asserted as: no failure text is ever fabricated before validation.
func TestP1145ValidationFailureFastPath(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	r := p11NewRig(t, srv, true)
	res := r.warm(t, precompute.TriggerWorkerDispatch)
	fix := r.candidates(t, res.JobID)["FIX"]
	if fix.Result != precompute.ResultFragment || fix.Missing[0] != precompute.InputValidationFailures {
		t.Fatalf("T11.30.1 %+v", fix)
	}
	if n := p9Count(t, r.h, "canonical_prompts", "project_id = '"+r.pos.ProjectID.String()+"' AND content LIKE '%TEST-2 failed%'"); n != 0 {
		t.Fatal("T11.30.2/T11.25.3 failures fabricated")
	}
	r.moveTo(t, "VALIDATE", r.units["11.3"])
	r.moveTo(t, "FIX", r.units["11.3"])
	before := p11FragmentHits(t, r, "SYSTEM_ROLE_PREFIX")
	got := r.prepareNext(t, service.NextRequest{})
	if p11FragmentHits(t, r, "SYSTEM_ROLE_PREFIX") <= before || p11FragmentHits(t, r, "STATE_PREFIX") == 0 {
		t.Fatal("T11.66.1 static FIX fragment not reused")
	}
	if !strings.Contains(got.WorkerPrompt.Content, r.units["11.3"].String()) || strings.Contains(got.WorkerPrompt.Content, r.units["11.4"].String()) {
		t.Fatal("T11.66 FIX prompt not scoped to 11.3")
	}
	if !strings.Contains(got.WorkerPrompt.Content, "AC-1: 11.3 criterion one.") {
		t.Fatal("T11.66.3 original criteria not retained")
	}
}

// P11-46/P11-47 (WBS 11.24, 11.67, 11.68, 11.70, 11.82): a precomputed
// IMPLEMENT 11.4 prompt is never dispatched after its inputs change before
// use; canonical material stays reusable for a compression-only change.
func TestP1146StaleRaces(t *testing.T) {
	ctx := context.Background()
	t.Run("architecture", func(t *testing.T) {
		srv := providertest.New()
		defer srv.Close()
		r := p11NewRig(t, srv, true)
		next := r.candidates(t, r.warm(t, precompute.TriggerWorkerDispatch).JobID)["IMPLEMENT+next"]
		(&p10Env{h: r.h, pos: r.pos}).writeFile(t, "ARCHITECTURE", "ARCHITECTURE v8\n"+p9Background("ARCHITECTURE-V8", 120))
		r.markDone(t, r.units["11.3"])
		r.moveTo(t, "IMPLEMENT", r.units["11.4"])
		got := r.prepareNext(t, service.NextRequest{})
		if got.Hit || got.Entry.ID == *next.CacheEntryID {
			t.Fatal("11.67 v7 prompt used after ARCHITECTURE v8")
		}
		if e, _ := r.h.App.PromptCache().GetEntry(ctx, r.pos.ProjectID, *next.CacheEntryID); e.HitCount != 0 || e.Status != cache.StatusValid {
			t.Fatal("T11.24.4/T11.76.1 historical precomputed artifact not retained unused")
		}
	})
	t.Run("work unit modified", func(t *testing.T) {
		srv := providertest.New()
		defer srv.Close()
		r := p11NewRig(t, srv, true)
		next := r.candidates(t, r.warm(t, precompute.TriggerWorkerDispatch).JobID)["IMPLEMENT+next"]
		if _, err := r.h.DBExec(ctx, `UPDATE work_units SET acceptance_criteria = acceptance_criteria || '["AC-3: 11.4 criterion three."]'::jsonb WHERE id = '`+r.units["11.4"].String()+`'`); err != nil {
			t.Fatal(err)
		}
		r.markDone(t, r.units["11.3"])
		r.moveTo(t, "IMPLEMENT", r.units["11.4"])
		calls := srv.Calls()
		got := r.prepareNext(t, service.NextRequest{})
		if got.Hit || got.Entry.ID == *next.CacheEntryID || !strings.Contains(got.WorkerPrompt.Content, "AC-3: 11.4 criterion three.") || srv.Calls() == calls {
			t.Fatal("11.68/11.82 stale unit prompt used or not recompressed")
		}
	})
	t.Run("compression model", func(t *testing.T) {
		srv := providertest.New()
		defer srv.Close()
		r := p11NewRig(t, srv, true)
		next := r.candidates(t, r.warm(t, precompute.TriggerWorkerDispatch).JobID)["IMPLEMENT+next"]
		if _, err := r.h.DBExec(ctx, "UPDATE project_settings SET compression_model = 'model-B' WHERE project_id = '"+r.pos.ProjectID.String()+"'"); err != nil {
			t.Fatal(err)
		}
		r.markDone(t, r.units["11.3"])
		r.moveTo(t, "IMPLEMENT", r.units["11.4"])
		got := r.prepareNext(t, service.NextRequest{})
		if got.Hit || got.Entry.ID == *next.CacheEntryID || !hasLayer(got, "CANONICAL") || got.Run.Model != "model-B" {
			t.Fatalf("11.70 model-A output reused or canonical not reused: layers=%v", got.Layers)
		}
	})
}

// P11-30/P11-29 (WBS 11.4, 11.38-11.40, 11.58): the dispatch hook returns
// immediately while the job runs asynchronously; disabled settings prevent
// it; IDLE_ONLY defers compression work while the worker is active;
// candidate and token budgets stop cleanly.
func TestP1130HooksBudgetsPolicy(t *testing.T) {
	srv := providertest.New(providertest.Reply{Delay: 400 * time.Millisecond})
	defer srv.Close()
	r := p11NewRig(t, srv, true)
	ctx := context.Background()
	start := time.Now()
	job := r.svc().NotifyWorkerDispatched(ctx, r.pos.ProjectID)
	if job == nil || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("T11.4.2/T11.58.2 dispatch blocked (%v)", time.Since(start))
	}
	r.svc().Wait()
	j, _, _ := r.svc().GetJob(ctx, r.pos.ProjectID, job.ID)
	if j.Status != precompute.JobCompleted || j.Trigger != precompute.TriggerWorkerDispatch || j.CandidateCount == 0 {
		t.Fatalf("T11.4.1/T11.58.1/T11.58.3 job %+v", j)
	}
	// IDLE_ONLY while the worker is active: compression work skipped,
	// static fragments still prepared.
	st, _, _ := r.svc().PrecomputeSettings(ctx, r.pos.ProjectID)
	st.ResourcePolicy = precompute.PolicyIdleOnly
	if _, err := r.svc().UpdatePrecomputeSettings(ctx, r.pos.ProjectID, st); err != nil {
		t.Fatal(err)
	}
	calls := srv.Calls()
	if _, err := r.h.DBExec(ctx, "DELETE FROM prompt_cache_entries WHERE project_id = '"+r.pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	cands := r.candidates(t, r.warm(t, precompute.TriggerManual).JobID)
	if c := cands["IMPLEMENT+next"]; c.Result != precompute.ResultSkipped || !strings.Contains(c.Reason, "IDLE_ONLY") || srv.Calls() != calls {
		t.Fatalf("T11.40.2 %+v", c)
	}
	if cands["VALIDATE"].Result != precompute.ResultFragment {
		t.Fatal("fragment work must continue under IDLE_ONLY")
	}
	r.svc().NotifyWorkerFinished(r.pos.ProjectID)
	if c := r.candidates(t, r.warm(t, precompute.TriggerManual).JobID)["IMPLEMENT+next"]; c.Result != precompute.ResultBuilt {
		t.Fatalf("T11.40.1 idle worker: %+v", c)
	}
	// Candidate limit and token budget.
	st.ResourcePolicy, st.MaxCandidates = precompute.PolicyAlways, 2
	r.svc().UpdatePrecomputeSettings(ctx, r.pos.ProjectID, st)
	if res := r.warm(t, precompute.TriggerManual); res.Candidates != 2 {
		t.Fatalf("T11.16.1/T11.39.1 %d candidates", res.Candidates)
	}
	st.MaxCandidates, st.MaxTokens = 8, 10
	r.svc().UpdatePrecomputeSettings(ctx, r.pos.ProjectID, st)
	if _, err := r.h.DBExec(ctx, "DELETE FROM prompt_cache_entries WHERE project_id = '"+r.pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	if c := r.candidates(t, r.warm(t, precompute.TriggerManual).JobID)["IMPLEMENT+next"]; c.Result != precompute.ResultSkipped || c.Reason != precompute.BudgetTokens {
		t.Fatalf("T11.39.2/T11.39.4 %+v", c)
	}
	// Disabled settings prevent the trigger (T11.4.3 / T11.3.2).
	st.Enabled = false
	r.svc().UpdatePrecomputeSettings(ctx, r.pos.ProjectID, st)
	if j := r.svc().NotifyWorkerDispatched(ctx, r.pos.ProjectID); j != nil {
		t.Fatal("T11.4.3 disabled precompute triggered")
	}
	if code, _, _ := p9HTTP(t, http.MethodPost, r.base+"/precompute", ""); code != 409 {
		t.Fatal("T11.55.2 manual job on a disabled project")
	}
}

// P11-27 (WBS 11.47-11.49): a provider outage during speculation is
// recorded on the candidate, never fails the caller, and never blocks a
// later on-demand preparation (no negative-cache window).
func TestP1149ProviderOutage(t *testing.T) {
	srv := providertest.New(providertest.Reply{Status: http.StatusServiceUnavailable, Body: `{"error":"down"}`})
	defer srv.Close()
	r := p11NewRig(t, srv, true)
	ctx := context.Background()
	// Context too small for a canonical fallback → the candidate fails.
	if _, err := r.h.DBExec(ctx, "UPDATE project_settings SET context_window = 9000 WHERE project_id = '"+r.pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	res := r.warm(t, precompute.TriggerWorkerDispatch)
	next := r.candidates(t, res.JobID)["IMPLEMENT+next"]
	if next.Result != precompute.ResultFailed || res.Status != precompute.JobPartial || r.candidates(t, res.JobID)["VALIDATE"].Result != precompute.ResultFragment {
		t.Fatalf("T11.47.1-T11.47.3/T11.49.1 %+v %+v", res, next)
	}
	if n := p9Count(t, r.h, "compression_attempts", "error_kind = 'UNAVAILABLE'"); n == 0 {
		t.Fatal("T11.49.1 outage not recorded")
	}
	if n := p9Count(t, r.h, "prompt_cache_entries", "project_id = '"+r.pos.ProjectID.String()+"' AND retry_after IS NOT NULL"); n != 0 {
		t.Fatal("T11.49.3 speculative failure opened a negative-cache window")
	}
	r.markDone(t, r.units["11.3"])
	r.moveTo(t, "IMPLEMENT", r.units["11.4"])
	calls := srv.Calls()
	if _, err := r.svc().PrepareNext(ctx, r.pos.ProjectID, service.NextRequest{}); srv.Calls() == calls {
		t.Fatalf("T11.49.3 on-demand attempt suppressed: %v", err)
	}
}
