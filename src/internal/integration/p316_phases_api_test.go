package integration

import (
	"context"
	"net/http"
	"testing"
)

func phasesURL(t *testing.T, h *Harness, projectID string, phaseID ...string) string {
	t.Helper()
	u := h.BaseURL + "/api/v1/projects/" + projectID + "/phases"
	if len(phaseID) > 0 {
		u += "/" + phaseID[0]
	}
	return u
}

// T3.16.1 — First phase receives order 1.
func TestT3161FirstPhaseOrderOne(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "phases first", "")
	status, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID),
		`{"name":"Phase A","description":"first"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST first phase = %d, want 201", status)
	}
	if out["order"] != float64(1) {
		t.Fatalf("created phase order = %v, want 1", out["order"])
	}
	var order int
	if err := h.DBScan(context.Background(),
		"SELECT phase_order FROM phases WHERE project_id = '"+created.ID+"'", &order); err != nil {
		t.Fatal(err)
	}
	if order != 1 {
		t.Fatalf("stored order = %d, want 1", order)
	}
}

// T3.16.2 — Additional phase appends.
func TestT3162PhasesAppend(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "phases append", "")
	for i, name := range []string{"Alpha", "Beta", "Gamma"} {
		status, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID),
			`{"name":"`+name+`"}`)
		if status != http.StatusCreated {
			t.Fatalf("POST %s = %d, want 201", name, status)
		}
		want := float64(i + 1)
		if out["order"] != want {
			t.Fatalf("POST %s order = %v, want %v", name, out["order"], want)
		}
	}
	status, list := doJSON(t, http.MethodGet, phasesURL(t, h, created.ID), "")
	if status != http.StatusOK {
		t.Fatalf("GET list = %d, want 200", status)
	}
	phases, ok := list["phases"].([]any)
	if !ok || len(phases) != 3 {
		t.Fatalf("GET list = %+v, want 3 phases", list["phases"])
	}
}

// T3.16.3 — Phase list ordered.
func TestT3163ListOrdered(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "phases ordered", "")
	for _, name := range []string{"One", "Two", "Three"} {
		if status, _ := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"`+name+`"}`); status != http.StatusCreated {
			t.Fatalf("POST %s = %d, want 201", name, status)
		}
	}
	status, list := doJSON(t, http.MethodGet, phasesURL(t, h, created.ID), "")
	if status != http.StatusOK {
		t.Fatalf("GET list = %d, want 200", status)
	}
	phases := list["phases"].([]any)
	var names []string
	var orders []int
	for _, item := range phases {
		m := item.(map[string]any)
		names = append(names, m["name"].(string))
		orders = append(orders, int(m["order"].(float64)))
	}
	wantNames := []string{"One", "Two", "Three"}
	wantOrders := []int{1, 2, 3}
	for i := range wantNames {
		if names[i] != wantNames[i] || orders[i] != wantOrders[i] {
			t.Fatalf("list = %v (orders %v), want %v (orders %v)", names, orders, wantNames, wantOrders)
		}
	}
}

// T3.16.4 — User-supplied order ignored/rejected according to API contract.
// The contract here rejects it: order is server-assigned, so it is not in the
// closed create field set and returns 400.
func TestT3164UserSuppliedOrderRejected(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "phases order", "")
	status, _ := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID),
		`{"name":"X","order":9}`)
	if status != http.StatusBadRequest {
		t.Fatalf("POST with order = %d, want 400", status)
	}
	status, _ = doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"order":5}`)
	if status != http.StatusBadRequest {
		t.Fatalf("POST order-only = %d, want 400 (name also required)", status)
	}
	var count int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM phases WHERE project_id = '"+created.ID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected creates persisted %d rows, want 0", count)
	}
}

// T3.16.5 — New phase while RUNNING appended to end.
func TestT3165AppendWhileRunning(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "phases running", "")
	for _, name := range []string{"A", "B"} {
		if status, _ := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"`+name+`"}`); status != http.StatusCreated {
			t.Fatalf("POST %s = %d, want 201", name, status)
		}
	}
	if _, err := h.DBExec(context.Background(),
		"UPDATE projects SET status = 'RUNNING' WHERE id = '"+created.ID+"'"); err != nil {
		t.Fatal(err)
	}
	status, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"C"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST while RUNNING = %d, want 201", status)
	}
	if out["order"] != float64(3) {
		t.Fatalf("appended order = %v, want 3 (max+1, end of list)", out["order"])
	}
}

// T3.16.6 — Current phase unchanged by append.
func TestT3166CurrentPhaseUnchanged(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	created := createViaAPI(t, h, "phases current", "")
	_, first := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"First"}`)
	_, _ = doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Second"}`)
	firstID := first["id"].(string)
	// Drive the first phase to EXECUTING: it is now the current phase.
	if _, err := h.DBExec(ctx,
		"UPDATE phases SET status = 'EXECUTING', started_at = now() WHERE id = '"+firstID+"'"); err != nil {
		t.Fatal(err)
	}
	status, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Third"}`)
	if status != http.StatusCreated {
		t.Fatalf("append = %d, want 201", status)
	}
	if out["order"] != float64(3) {
		t.Fatalf("append order = %v, want 3", out["order"])
	}
	var curStatus, curStarted string
	if err := h.DBScan(ctx,
		"SELECT status, started_at::text FROM phases WHERE id = '"+firstID+"'", &curStatus, &curStarted); err != nil {
		t.Fatal(err)
	}
	if curStatus != "EXECUTING" || curStarted == "" {
		t.Fatalf("current phase mutated by append: status=%q started_at=%q", curStatus, curStarted)
	}
}

// T3.16 regression — DELETE on an existing phase returns 204 and removes
// the row; the service must map a successful (nil) repository result to
// success, not to an infrastructure error (caught by TESTER live check).
func TestT316DeleteExistingPhase(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "phases delete", "")
	_, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"D"}`)
	id := out["id"].(string)
	status, _ := doJSON(t, http.MethodDelete, phasesURL(t, h, created.ID, id), "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE existing = %d, want 204", status)
	}
	var n int
	if err := h.DBScan(context.Background(),
		"SELECT count(*) FROM phases WHERE id = '"+id+"'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("phase row survives delete: count=%d", n)
	}
}

// T3.16.7 — Unknown project returns 404.
func TestT3167UnknownProject404(t *testing.T) {
	h := NewHarness(t)
	id := "00000000-0000-0000-0000-000000000000"
	status, _ := doJSON(t, http.MethodPost, phasesURL(t, h, id), `{"name":"X"}`)
	if status != http.StatusNotFound {
		t.Fatalf("POST unknown project = %d, want 404", status)
	}
	status, _ = doJSON(t, http.MethodGet, phasesURL(t, h, id), "")
	if status != http.StatusNotFound {
		t.Fatalf("GET unknown project = %d, want 404", status)
	}
}
