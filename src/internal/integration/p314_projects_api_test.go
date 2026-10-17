package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// projectAPIBody is the expected stable JSON shape of a project resource.
type projectAPIBody struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status"`
}

// listAPIBody wraps the list response.
type listAPIBody struct {
	Projects []projectAPIBody `json:"projects"`
}

func doJSON(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s %s: non-JSON body %q", method, url, raw)
		}
	}
	return resp.StatusCode, out
}

func createViaAPI(t *testing.T, h *Harness, name, desc string) projectAPIBody {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.BaseURL+"/api/v1/projects",
		bytes.NewReader([]byte(`{"name":"`+name+`","description":"`+desc+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /api/v1/projects = %d, want 201 (body %q)", resp.StatusCode, raw)
	}
	var body projectAPIBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.ID == "" || body.Name == "" {
		t.Fatalf("201 body missing fields: %+v", body)
	}
	return body
}

// T3.14.1 — POST creates project: 201 with the project resource.
func TestT3141PostCreatesProject(t *testing.T) {
	h := NewHarness(t)
	body := createViaAPI(t, h, "api project", "created via REST")
	var status string
	ctx := context.Background()
	if err := h.DBScan(ctx, "SELECT status FROM projects WHERE id = '"+body.ID+"'", &status); err != nil {
		t.Fatalf("no persisted project: %v", err)
	}
	if status != "DRAFT" {
		t.Fatalf("status = %q, want DRAFT", status)
	}
}

// T3.14.2 — Invalid POST returns 400: empty name and malformed JSON.
func TestT3142InvalidPost400(t *testing.T) {
	h := NewHarness(t)
	for name, body := range map[string]string{
		"empty name":     `{"name":"","description":"x"}`,
		"missing name":   `{"description":"x"}`,
		"malformed json": `{not json`,
	} {
		status, _ := doJSON(t, http.MethodPost, h.BaseURL+"/api/v1/projects", body)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: POST = %d, want 400", name, status)
		}
	}
	var count int
	if err := h.DBScan(context.Background(), "SELECT COUNT(*) FROM projects", &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid POST persisted %d projects", count)
	}
}

// T3.14.3 — GET project returns 200 with the stored fields.
func TestT3143GetProject200(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "fetch me", "desc")
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+created.ID, "")
	if status != http.StatusOK {
		t.Fatalf("GET project = %d, want 200", status)
	}
	if out["id"] != created.ID || out["name"] != "fetch me" || out["status"] != "DRAFT" {
		t.Fatalf("GET body mismatch: %+v", out)
	}
}

// T3.14.4 — Unknown project returns 404.
func TestT3144UnknownProject404(t *testing.T) {
	h := NewHarness(t)
	status, _ := doJSON(t, http.MethodGet,
		h.BaseURL+"/api/v1/projects/00000000-0000-0000-0000-000000000000", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET unknown = %d, want 404", status)
	}
}

// T3.14.5 — List returns 200 and includes the created project.
func TestT3145List200(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "in the list", "")
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects", "")
	if status != http.StatusOK {
		t.Fatalf("GET list = %d, want 200", status)
	}
	list, ok := out["projects"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("list body = %+v, want exactly 1 project", out)
	}
	first, _ := list[0].(map[string]any)
	if first["id"] != created.ID {
		t.Fatalf("list = %+v, want created project %s", first, created.ID)
	}
}

// T3.14.6 — PATCH updates supported fields (name, description).
func TestT3146PatchUpdatesFields(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "before", "old")
	status, _ := doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+created.ID,
		`{"name":"after","description":"new","version":1}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH = %d, want 200", status)
	}
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+created.ID, "")
	if status != http.StatusOK || out["name"] != "after" || out["description"] != "new" {
		t.Fatalf("after PATCH: status=%d body=%+v", status, out)
	}
	var name string
	if err := h.DBScan(context.Background(), "SELECT name FROM projects WHERE id = '"+created.ID+"'", &name); err != nil {
		t.Fatal(err)
	}
	if name != "after" {
		t.Fatalf("persisted name = %q, want after", name)
	}
}

// T3.14.7 — Invalid PATCH rejected: empty name and status changes are 400,
// and a rejected PATCH leaves the resource untouched.
func TestT3147InvalidPatchRejected(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "stable", "")
	for name, body := range map[string]string{
		"empty name":   `{"name":""}`,
		"status field": `{"status":"RUNNING"}`,
	} {
		status, _ := doJSON(t, http.MethodPatch, h.BaseURL+"/api/v1/projects/"+created.ID, body)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: PATCH = %d, want 400", name, status)
		}
	}
	status, out := doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+created.ID, "")
	if status != http.StatusOK || out["name"] != "stable" || out["status"] != "DRAFT" {
		t.Fatalf("rejected PATCH altered resource: status=%d body=%+v", status, out)
	}
}

// T3.14.8 — DELETE eligible project succeeds: 204, then the project is gone.
func TestT3148DeleteEligible(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "deletable", "")
	status, _ := doJSON(t, http.MethodDelete, h.BaseURL+"/api/v1/projects/"+created.ID, "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204", status)
	}
	status, _ = doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+created.ID, "")
	if status != http.StatusNotFound {
		t.Fatalf("GET after delete = %d, want 404", status)
	}
}

// T3.14.9 — DELETE protected project returns conflict (409); it survives.
func TestT3149DeleteProtectedConflict(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "protected", "")
	if _, err := h.DBExec(context.Background(),
		"UPDATE projects SET status = 'RUNNING' WHERE id = '"+created.ID+"'"); err != nil {
		t.Fatal(err)
	}
	status, _ := doJSON(t, http.MethodDelete, h.BaseURL+"/api/v1/projects/"+created.ID, "")
	if status != http.StatusConflict {
		t.Fatalf("DELETE protected = %d, want 409", status)
	}
	status, _ = doJSON(t, http.MethodGet, h.BaseURL+"/api/v1/projects/"+created.ID, "")
	if status != http.StatusOK {
		t.Fatalf("protected project lost: GET = %d, want 200", status)
	}
}
