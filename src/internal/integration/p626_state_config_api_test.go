package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	"overseer/internal/service"
)

// P6-26 (WBS 6.36): the state-configuration REST surface over the real
// HTTP server. The machine is seeded through the P6-17 repository helper
// (machine creation is a service-only op, the established P6-25 pattern);
// the role through the P6-07 helper.

// p626Seed seeds the standard fixture: a project, its machine (v1) with
// states A→B (A referenced by the transition, B referenced), and a
// project-scoped role.
func p626Seed(t *testing.T, h *Harness) (projectID, roleID string) {
	t.Helper()
	proj := p605Project(t, h)
	p617SeedMachine(t, h, proj, "p626m",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	role := p607Role(t, h, proj, "P626-ROLE")
	return proj, role
}

// T6.36.1 — State created: POST adds a state to the current machine; it
// appears in the machine read.
func TestP626CreateState(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	body, _ := json.Marshal(map[string]any{"key": " review ", "name": "Review", "kind": "VALIDATION"})
	status, out := doJSON(t, "POST", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine/states", string(body))
	if status != http.StatusCreated {
		t.Fatalf("POST states = %d, want 201 (body %v)", status, out)
	}
	if out["key"] != "REVIEW" {
		t.Fatalf("created state key = %v, want REVIEW (normalized)", out["key"])
	}
	if out["enabled"] != true {
		t.Fatalf("created state enabled = %v, want the default true", out["enabled"])
	}
	// The new state is part of the current machine definition.
	status, cur := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine", "")
	if status != 200 {
		t.Fatalf("GET machine = %d", status)
	}
	found := false
	for _, s := range cur["states"].([]any) {
		if s.(map[string]any)["key"] == "REVIEW" {
			found = true
		}
	}
	if !found {
		t.Fatalf("machine states = %v, want to include REVIEW", cur["states"])
	}
}

// T6.36.1 — create guards: unknown field rejected (400), missing required
// field rejected (400), unknown machine (404).
func TestP626CreateStateGuards(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/states"
	bad, _ := json.Marshal(map[string]any{"key": "X", "name": "X", "kind": "CONTROL", "bogus": true})
	if status, out := doJSON(t, "POST", base, string(bad)); status != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400 (body %v)", status, out)
	}
	missing, _ := json.Marshal(map[string]any{"key": "X", "kind": "CONTROL"})
	if status, out := doJSON(t, "POST", base, string(missing)); status != http.StatusBadRequest {
		t.Fatalf("missing name = %d, want 400 (body %v)", status, out)
	}
	ghost, _ := json.Marshal(map[string]any{"key": "X", "name": "X", "kind": "CONTROL"})
	ghostProj := createViaAPI(t, h, "p626-ghost", "no machine")
	if status, out := doJSON(t, "POST", h.BaseURL+"/api/v1/projects/"+ghostProj.ID+"/state-machine/states", string(ghost)); status != http.StatusNotFound {
		t.Fatalf("project without machine = %d, want 404 (body %v)", status, out)
	}
}

// T6.36.2 — State edited: PUT updates the definition (name, template
// reference, disable) and the machine read reflects it.
func TestP626EditState(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	statesURL := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/states"
	// Update name + prompt-template reference.
	body, _ := json.Marshal(map[string]any{"name": "Plan Phase", "prompt_template": "phase-plan"})
	status, out := doJSON(t, "PUT", statesURL+"/a", string(body))
	if status != http.StatusOK {
		t.Fatalf("PUT state a = %d (body %v)", status, out)
	}
	if out["name"] != "Plan Phase" || out["prompt_template"] != "PHASE-PLAN" {
		t.Fatalf("updated view = %v, want name + normalized template", out)
	}
	// Disable is an update with enabled=false.
	disable, _ := json.Marshal(map[string]any{"enabled": false})
	status, out = doJSON(t, "PUT", statesURL+"/a", string(disable))
	if status != http.StatusOK || out["enabled"] != false {
		t.Fatalf("disable a = %d %v, want 200 + disabled", status, out)
	}
	// Unknown state key → 404; unknown field → 400.
	if status, out := doJSON(t, "PUT", statesURL+"/nope", `{"name":"x"}`); status != http.StatusNotFound {
		t.Fatalf("unknown state = %d, want 404 (body %v)", status, out)
	}
	if status, out := doJSON(t, "PUT", statesURL+"/a", `{"nope":true}`); status != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400 (body %v)", status, out)
	}
}

// T6.36.3 — Role assigned: POST appends the role to the state's list; the
// machine read shows the assignment; a duplicate is a conflict; a missing
// role is a 404.
func TestP626AssignRole(t *testing.T) {
	h := NewHarness(t)
	proj, role := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/states"
	status, out := doJSON(t, "POST", base+"/a/roles/"+role, "")
	if status != http.StatusOK {
		t.Fatalf("assign role = %d (body %v)", status, out)
	}
	roles, _ := out["roles"].([]any)
	if len(roles) != 1 || roles[0] != "P626-ROLE" {
		t.Fatalf("roles = %v, want [P626-ROLE]", out["roles"])
	}
	// Duplicate (state, role) → 409.
	if status, out := doJSON(t, "POST", base+"/a/roles/"+role, ""); status != http.StatusConflict {
		t.Fatalf("duplicate assign = %d, want 409 (body %v)", status, out)
	}
	// Unknown role → 404.
	if status, out := doJSON(t, "POST", base+"/a/roles/00000000-0000-0000-0000-000000000001", ""); status != http.StatusNotFound {
		t.Fatalf("unknown role = %d, want 404 (body %v)", status, out)
	}
}

// T6.36.4 — State requirements configured: the instruction is set (one
// append-only version), the state-file list is replaced (order preserved,
// the prior set gone), and the prompt-template reference is updated.
func TestP626ConfigureRequirements(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/states"
	// Instruction: set twice → two versions, current = the second.
	status, out := doJSON(t, "PUT", base+"/a/instructions", `{"content":"first"}`)
	if status != http.StatusOK || out["instruction_version"] != float64(1) {
		t.Fatalf("set instruction = %d %v, want 200 + version 1", status, out)
	}
	status, out = doJSON(t, "PUT", base+"/a/instructions", `{"content":"second"}`)
	if status != http.StatusOK || out["instruction_version"] != float64(2) {
		t.Fatalf("second instruction = %d %v, want 200 + version 2", status, out)
	}
	// State files: replace — the new ordered list is exactly stored.
	files, _ := json.Marshal(map[string]any{"categories": []string{"REPORT", "PLAN"}})
	status, out = doJSON(t, "PUT", base+"/a/state-files", string(files))
	if status != http.StatusOK {
		t.Fatalf("set files = %d (body %v)", status, out)
	}
	gotFiles, _ := out["state_files"].([]any)
	if len(gotFiles) != 2 || gotFiles[0] != "REPORT" || gotFiles[1] != "PLAN" {
		t.Fatalf("state_files = %v, want [REPORT PLAN] in order", out["state_files"])
	}
	// Empty list and duplicate category → 400.
	if status, out := doJSON(t, "PUT", base+"/a/state-files", `{"categories":[]}`); status != http.StatusBadRequest {
		t.Fatalf("empty files = %d, want 400 (body %v)", status, out)
	}
	if status, out := doJSON(t, "PUT", base+"/a/state-files", `{"categories":["A","A"]}`); status != http.StatusBadRequest {
		t.Fatalf("duplicate files = %d, want 400 (body %v)", status, out)
	}
	// Prompt-template reference update.
	tpl, _ := json.Marshal(map[string]any{"template": "impl-tpl"})
	status, out = doJSON(t, "PUT", base+"/a/prompt-template", string(tpl))
	if status != http.StatusOK || out["prompt_template"] != "IMPL-TPL" {
		t.Fatalf("template = %d %v, want 200 + IMPL-TPL", status, out)
	}
}

// T6.36.5 — Historically referenced state protected: a state referenced by
// a current-version transition is rejected with 409 and the row survives;
// an unreferenced state deletes with 204 and its row is gone.
func TestP626DeleteStateProtection(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/states"
	// State A is referenced (initial state + the A→B transition): 409.
	status, out := doJSON(t, "DELETE", base+"/a", "")
	if status != http.StatusConflict {
		t.Fatalf("delete referenced state = %d, want 409 (body %v)", status, out)
	}
	// The row survived.
	status, cur := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine", "")
	if status != 200 {
		t.Fatalf("GET machine = %d", status)
	}
	for _, s := range cur["states"].([]any) {
		if s.(map[string]any)["key"] == "A" {
			return
		}
	}
	t.Fatal("state A row must survive the protected delete")
}

// T6.36.5 — an unreferenced state deletes cleanly (204; the machine read
// no longer lists it).
func TestP626DeleteUnreferenced(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/states"
	// Create a leaf state (no transition references it).
	created, _ := json.Marshal(map[string]any{"key": "leaf", "name": "Leaf", "kind": "CONTROL"})
	if status, out := doJSON(t, "POST", base, string(created)); status != http.StatusCreated {
		t.Fatalf("create leaf = %d (body %v)", status, out)
	}
	if status, out := doJSON(t, "DELETE", base+"/leaf", ""); status != http.StatusNoContent {
		t.Fatalf("delete leaf = %d, want 204 (body %v)", status, out)
	}
	status, cur := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine", "")
	if status != 200 {
		t.Fatalf("GET machine = %d", status)
	}
	for _, s := range cur["states"].([]any) {
		if s.(map[string]any)["key"] == "LEAF" {
			t.Fatal("deleted state must not be listed")
		}
	}
	// Deleting it again → 404 (the row is gone).
	if status, out := doJSON(t, "DELETE", base+"/leaf", ""); status != http.StatusNotFound {
		t.Fatalf("delete again = %d, want 404 (body %v)", status, out)
	}
}
