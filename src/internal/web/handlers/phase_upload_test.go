package handlers

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"overseer/internal/readmodel"
	"overseer/internal/web"
)

// failingAppendAPI fails the failAt-th phase append (1-based; 0 = never).
type failingAppendAPI struct {
	*fakeAPI
	failAt  int
	appends int
}

func (f *failingAppendAPI) Do(r *http.Request, method, path string, body any, ifMatch string) web.APIResponse {
	resp := f.fakeAPI.Do(r, method, path, body, ifMatch)
	if method == http.MethodPost && strings.HasSuffix(path, "/phases") {
		f.appends++
		if f.appends == f.failAt {
			return errResp(400, "invalid_input", "name already used", nil)
		}
	}
	return resp
}

func uploadRequest(t *testing.T, files map[string]string, order []string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, name := range order {
		fw, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write([]byte(files[name]))
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/projects/"+tpid+"/phases/wbs-upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "text/html")
	return req
}

func uploadRig(t *testing.T, failAt int) (*failingAppendAPI, *http.ServeMux) {
	base := &fakeAPI{get: map[string]web.APIResponse{}, do: map[string]web.APIResponse{}}
	base.get["/api/v1/projects/"+tpid+"/overview"] = jsonResp(200, overviewFixture())
	base.get["/api/v1/projects/"+tpid+"/console/phases"] = jsonResp(200, map[string]any{"phases": []readmodel.PhaseRow{}})
	base.do["POST /api/v1/projects/"+tpid+"/phases"] = jsonResp(201, map[string]any{"id": "p"})
	api := &failingAppendAPI{fakeAPI: base, failAt: failAt}
	c, err := New(Deps{API: api, CSRF: func(*http.Request) string { return "tok" }, Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	web.SetHTMLErrors(c.htmlError)
	for _, rt := range c.routes() {
		mux.HandleFunc(rt.method+" "+rt.pattern, rt.h)
	}
	return api, mux
}

func appended(api *failingAppendAPI) []string {
	var out []string
	for i, call := range api.calls {
		if call == "POST /api/v1/projects/"+tpid+"/phases" {
			b := api.bodies[i].(map[string]any)
			out = append(out, b["name"].(string)+"="+b["wbs"].(string))
		}
	}
	return out
}

func TestUploadPhasesAppendsInFileNameOrder(t *testing.T) {
	api, mux := uploadRig(t, 0)
	files := map[string]string{"02-Storage.md": "# storage", "01-Core.txt": "# core", "03-API.MD": "# api"}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadRequest(t, files, []string{"02-Storage.md", "03-API.MD", "01-Core.txt"}))
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "Appended+3+phases") {
		t.Fatalf("%d %s\n%s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if got := strings.Join(appended(api), ","); got != "01-Core=# core,02-Storage=# storage,03-API=# api" {
		t.Fatalf("appended %s", got)
	}
}

func TestUploadPhasesChecksEveryFileFirst(t *testing.T) {
	api, mux := uploadRig(t, 0)
	files := map[string]string{"a.md": "# a", "b.pdf": "%PDF", "c.md": "  \n", "d.md": "\xff\xfe"}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadRequest(t, files, []string{"a.md", "b.pdf", "c.md", "d.md"}))
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "No phases were appended.") ||
		!strings.Contains(body, "b.pdf: only .md or .txt") || !strings.Contains(body, "c.md: the file is empty") ||
		!strings.Contains(body, "d.md: the file is not UTF-8 text") {
		t.Fatalf("%d\n%s", rec.Code, body)
	}
	if len(appended(api)) != 0 {
		t.Fatal("a phase was appended despite an invalid file")
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadRequest(t, nil, nil))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Choose or drop at least one") {
		t.Fatalf("no files: %d", rec.Code)
	}
}

func TestUploadPhasesReportsPartialFailure(t *testing.T) {
	api, mux := uploadRig(t, 2)
	files := map[string]string{"1.md": "# one", "2.md": "# two", "3.md": "# three"}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, uploadRequest(t, files, []string{"1.md", "2.md", "3.md"}))
	body := rec.Body.String()
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "Appended 1: 1.") ||
		!strings.Contains(body, "2.md was not appended: name already used") || !strings.Contains(body, "remaining files were not uploaded") {
		t.Fatalf("%d\n%s", rec.Code, body)
	}
	if n := len(appended(api)); n != 2 {
		t.Fatalf("append attempts %d, want 2 (stop after the failure)", n)
	}
}

func TestPhasesPageOffersMultiFileUpload(t *testing.T) {
	_, mux := uploadRig(t, 0)
	rec := serve(mux, http.MethodGet, "/projects/"+tpid+"/phases", nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `action="/projects/`+tpid+`/phases/wbs-upload" enctype="multipart/form-data"`) ||
		!strings.Contains(body, `name="files" type="file" multiple`) {
		t.Fatalf("%d\n%s", rec.Code, body)
	}
}

func orderRig(t *testing.T) (*fakeAPI, *http.ServeMux) {
	api, mux := uploadRig(t, 0)
	api.get["/api/v1/projects/"+tpid+"/console/phases"] = jsonResp(200, map[string]any{"phases": []readmodel.PhaseRow{
		{ID: "a", Order: 1, Name: "Done", Status: "DONE", LockReason: "the phase has started (DONE)"},
		{ID: "b", Order: 2, Name: "B", Status: "PENDING", Movable: true},
		{ID: "c", Order: 3, Name: "Running", Status: "EXECUTING", LockReason: "the phase has started (EXECUTING)"},
		{ID: "d", Order: 4, Name: "D", Status: "PENDING", Movable: true},
	}})
	api.do["PUT /api/v1/projects/"+tpid+"/phases/order"] = jsonResp(200, map[string]any{"phases": []any{}})
	return api.fakeAPI, mux
}

func lastOrder(api *fakeAPI) string {
	for i := len(api.bodies) - 1; i >= 0; i-- {
		if b, ok := api.bodies[i].(map[string]any); ok {
			if ids, ok := b["phase_ids"].([]string); ok {
				return strings.Join(ids, ",")
			}
		}
	}
	return ""
}

func TestReorderPhasesMoveSkipsLockedPhases(t *testing.T) {
	api, mux := orderRig(t)
	// "Move down" swaps with the next movable phase, past the running one.
	rec := serve(mux, http.MethodPost, "/projects/"+tpid+"/phases/order", url.Values{"move": {"b:down"}, "phase_ids": {"a", "b", "c", "d"}})
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "Phase+order+saved") || lastOrder(api) != "a,d,c,b" {
		t.Fatalf("%d %s order %s", rec.Code, rec.Header().Get("Location"), lastOrder(api))
	}
	for move, msg := range map[string]string{"b:up": "cannot move up any further", "c:up": "Running cannot move: the phase has started (EXECUTING)", "x:up": "Unknown move"} {
		rec = serve(mux, http.MethodPost, "/projects/"+tpid+"/phases/order", url.Values{"move": {move}})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), msg) {
			t.Fatalf("%s: %d\n%s", move, rec.Code, rec.Body)
		}
	}
}

func TestReorderPhasesDragOrderAndServerRejection(t *testing.T) {
	api, mux := orderRig(t)
	rec := serve(mux, http.MethodPost, "/projects/"+tpid+"/phases/order", url.Values{"phase_ids": {"a", "d", "c", "b"}})
	if rec.Code != http.StatusSeeOther || lastOrder(api) != "a,d,c,b" {
		t.Fatalf("%d %s", rec.Code, lastOrder(api))
	}
	api.do["PUT /api/v1/projects/"+tpid+"/phases/order"] = errResp(409, "conflict", "phase 3 — Running cannot move: the phase has started (EXECUTING)", nil)
	rec = serve(mux, http.MethodPost, "/projects/"+tpid+"/phases/order", url.Values{"phase_ids": {"a", "c", "b", "d"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "The phase order was not changed: phase 3 — Running cannot move") {
		t.Fatalf("%d\n%s", rec.Code, rec.Body)
	}
}

func TestPhasesPageOffersReordering(t *testing.T) {
	_, mux := orderRig(t)
	body := serve(mux, http.MethodGet, "/projects/"+tpid+"/phases", nil).Body.String()
	if !strings.Contains(body, `action="/projects/`+tpid+`/phases/order"`) || !strings.Contains(body, `value="b:down"`) ||
		strings.Contains(body, `value="c:up"`) || !strings.Contains(body, `data-phase-id="b" data-movable`) ||
		strings.Contains(body, `data-phase-id="c" data-movable`) || !strings.Contains(body, "Cannot move: the phase has started (EXECUTING)") {
		t.Fatalf("%s", body)
	}
}
