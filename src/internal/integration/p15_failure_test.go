package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"overseer/internal/config"
	"overseer/internal/execution"
	"overseer/internal/precompute"
)

// TestP15164MaterializationFailureRetry — WBS 15.164 / 15.52 / 15.87 /
// 15.109: materialization blocked after execution initialization → #1
// retained INITIALIZING with failed stage INITIALIZING_FILES, project
// READY; after repair the retry resumes #1, reconciles files, no runtime
// duplication, completes.
func TestP15164MaterializationFailureRetry(t *testing.T) {
	r := p15NewRig(t)
	ctx := context.Background()
	if _, err := r.svc().UpdateExecutionSettings(ctx, r.pid, execution.Settings{DirtyPolicy: execution.DirtyAllow, MaterializeStateFiles: true,
		MaxBootstrapControlTransitions: 100, BootstrapLease: execution.DefaultSettings().BootstrapLease}); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(r.dir, ".overseer")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, be := r.bootstrapErr(t)
	if be.Code != execution.CodeMaterializationFailed || be.Class != execution.ClassRecoverable {
		t.Fatalf("%+v", be)
	}
	e1 := res.Execution
	if e1 == nil || e1.Number != 1 || e1.Status != execution.ExecutionInitializing || e1.Stage != execution.StageFailed ||
		e1.FailedStage != execution.StageInitializingFiles || e1.FailureCode != execution.CodeMaterializationFailed {
		t.Fatalf("execution after failure %+v", e1)
	}
	if r.projectStatus(t) != "READY" {
		t.Fatal("project not READY after failure")
	}
	// Restart-visible failed stage (WBS 15.86).
	st, err := r.svc().BootstrapStatusOf(ctx, r.pid, e1.ID)
	if err != nil || st.Execution.FailedStage != execution.StageInitializingFiles {
		t.Fatalf("status %v %+v", err, st)
	}
	versionsBefore := r.count(t, "SELECT count(*) FROM state_file_versions v JOIN state_files f ON f.id = v.state_file_id WHERE f.project_id = '"+r.pid.String()+"'")

	// Repair and retry.
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	e2 := r.bootstrap(t).Execution
	if e2.ID != e1.ID || e2.Number != 1 || e2.Status != execution.ExecutionRunning {
		t.Fatalf("retry did not resume #1: %+v", e2)
	}
	if n := r.count(t, "SELECT count(*) FROM executions WHERE "+r.where()); n != 1 {
		t.Fatalf("executions = %d", n)
	}
	if n := r.count(t, "SELECT count(*) FROM runtime_state WHERE "+r.where()); n != 1 {
		t.Fatalf("runtime rows = %d", n)
	}
	if after := r.count(t, "SELECT count(*) FROM state_file_versions v JOIN state_files f ON f.id = v.state_file_id WHERE f.project_id = '"+r.pid.String()+"'"); after != versionsBefore {
		t.Fatalf("state-file versions duplicated: %d → %d", versionsBefore, after)
	}
	for _, f := range []string{"PROJECT_STATE.md", "TASK_ARCHIVE.md"} { // project-scoped files are materialized (Phase 7)
		if _, err := os.Stat(filepath.Join(r.dir, ".overseer", "state", f)); err != nil {
			t.Fatalf("materialized %s: %v", f, err)
		}
	}
	r.noWorker(t)
	m, _ := r.svc().Metrics(ctx, r.pid)
	if m.Failures != 1 || m.Successes != 1 || m.Retries != 1 || m.FailuresByCode["MATERIALIZATION_FAILED"] != 1 {
		t.Fatalf("metrics %+v", m)
	}
}

// TestP15124CompressionFailureRetry — WBS 15.124 / 15.73 / 15.111: the
// canonical prompt is composed, compression fails (provider down, canonical
// does not fit) → no dispatch, failure persisted; retry resumes prompt
// preparation from the persisted context snapshot.
func TestP15124CompressionFailureRetry(t *testing.T) {
	var healthy atomic.Bool
	var target *url.URL
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Model listing always succeeds so the provider passes bootstrap's
		// reachability check; only compression calls fail while unhealthy.
		if !healthy.Load() && !strings.HasSuffix(req.URL.Path, "/models") {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, req)
	}))
	t.Cleanup(flaky.Close)
	r := p15NewRigWith(t, p15DefaultPhases, func(c *config.Config) {
		c.CompressionProviders["flaky"] = config.CompressionProviderConfig{Kind: config.CompressionKindOpenAICompatible, BaseURL: flaky.URL + "/v1", APIKeyEnv: "P9_FAKE_KEY"}
	})
	target, _ = url.Parse(r.srv.URL)
	// A large compressible ARCHITECTURE file required by the planning role
	// makes the canonical prompt exceed the context window.
	arch := p713ProjectFile(t, r.h, r.pid, p709DefaultTypeID(t, r.h, "ARCHITECTURE"))
	big := "# Architecture\n"
	for i := 0; i < 300; i++ {
		big += "Layer " + itoa(i) + " delegates persistence to repositories and keeps the domain free of infrastructure.\n"
	}
	if _, err := p708Service(t, r.h).UpdateContent(context.Background(), arch.String(), big, 0); err != nil {
		t.Fatal(err)
	}
	p712RequireRoleFiles(t, r.h, r.roles["PLANNER"], []string{"ARCHITECTURE"})
	r.exec(t, "UPDATE project_settings SET compression_provider = 'flaky', context_window = 2000 WHERE "+r.where())
	if code, raw, _ := p9HTTP(t, http.MethodPut, r.h.BaseURL+"/api/v1/projects/"+r.pid.String()+"/settings/compression", `{"mode":"ALWAYS","target_tokens":1500,"max_attempts":1}`); code != 200 {
		t.Fatalf("compression settings: %s", raw)
	}
	cs, _, err := r.h.App.PromptCache().CacheSettings(context.Background(), r.pid)
	if err != nil {
		t.Fatal(err)
	}
	cs.NegativeWindow = 0 // retry immediately (the Phase 10 negative cache would otherwise suppress it)
	if _, err := r.h.App.PromptCache().UpdateCacheSettings(context.Background(), r.pid, cs); err != nil {
		t.Fatal(err)
	}
	res, be := r.bootstrapErr(t)
	if be.Code != execution.CodeCompressionFailed || res.Execution.Status != execution.ExecutionInitializing ||
		res.Execution.FailedStage != execution.StagePreparingContext {
		t.Fatalf("%+v %+v", be, res.Execution)
	}
	snap := res.Execution.InitialContextSnapshotID
	if snap == nil {
		t.Fatal("context snapshot not persisted before prompt preparation")
	}
	if n := r.count(t, "SELECT count(*) FROM canonical_prompts WHERE "+r.where()); n == 0 {
		t.Fatal("canonical prompt not composed")
	}
	if r.projectStatus(t) != "READY" {
		t.Fatal("project RUNNING after compression failure")
	}
	r.noWorker(t)
	healthy.Store(true)
	e := r.bootstrap(t).Execution
	if e.ID != res.Execution.ID || e.InitialContextSnapshotID == nil || *e.InitialContextSnapshotID != *snap || e.Status != execution.ExecutionRunning {
		t.Fatalf("resume %+v", e)
	}
	var src string
	if err := r.h.DBScan(context.Background(), "SELECT source FROM worker_prompts WHERE id = '"+e.InitialWorkerPromptID.String()+"'", &src); err != nil || src != "COMPRESSED" {
		t.Fatalf("worker prompt source %q %v", src, err)
	}
	if n := r.count(t, "SELECT count(*) FROM context_snapshots WHERE "+r.where()); n != 1 {
		t.Fatalf("context snapshots = %d (recomposed context)", n)
	}
	r.noWorker(t)
}

// TestP15125SpeculativeFailure — WBS 15.125 / 15.75 / 15.76: speculative
// precomputation is scheduled but never awaited — bootstrap completes while
// the speculative job is still blocked, and a refused / failing speculative
// job never fails bootstrap.
func TestP15125SpeculativeFailure(t *testing.T) {
	r := p15NewRig(t)
	release := make(chan struct{})
	var hits atomic.Int32
	r.h.App.Precompute().WithBeforeCandidate(func(ctx context.Context, rec *precompute.CandidateRecord) {
		hits.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
	})
	defer close(release)
	e := r.bootstrap(t).Execution
	if e.Status != execution.ExecutionRunning {
		t.Fatalf("%+v", e)
	}
	if n := r.count(t, "SELECT count(*) FROM precompute_jobs WHERE "+r.where()); n == 0 {
		t.Fatal("precompute not scheduled")
	}
	// A second project whose precompute is disabled: the trigger is
	// refused and bootstrap still succeeds.
	r2 := p15Build(t, r.h, r.g, r.srv, p15DefaultPhases)
	ps, _, err := r.h.App.Precompute().PrecomputeSettings(context.Background(), r2.pid)
	if err != nil {
		t.Fatal(err)
	}
	ps.Enabled = false
	if _, err := r.h.App.Precompute().UpdatePrecomputeSettings(context.Background(), r2.pid, ps); err != nil {
		t.Fatal(err)
	}
	if e := r2.bootstrap(t).Execution; e.Status != execution.ExecutionRunning {
		t.Fatalf("%+v", e)
	}
}

// TestP1588ConfigurationFailureRetry — WBS 15.88: a project corrected after
// a readiness failure bootstraps successfully (no execution was created).
func TestP1588ConfigurationFailureRetry(t *testing.T) {
	r := p15NewRig(t)
	r.exec(t, "UPDATE roles SET enabled = false WHERE id = '"+r.roles["PLANNER"].String()+"'")
	r.bootstrapErr(t)
	r.exec(t, "UPDATE roles SET enabled = true WHERE id = '"+r.roles["PLANNER"].String()+"'")
	e := r.bootstrap(t).Execution
	if e.Number != 1 {
		t.Fatalf("number %d", e.Number)
	}
}
