package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"overseer/internal/readmodel"
	"overseer/internal/web"
)

// fakeAPI answers in-process API calls from a table (P17-123: handlers are
// tested against a mocked backend).
type fakeAPI struct {
	get    map[string]web.APIResponse
	do     map[string]web.APIResponse
	calls  []string
	bodies []any
}

func jsonResp(status int, v any) web.APIResponse {
	b, _ := json.Marshal(v)
	return web.APIResponse{Status: status, Body: b, Header: http.Header{}}
}

func errResp(status int, code, msg string, fields map[string]string) web.APIResponse {
	return jsonResp(status, map[string]any{"error": map[string]any{"code": code, "message": msg, "fields": fields}})
}

func (f *fakeAPI) lookup(m map[string]web.APIResponse, key string) web.APIResponse {
	if r, ok := m[key]; ok {
		return r
	}
	// Prefix match (query strings).
	for k, r := range m {
		if strings.HasSuffix(k, "*") && strings.HasPrefix(key, strings.TrimSuffix(k, "*")) {
			return r
		}
	}
	return errResp(404, "not_found", "resource not found", nil)
}

func (f *fakeAPI) Do(_ *http.Request, method, path string, body any, _ string) web.APIResponse {
	f.calls = append(f.calls, method+" "+path)
	f.bodies = append(f.bodies, body)
	return f.lookup(f.do, method+" "+path)
}

func (f *fakeAPI) Get(r *http.Request, path string, v any) web.APIResponse {
	f.calls = append(f.calls, "GET "+path)
	resp := f.lookup(f.get, path)
	if resp.OK() && v != nil {
		_ = json.Unmarshal(resp.Body, v)
	}
	return resp
}

const tpid = "11111111-1111-4111-8111-111111111111"

func overviewFixture() readmodel.Overview {
	return readmodel.Overview{ProjectID: tpid, Name: "Trader <b>", Status: "RUNNING", Actions: []string{"PAUSE", "CANCEL"},
		Progress: readmodel.NewProgress(22, 30), Context: readmodel.NewContextUsage(39200, 64000, 0.8),
		Execution:   &readmodel.ExecutionSummary{ID: "e1", Number: 2, Status: "RUNNING", Active: true},
		Phase:       &readmodel.PhaseRef{ID: "p7", Order: 7, Name: "Execution Engine", Status: "EXECUTING"},
		CurrentUnit: &readmodel.UnitRef{ID: "u23", Key: "7.23", Name: "Context Snapshot", Status: "EXECUTING", Stage: readmodel.StageExecuting},
		Runtime:     readmodel.RuntimeRef{StateKey: "IMPLEMENT"}, Staleness: readmodel.Staleness{State: "STALE", Stale: true, CurrentWBS: 7, PlanSource: 6}}
}

func newConsole(t *testing.T, api *fakeAPI) (*Console, *http.ServeMux) {
	t.Helper()
	c, err := New(Deps{API: api, CSRF: func(*http.Request) string { return "tok" }, Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	web.SetHTMLErrors(c.htmlError)
	for _, rt := range c.routes() {
		mux.HandleFunc(rt.method+" "+rt.pattern, rt.h)
	}
	return c, mux
}

func serve(mux *http.ServeMux, method, path string, form url.Values) *httptest.ResponseRecorder {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// P17-123 (WBS 17.178) — success, not found, conflict, validation and
// forbidden responses map to the right pages.
func TestP17123HandlerOutcomes(t *testing.T) {
	api := &fakeAPI{get: map[string]web.APIResponse{}, do: map[string]web.APIResponse{}}
	_, mux := newConsole(t, api)
	ov := overviewFixture()
	api.get["/api/v1/projects/"+tpid+"/overview"] = jsonResp(200, ov)
	api.get["/api/v1/projects/"+tpid+"/console/flows"] = jsonResp(200, map[string]any{"flows": []readmodel.Flow{{Name: "Work unit"}}})

	// Success: values come from the read model; markup escaped; status text.
	rec := serve(mux, http.MethodGet, "/projects/"+tpid, nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "Trader &lt;b&gt;") || !strings.Contains(body, "22 / 30 tasks complete") ||
		!strings.Contains(body, "73.3%") || !strings.Contains(body, "61.3%") || !strings.Contains(body, "Execution Plan Stale") ||
		!strings.Contains(body, "IMPLEMENT") || !strings.Contains(body, `action="/projects/`+tpid+`/pause"`) || strings.Contains(body, "/resume") {
		t.Fatalf("overview: %d\n%s", rec.Code, body)
	}
	// Not found from the backend → 404 page, no internals.
	rec = serve(mux, http.MethodGet, "/projects/22222222-2222-4222-8222-222222222222", nil)
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "Not found") {
		t.Fatalf("404: %d", rec.Code)
	}
	// Forbidden from the API → 403 page.
	api.do["POST /api/v1/projects/"+tpid+"/pause"] = errResp(403, "FORBIDDEN", "not permitted", nil)
	rec = serve(mux, http.MethodPost, "/projects/"+tpid+"/pause", url.Values{})
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Not permitted") {
		t.Fatalf("403: %d", rec.Code)
	}
	// Lifecycle conflict → 409 page offering reload.
	api.do["POST /api/v1/projects/"+tpid+"/pause"] = errResp(409, "conflict", "invalid lifecycle transition", nil)
	rec = serve(mux, http.MethodPost, "/projects/"+tpid+"/pause", url.Values{})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "Reload the page") {
		t.Fatalf("409: %d", rec.Code)
	}
	// Success → post-redirect-get.
	api.do["POST /api/v1/projects/"+tpid+"/pause"] = jsonResp(200, map[string]any{})
	rec = serve(mux, http.MethodPost, "/projects/"+tpid+"/pause", url.Values{})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/projects/"+tpid) {
		t.Fatalf("PRG: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Validation: field errors beside fields, values preserved (422).
	api.get["/api/v1/providers"] = jsonResp(200, map[string]any{"providers": []readmodel.ProviderInfo{{Kind: "worker", Name: "hermes"}}})
	api.do["POST /api/v1/console/projects"] = errResp(400, "invalid_input", "Request is invalid.", map[string]string{"name": "required", "context_window": "must be a positive token count"})
	rec = serve(mux, http.MethodPost, "/projects", url.Values{"name": {""}, "description": {`"><script>`}})
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "must be a positive token count") || strings.Contains(rec.Body.String(), `"><script>`) {
		t.Fatalf("422: %d", rec.Code)
	}
	// WBS stale save → 409 conflict page, submitted text kept.
	api.get["/api/v1/projects/"+tpid+"/console/phases/p7"] = jsonResp(200, readmodel.PhaseDetail{Phase: readmodel.PhaseRow{ID: "p7", Order: 7, Name: "E"}})
	api.get["/api/v1/projects/"+tpid+"/phases/p7/wbs/versions"] = jsonResp(200, map[string]any{"versions": []map[string]any{{"version": 5, "content": "# v5"}}})
	api.do["PUT /api/v1/projects/"+tpid+"/phases/p7/wbs"] = errResp(409, "conflict", "WBS changed", nil)
	rec = serve(mux, http.MethodPost, "/projects/"+tpid+"/phases/p7/wbs", url.Values{"content": {"# mine"}, "expected_version": {"4"}})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "# mine") || !strings.Contains(rec.Body.String(), "Conflict — not saved") {
		t.Fatalf("WBS conflict: %d", rec.Code)
	}
}

// P17-77 / P17-123 (WBS 17.119 / 17.165) — every GET route is READ and
// every POST is delegated to the API (audited once there).
func TestP17123RouteTable(t *testing.T) {
	c, _ := newConsole(t, &fakeAPI{})
	for _, rt := range c.routes() {
		switch rt.method {
		case http.MethodGet:
			if rt.action != "READ" || rt.delegated {
				t.Fatalf("GET %s: %s", rt.pattern, rt.action)
			}
		case http.MethodPost:
			if rt.action == "READ" || !rt.delegated {
				t.Fatalf("POST %s: %s", rt.pattern, rt.action)
			}
		default:
			t.Fatalf("unexpected method %s", rt.method)
		}
	}
}

// P17-76 / P17-177 (WBS 17.147 / 17.177) — partials render textual status
// and survive missing optional data.
func TestP17177Partials(t *testing.T) {
	c, _ := newConsole(t, &fakeAPI{})
	var b strings.Builder
	for _, s := range []string{"PASS", "FAIL", "RUNNING", "BLOCKED", "WHATEVER"} {
		b.Reset()
		if err := c.rnd.Fragment(&b, "badge", s); err != nil || !strings.Contains(b.String(), s) {
			t.Fatalf("badge %s: %v %s", s, err, b.String())
		}
	}
	// Optional data absent: fragments render without panicking.
	for name, data := range map[string]any{"frag-overview": &readmodel.Overview{}, "frag-current-task": (*readmodel.CurrentTask)(nil),
		"frag-flows": []readmodel.Flow(nil), "frag-units": readmodel.UnitPage{}, "frag-timeline": readmodel.Timeline{},
		"frag-recovery": readmodel.Recovery{}, "frag-actions": actionsData{}} {
		b.Reset()
		if err := c.rnd.Fragment(&b, name, data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Append phase: a non-blank initial WBS (loaded from a dropped file) is
// forwarded so the phase and WBS v1 are created together; a blank one is
// omitted and the plain create path is used.
func TestAppendPhaseForwardsInitialWBS(t *testing.T) {
	api := &fakeAPI{get: map[string]web.APIResponse{}, do: map[string]web.APIResponse{}}
	_, mux := newConsole(t, api)
	api.do["POST /api/v1/projects/"+tpid+"/phases"] = jsonResp(201, map[string]any{"id": "p8"})
	for _, tc := range []struct {
		wbs     string
		wantWBS bool
	}{{"# Phase 8 WBS\n", true}, {"  \n", false}} {
		api.bodies = nil
		rec := serve(mux, http.MethodPost, "/projects/"+tpid+"/phases", url.Values{"name": {"phase-8"}, "description": {""}, "wbs": {tc.wbs}})
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("wbs %q: status %d", tc.wbs, rec.Code)
		}
		body, _ := api.bodies[len(api.bodies)-1].(map[string]any)
		got, ok := body["wbs"]
		if ok != tc.wantWBS || (ok && got != tc.wbs) {
			t.Fatalf("wbs %q: forwarded body %v", tc.wbs, body)
		}
	}
}

// Project delete: each list row links to the confirmation page only when
// the project is deletable; the POST requires confirmation + reason and
// delegates to DELETE /api/v1/projects/{id}.
func TestDeleteProjectFlow(t *testing.T) {
	api := &fakeAPI{get: map[string]web.APIResponse{}, do: map[string]web.APIResponse{}}
	_, mux := newConsole(t, api)
	other := "22222222-2222-4222-8222-222222222222"
	api.get["/api/v1/console/projects?*"] = jsonResp(200, readmodel.ProjectList{Rows: []readmodel.ProjectRow{
		{ID: tpid, Name: "Done", Status: "COMPLETED", Deletable: true},
		{ID: other, Name: "Live", Status: "RUNNING"},
	}})
	rec := serve(mux, http.MethodGet, "/projects", nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `href="/projects/`+tpid+`/delete"`) || strings.Contains(body, `href="/projects/`+other+`/delete"`) ||
		!strings.Contains(body, "Delete unavailable (RUNNING)") {
		t.Fatalf("list: %d\n%s", rec.Code, body)
	}

	ov := overviewFixture()
	ov.Status, ov.Deletable = "COMPLETED", true
	api.get["/api/v1/projects/"+tpid+"/overview"] = jsonResp(200, ov)
	if rec := serve(mux, http.MethodGet, "/projects/"+tpid+"/delete", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `action="/projects/`+tpid+`/delete"`) {
		t.Fatalf("confirm page: %d", rec.Code)
	}
	ov.Status, ov.Deletable = "RUNNING", false
	api.get["/api/v1/projects/"+tpid+"/overview"] = jsonResp(200, ov)
	if rec := serve(mux, http.MethodGet, "/projects/"+tpid+"/delete", nil); rec.Code != 200 || strings.Contains(rec.Body.String(), `action="/projects/`+tpid+`/delete"`) ||
		!strings.Contains(rec.Body.String(), "Cannot delete") {
		t.Fatalf("protected page: %d", rec.Code)
	}

	api.calls = nil
	if rec := serve(mux, http.MethodPost, "/projects/"+tpid+"/delete", url.Values{"reason": {"cleanup"}}); rec.Code != http.StatusBadRequest || len(api.calls) != 0 {
		t.Fatalf("unconfirmed: %d %v", rec.Code, api.calls)
	}
	api.do["DELETE /api/v1/projects/"+tpid] = web.APIResponse{Status: http.StatusNoContent, Header: http.Header{}}
	rec = serve(mux, http.MethodPost, "/projects/"+tpid+"/delete", url.Values{"reason": {"cleanup"}, "confirm": {"yes"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/projects?") {
		t.Fatalf("delete: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if api.calls[len(api.calls)-1] != "DELETE /api/v1/projects/"+tpid {
		t.Fatalf("calls: %v", api.calls)
	}
}

// Worker / Compressor tabs: the compressor page renders the provider, its
// run history and attempts; "Test connection" sends the project's provider
// and model to the API and shows the reply or the error.
func TestProviderTabsAndConnectionTest(t *testing.T) {
	api := &fakeAPI{get: map[string]web.APIResponse{}, do: map[string]web.APIResponse{}}
	_, mux := newConsole(t, api)
	p := "/api/v1/projects/" + tpid
	api.get[p+"/overview"] = jsonResp(200, overviewFixture())
	api.get[p+"/settings"] = jsonResp(200, map[string]any{"worker_provider": "hermes", "worker_model": "swift-qwen-3.8",
		"compression_provider": "local", "compression_model": "qwen-small"})
	api.get[p+"/settings/compression"] = jsonResp(200, map[string]any{"provider": "local", "model": "qwen-small", "mode": "AUTO", "enabled": true})
	api.get["/api/v1/providers"] = jsonResp(200, map[string]any{"providers": []readmodel.ProviderInfo{
		{Kind: "compression", Name: "local", ProviderKind: "openai_compatible", EndpointHost: "devpipe-compressor:8080"}}})
	api.get[p+"/compression-metrics"] = jsonResp(200, map[string]any{"runs": 3, "prompts_compressed": 2, "average_reduction_percent": 41.5})
	api.get[p+"/compression-runs?*"] = jsonResp(200, map[string]any{"runs": []map[string]any{{"id": "run-0001-x", "canonical_prompt_id": "cp-0001-x",
		"outcome": "COMPRESSED", "model": "qwen-small", "attempts": 1, "canonical_tokens": 9000, "target_tokens": 4000, "created_at": "2026-10-14T10:00:00Z"}},
		"page": readmodel.Page{Total: 1, Limit: 50}})
	api.get[p+"/compression-attempts?*"] = jsonResp(200, map[string]any{"attempts": []map[string]any{{"run_id": "run-0001-x", "attempt": 1,
		"status": "PROVIDER_ERROR", "error_message": "timeout <x>", "duration_ms": 1500}}})

	rec := serve(mux, http.MethodGet, "/projects/"+tpid+"/compressor", nil)
	body := rec.Body.String()
	for _, want := range []string{"devpipe-compressor:8080", "qwen-small", `href="/projects/` + tpid + `/prompts/cp-0001-x"`, "9000 → 4000",
		"PROVIDER_ERROR", "timeout &lt;x&gt;", "1.5s", "41.5%", `action="/projects/` + tpid + `/compressor/test"`, `href="/projects/` + tpid + `/compressor"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("compressor page: %d missing %q\n%s", rec.Code, want, body)
		}
	}

	api.do["POST /api/v1/providers/compression/local/test"] = jsonResp(200, readmodel.ProviderTest{Kind: "compression", Provider: "local",
		Model: "qwen-small", OK: true, Response: "I can <summarize>.", DurationMS: 800})
	api.bodies = nil
	rec = serve(mux, http.MethodPost, "/projects/"+tpid+"/compressor/test", url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "I can &lt;summarize&gt;.") {
		t.Fatalf("compressor test: %d\n%s", rec.Code, rec.Body.String())
	}
	if b, _ := api.bodies[0].(map[string]string); b["model"] != "qwen-small" {
		t.Fatalf("compressor test body: %v", api.bodies)
	}

	api.get[p+"/console/worker"] = jsonResp(200, readmodel.SessionPanel{})
	api.do["POST /api/v1/providers/worker/hermes/test"] = jsonResp(200, readmodel.ProviderTest{Kind: "worker", Provider: "hermes",
		Model: "swift-qwen-3.8", Error: "gateway unavailable"})
	rec = serve(mux, http.MethodPost, "/projects/"+tpid+"/worker/test", url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "gateway unavailable") || !strings.Contains(rec.Body.String(), "swift-qwen-3.8") {
		t.Fatalf("worker test: %d\n%s", rec.Code, rec.Body.String())
	}

	// No provider configured: no API call, an explanatory error.
	api.get[p+"/settings"] = jsonResp(200, map[string]any{})
	api.calls = nil
	rec = serve(mux, http.MethodPost, "/projects/"+tpid+"/worker/test", url.Values{})
	if !strings.Contains(rec.Body.String(), "No worker provider and model are configured") {
		t.Fatalf("unconfigured: %d\n%s", rec.Code, rec.Body.String())
	}
	for _, c := range api.calls {
		if strings.HasPrefix(c, "POST ") {
			t.Fatalf("unconfigured test called the API: %v", api.calls)
		}
	}
}
