package handlers

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"overseer/internal/readmodel"
	"overseer/internal/web/viewmodels"
)

var timelineCategories = []string{"planning", "worker", "validation", "checkpoint", "phase", "recovery", "system"}

type executionData struct {
	Overview      *readmodel.Overview
	Actions       actionsData
	Flows         []readmodel.Flow
	Timeline      readmodel.Timeline
	Categories    []string
	AllCategories []string
}

func categoriesOf(r *http.Request) []string {
	var out []string
	for _, v := range r.URL.Query()["category"] {
		for _, c := range strings.Split(v, ",") {
			if c = strings.TrimSpace(c); c != "" {
				out = append(out, c)
			}
		}
	}
	return out
}

func timelineQuery(r *http.Request) string {
	v := url.Values{}
	for _, c := range categoriesOf(r) {
		v.Add("category", c)
	}
	if b := r.URL.Query().Get("before"); b != "" {
		v.Set("before", b)
	}
	v.Set("limit", "100")
	return v.Encode()
}

// executionPage — GET /projects/{id}/execution and
// /projects/{id}/executions/{execution} (WBS 17.80 / 17.81 / 17.141): the
// timeline is persisted events, filtered and paged server-side.
func (c *Console) executionPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d := executionData{Overview: ov, Actions: c.actionsOf(r, ov), Flows: c.flows(r), Categories: categoriesOf(r), AllCategories: timelineCategories}
	execID := r.PathValue("execution")
	if execID != "" && (ov.Execution == nil || ov.Execution.ID != execID) {
		// A historical execution: show its own record.
		var e map[string]any
		if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/executions/"+url.PathEscape(execID), &e) {
			return
		}
		ov2 := *ov
		ov2.Execution = &readmodel.ExecutionSummary{ID: execID}
		if st, ok := e["status"].(string); ok {
			ov2.Execution.Status = st
		}
		if n, ok := e["number"].(float64); ok {
			ov2.Execution.Number = int(n)
		}
		d.Overview = &ov2
	}
	if d.Overview.Execution != nil {
		c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/console/executions/"+url.PathEscape(d.Overview.Execution.ID)+"/timeline?"+timelineQuery(r), &d.Timeline)
	}
	c.render(w, r, http.StatusOK, "execution", viewmodels.Page{Shell: c.projectShell(r, "Execution", ov), Data: d})
}

// fragTimeline — the timeline region (live refresh).
func (c *Console) fragTimeline(w http.ResponseWriter, r *http.Request) {
	var t readmodel.Timeline
	resp := c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/console/executions/"+url.PathEscape(r.URL.Query().Get("execution"))+"/timeline?"+timelineQuery(r), &t)
	if !resp.OK() {
		http.Error(w, "unavailable", resp.Status)
		return
	}
	c.fragment(w, r, "frag-timeline", t)
}

type workerData struct {
	Panel      readmodel.SessionPanel
	Dispatches []readmodel.DispatchRow
	Page       readmodel.Page
	Results    []map[string]any
	// Provider / Model are the project's configured worker (the target of
	// the connection test); Test is a just-run test's outcome.
	Provider, Model string
	Test            *readmodel.ProviderTest
}

// workerPage — GET /projects/{id}/worker (WBS 17.82–17.84).
func (c *Console) workerPage(w http.ResponseWriter, r *http.Request) {
	c.renderWorker(w, r, nil)
}

// workerTest — POST /projects/{id}/worker/test: sends the connection-test
// prompt to the project's worker model.
func (c *Console) workerTest(w http.ResponseWriter, r *http.Request) {
	var st struct {
		Provider string `json:"worker_provider"`
		Model    string `json:"worker_model"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings", &st)
	c.renderWorker(w, r, c.providerTest(w, r, "worker", st.Provider, st.Model))
}

func (c *Console) renderWorker(w http.ResponseWriter, r *http.Request, test *readmodel.ProviderTest) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d := workerData{Test: test}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/console/worker", &d.Panel)
	var disp struct {
		Dispatches []readmodel.DispatchRow `json:"dispatches"`
		Page       readmodel.Page          `json:"page"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/console/dispatches?"+qs(r, "limit", "offset"), &disp)
	d.Dispatches, d.Page = disp.Dispatches, disp.Page
	var res struct {
		Results []map[string]any `json:"results"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/worker/results?limit=50", &res)
	d.Results = res.Results
	var st struct {
		Provider string `json:"worker_provider"`
		Model    string `json:"worker_model"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings", &st)
	d.Provider, d.Model = st.Provider, st.Model
	c.render(w, r, http.StatusOK, "worker", viewmodels.Page{Shell: c.projectShell(r, "Worker", ov), Data: d})
}

type compressorData struct {
	Settings map[string]any
	Provider *readmodel.ProviderInfo
	Metrics  *compressionMetrics
	Runs     []compressionRunRow
	Page     readmodel.Page
	Attempts []compressionAttemptRow
	Test     *readmodel.ProviderTest
}

type compressionMetrics struct {
	Runs             int     `json:"runs"`
	Compressed       int     `json:"prompts_compressed"`
	Bypassed         int     `json:"prompts_bypassed"`
	Fallback         int     `json:"prompts_fallback"`
	Failed           int     `json:"prompts_failed"`
	Attempts         int     `json:"attempts"`
	AverageReduction float64 `json:"average_reduction_percent"`
}

type compressionRunRow struct {
	ID              string `json:"id"`
	PromptID        string `json:"canonical_prompt_id"`
	Outcome         string `json:"outcome"`
	Model           string `json:"model"`
	Attempts        int    `json:"attempts"`
	CanonicalTokens int    `json:"canonical_tokens"`
	TargetTokens    int    `json:"target_tokens"`
	Reason          string `json:"reason"`
	CreatedAt       string `json:"created_at"`
}

type compressionAttemptRow struct {
	RunID        string `json:"run_id"`
	Attempt      int    `json:"attempt"`
	Status       string `json:"status"`
	Model        string `json:"model"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	DurationMS   int64  `json:"duration_ms"`
	Error        string `json:"error_message"`
	CreatedAt    string `json:"created_at"`
}

// compressorPage — GET /projects/{id}/compressor: the compression
// provider, its run history and latest attempts.
func (c *Console) compressorPage(w http.ResponseWriter, r *http.Request) {
	c.renderCompressor(w, r, nil)
}

// compressorTest — POST /projects/{id}/compressor/test: sends the
// connection-test prompt to the project's compression model.
func (c *Console) compressorTest(w http.ResponseWriter, r *http.Request) {
	var st struct {
		Provider string `json:"compression_provider"`
		Model    string `json:"compression_model"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings", &st)
	c.renderCompressor(w, r, c.providerTest(w, r, "compression", st.Provider, st.Model))
}

func (c *Console) renderCompressor(w http.ResponseWriter, r *http.Request, test *readmodel.ProviderTest) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d := compressorData{Test: test}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/settings/compression", &d.Settings)
	if name, _ := d.Settings["provider"].(string); name != "" {
		_, comps := c.providerLists(r)
		for i := range comps {
			if comps[i].Name == name {
				d.Provider = &comps[i]
			}
		}
	}
	var m compressionMetrics
	if c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/compression-metrics", &m).OK() {
		d.Metrics = &m
	}
	var runs struct {
		Runs []compressionRunRow `json:"runs"`
		Page readmodel.Page      `json:"page"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/compression-runs?"+qs(r, "limit", "offset"), &runs)
	d.Runs, d.Page = runs.Runs, runs.Page
	var atts struct {
		Attempts []compressionAttemptRow `json:"attempts"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/compression-attempts?limit=50", &atts)
	d.Attempts = atts.Attempts
	c.render(w, r, http.StatusOK, "compressor", viewmodels.Page{Shell: c.projectShell(r, "Compressor", ov), Data: d})
}

// providerTest runs the connection test through the API. The model's reply
// can take longer than the server's write timeout, so the deadline is
// extended for this response.
func (c *Console) providerTest(w http.ResponseWriter, r *http.Request, kind, provider, model string) *readmodel.ProviderTest {
	out := &readmodel.ProviderTest{Kind: kind, Provider: provider, Model: model}
	if provider == "" || model == "" {
		out.Error = "No " + kind + " provider and model are configured for this project."
		return out
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(11 * time.Minute))
	resp := c.d.API.Do(r, http.MethodPost, "/api/v1/providers/"+url.PathEscape(kind)+"/"+url.PathEscape(provider)+"/test", map[string]string{"model": model}, "")
	if !resp.OK() {
		_, msg, _ := resp.Error()
		out.Error = msg
		return out
	}
	if err := resp.Decode(out); err != nil {
		out.Error = "The test response could not be read."
	}
	return out
}

// resultPage — GET /projects/{id}/results/{result} (WBS 17.84 / 17.85).
func (c *Console) resultPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var res readmodel.WorkerResult
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/results/"+url.PathEscape(r.PathValue("result")), &res) {
		return
	}
	c.render(w, r, http.StatusOK, "result", viewmodels.Page{Shell: c.projectShell(r, "Worker result", ov), Data: res})
}

// continuationsPage — GET /projects/{id}/continuations (WBS 17.86 / 17.87).
func (c *Console) continuationsPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var l struct {
		Continuations []readmodel.ContinuationRow `json:"continuations"`
	}
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/continuations", &l) {
		return
	}
	c.render(w, r, http.StatusOK, "continuations", viewmodels.Page{Shell: c.projectShell(r, "Continuations", ov), Data: l.Continuations})
}

type validationsData struct {
	Runs  []readmodel.ValidationRun
	Fixes []readmodel.FixAttempt
}

// validationsPage — GET /projects/{id}/validations (WBS 17.88–17.92).
func (c *Console) validationsPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var v struct {
		Validations []readmodel.ValidationRun `json:"validations"`
	}
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/validations", &v) {
		return
	}
	var f struct {
		FixAttempts []readmodel.FixAttempt `json:"fix_attempts"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/console/fix-attempts", &f)
	c.render(w, r, http.StatusOK, "validations", viewmodels.Page{Shell: c.projectShell(r, "Validation", ov), Data: validationsData{Runs: v.Validations, Fixes: f.FixAttempts}})
}

// validationPage — GET /projects/{id}/validations/{run} (WBS 17.89, 17.144).
func (c *Console) validationPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var d readmodel.ValidationDetail
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/validations/"+url.PathEscape(r.PathValue("run")), &d) {
		return
	}
	c.render(w, r, http.StatusOK, "validation", viewmodels.Page{Shell: c.projectShell(r, "Validation run", ov), Data: d})
}

type outputData struct {
	RunID    string
	Stream   string
	Offset   int
	End      int
	Total    int
	Text     string
	Complete bool
}

// outputPage — GET /projects/{id}/validations/{run}/output/{result}: one
// lazily loaded range (WBS 17.121 / 17.153).
func (c *Console) outputPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	stream := r.URL.Query().Get("stream")
	if stream == "" {
		stream = "stdout"
	}
	offset := atoi(r.URL.Query().Get("offset"))
	var o struct {
		Text       string `json:"text"`
		Total      int    `json:"total"`
		NextOffset int    `json:"next_offset"`
		Complete   bool   `json:"complete"`
	}
	path := "/api/v1/projects/" + pid(r) + "/validation-results/" + url.PathEscape(r.PathValue("result")) + "/output?stream=" +
		url.QueryEscape(stream) + "&offset=" + strconv.Itoa(offset) + "&length=16384"
	if !c.load(w, r, path, &o) {
		return
	}
	c.render(w, r, http.StatusOK, "output", viewmodels.Page{Shell: c.projectShell(r, "Output", ov), Data: outputData{RunID: r.PathValue("run"),
		Stream: stream, Offset: offset, End: o.NextOffset, Total: o.Total, Text: o.Text, Complete: o.Complete}})
}

// checkpointPage — GET /projects/{id}/checkpoints/{checkpoint} (WBS 17.93,
// 17.145).
func (c *Console) checkpointPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var cp readmodel.Checkpoint
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/checkpoints/"+url.PathEscape(r.PathValue("checkpoint")), &cp) {
		return
	}
	c.render(w, r, http.StatusOK, "checkpoint", viewmodels.Page{Shell: c.projectShell(r, "Checkpoint", ov), Data: cp})
}

type repositoryData struct {
	Status      readmodel.RepositoryStatus
	Checkpoints []readmodel.Checkpoint
}

// repositoryPage — GET /projects/{id}/repository (WBS 17.94 / 17.95).
func (c *Console) repositoryPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	d := repositoryData{}
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/repository", &d.Status) {
		return
	}
	var cps struct {
		Checkpoints []readmodel.Checkpoint `json:"checkpoints"`
	}
	c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/console/checkpoints", &cps)
	d.Checkpoints = cps.Checkpoints
	c.render(w, r, http.StatusOK, "repository", viewmodels.Page{Shell: c.projectShell(r, "Repository", ov), Data: d})
}

// recoveryPage — GET /projects/{id}/executions/{execution}/recovery
// (WBS 17.96–17.99).
func (c *Console) recoveryPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var rec readmodel.Recovery
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/executions/"+url.PathEscape(r.PathValue("execution"))+"/recovery", &rec) {
		return
	}
	c.render(w, r, http.StatusOK, "recovery", viewmodels.Page{Shell: c.projectShell(r, "Recovery", ov), Data: rec})
}

// recoveryAction — POST .../recovery: a backend-approved typed action with
// confirmation + reason for high-impact ones (WBS 17.98 / 17.100).
func (c *Console) recoveryAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		c.htmlError(w, r, http.StatusBadRequest, "invalid_input", "The form could not be read.")
		return
	}
	dryRun := r.PostFormValue("dry_run") != ""
	body := map[string]any{"action": r.PostFormValue("action"), "reason": r.PostFormValue("reason"), "confirm": checked(r, "confirm"),
		"idempotency_key": r.PostFormValue("idempotency_key"), "evidence_hash": r.PostFormValue("evidence_hash"),
		"resolution": r.PostFormValue("resolution"), "dry_run": dryRun}
	path := "/api/v1/projects/" + pid(r) + "/executions/" + url.PathEscape(r.PathValue("execution")) + "/recovery/actions"
	resp := c.d.API.Do(r, http.MethodPost, path, body, r.PostFormValue("reason"))
	if resp.OK() && dryRun {
		// Dry run (WBS 18.184): show what would change; nothing changed.
		var out struct {
			DryRun struct {
				Records            []string `json:"records"`
				ExternalOperations []string `json:"external_operations"`
			} `json:"dry_run"`
		}
		_ = resp.Decode(&out)
		ov, ok := c.overview(w, r)
		if !ok {
			return
		}
		var rec readmodel.Recovery
		c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/executions/"+url.PathEscape(r.PathValue("execution"))+"/recovery", &rec)
		msg := "Dry run — nothing was changed. Would change: " + strings.Join(out.DryRun.Records, "; ") +
			". External operations: " + strings.Join(out.DryRun.ExternalOperations, "; ") + "."
		c.render(w, r, http.StatusOK, "recovery", viewmodels.Page{Shell: c.projectShell(r, "Recovery", ov), Data: rec, Message: msg})
		return
	}
	if !resp.OK() {
		ov, ok := c.overview(w, r)
		if !ok {
			return
		}
		var rec readmodel.Recovery
		c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/executions/"+url.PathEscape(r.PathValue("execution"))+"/recovery", &rec)
		_, msg, _ := resp.Error()
		c.render(w, r, resp.Status, "recovery", viewmodels.Page{Shell: c.projectShell(r, "Recovery", ov), Data: rec, Message: msg})
		return
	}
	redirect(w, r, "/projects/"+pid(r)+"/executions/"+url.PathEscape(r.PathValue("execution"))+"/recovery", "Recovery action performed.")
}

// fragRecovery — the recovery banner region.
func (c *Console) fragRecovery(w http.ResponseWriter, r *http.Request) {
	var rec readmodel.Recovery
	resp := c.d.API.Get(r, "/api/v1/projects/"+pid(r)+"/executions/"+url.PathEscape(r.URL.Query().Get("execution"))+"/recovery", &rec)
	if !resp.OK() {
		http.Error(w, "unavailable", resp.Status)
		return
	}
	c.fragment(w, r, "frag-recovery", rec)
}

// metricsPage — GET /projects/{id}/metrics (WBS 17.110–17.114).
func (c *Console) metricsPage(w http.ResponseWriter, r *http.Request) {
	ov, ok := c.overview(w, r)
	if !ok {
		return
	}
	var m readmodel.Metrics
	if !c.load(w, r, "/api/v1/projects/"+pid(r)+"/console/metrics", &m) {
		return
	}
	c.render(w, r, http.StatusOK, "metrics", viewmodels.Page{Shell: c.projectShell(r, "Metrics", ov), Data: m})
}
