package integration

import (
	"context"
	"net/http"
	"testing"
)

// P3-17 phase editing rules (WBS 3.17, T3.17.1-T3.17.5): pending phases are
// editable via PATCH; executing/done phases are protected; editing never
// changes phase order.

// T3.17.1 — Pending phase name editable.
func TestT3171PendingNameEditable(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p17 name", "")
	_, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Orig"}`)
	id := out["id"].(string)
	status, updated := doJSON(t, http.MethodPatch, phasesURL(t, h, created.ID, id), `{"name":"Renamed"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH pending name = %d, want 200", status)
	}
	if updated["name"] != "Renamed" {
		t.Fatalf("response name = %v, want Renamed", updated["name"])
	}
	var name string
	if err := h.DBScan(context.Background(),
		"SELECT name FROM phases WHERE id = '"+id+"'", &name); err != nil {
		t.Fatal(err)
	}
	if name != "Renamed" {
		t.Fatalf("stored name = %q, want Renamed", name)
	}
}

// T3.17.2 — Pending phase description editable.
func TestT3172PendingDescriptionEditable(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p17 desc", "")
	_, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Orig"}`)
	id := out["id"].(string)
	status, updated := doJSON(t, http.MethodPatch, phasesURL(t, h, created.ID, id), `{"description":"New description"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH pending description = %d, want 200", status)
	}
	if updated["description"] != "New description" {
		t.Fatalf("response description = %v, want New description", updated["description"])
	}
	var desc string
	if err := h.DBScan(context.Background(),
		"SELECT description FROM phases WHERE id = '"+id+"'", &desc); err != nil {
		t.Fatal(err)
	}
	if desc != "New description" {
		t.Fatalf("stored description = %q, want New description", desc)
	}
}

// T3.17.3 — Executing phase protected.
func TestT3173ExecutingPhaseProtected(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p17 executing", "")
	_, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Orig"}`)
	id := out["id"].(string)
	if _, err := h.DBExec(context.Background(),
		"UPDATE phases SET status = 'EXECUTING', started_at = now() WHERE id = '"+id+"'"); err != nil {
		t.Fatal(err)
	}
	status, _ := doJSON(t, http.MethodPatch, phasesURL(t, h, created.ID, id), `{"name":"Nope"}`)
	if status != http.StatusConflict {
		t.Fatalf("PATCH executing phase = %d, want 409", status)
	}
	var name string
	if err := h.DBScan(context.Background(),
		"SELECT name FROM phases WHERE id = '"+id+"'", &name); err != nil {
		t.Fatal(err)
	}
	if name != "Orig" {
		t.Fatalf("executing phase name = %q, want Orig (unmutated)", name)
	}
}

// T3.17.4 — DONE phase protected.
func TestT3174DonePhaseProtected(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p17 done", "")
	_, out := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"Orig"}`)
	id := out["id"].(string)
	if _, err := h.DBExec(context.Background(),
		"UPDATE phases SET status = 'DONE', completed_at = now() WHERE id = '"+id+"'"); err != nil {
		t.Fatal(err)
	}
	status, _ := doJSON(t, http.MethodPatch, phasesURL(t, h, created.ID, id), `{"name":"Nope"}`)
	if status != http.StatusConflict {
		t.Fatalf("PATCH done phase = %d, want 409", status)
	}
	var name string
	if err := h.DBScan(context.Background(),
		"SELECT name FROM phases WHERE id = '"+id+"'", &name); err != nil {
		t.Fatal(err)
	}
	if name != "Orig" {
		t.Fatalf("done phase name = %q, want Orig (unmutated)", name)
	}
}

// T3.17.5 — Editing a future pending phase doesn't change order.
func TestT3175EditDoesNotChangeOrder(t *testing.T) {
	h := NewHarness(t)
	created := createViaAPI(t, h, "p17 order", "")
	for _, name := range []string{"First", "Second"} {
		if status, _ := doJSON(t, http.MethodPost, phasesURL(t, h, created.ID), `{"name":"`+name+`"}`); status != http.StatusCreated {
			t.Fatalf("POST %s = %d, want 201", name, status)
		}
	}
	// Drive order 1 to EXECUTING so order 2 is a future pending phase.
	if _, err := h.DBExec(context.Background(),
		"UPDATE phases SET status = 'EXECUTING', started_at = now() WHERE project_id = '"+created.ID+"' AND phase_order = 1"); err != nil {
		t.Fatal(err)
	}
	var targetID string
	if err := h.DBScan(context.Background(),
		"SELECT id::text FROM phases WHERE project_id = '"+created.ID+"' AND phase_order = 2", &targetID); err != nil {
		t.Fatal(err)
	}
	status, _ := doJSON(t, http.MethodPatch, phasesURL(t, h, created.ID, targetID), `{"name":"Edited","description":"also edited"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH future pending phase = %d, want 200", status)
	}
	var order int
	if err := h.DBScan(context.Background(),
		"SELECT phase_order FROM phases WHERE id = '"+targetID+"'", &order); err != nil {
		t.Fatal(err)
	}
	if order != 2 {
		t.Fatalf("order after edit = %d, want 2 (unchanged)", order)
	}
}
