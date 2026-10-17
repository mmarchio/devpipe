package integration

import (
	"encoding/json"
	"testing"

	"overseer/internal/service"
)

// P6-25 (WBS 6.35): state-machine CRUD REST. GET current machine (the
// family's MAX-version), PUT publishes a NEW version (P6-17 Edit path),
// GET versions + version detail, invalid definition rejected (400), stale
// edit conflict (409). The endpoints drive the real HTTP server through
// the harness; the machine is seeded through the P6-17 repository helper
// since machine creation is a service-only op.

func p625DefaultStates() []map[string]any {
	return []map[string]any{
		{"key": "A", "kind": "CONTROL"},
		{"key": "B", "kind": "TERMINAL"},
	}
}

func p625DefaultTransitions() []map[string]any {
	return []map[string]any{
		{"from": "A", "outcome": "GO", "to": "B"},
	}
}

func putMachine(t *testing.T, h *Harness, projectID, body string) (int, map[string]any) {
	t.Helper()
	return doJSON(t, "PUT", h.BaseURL+"/api/v1/projects/"+projectID+"/state-machine", body)
}

// T6.35.1 — Current machine retrieved: GET returns the family's
// MAX-version machine with its definition and version.
func TestP625GetCurrentMachine(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	p617SeedMachine(t, h, proj, "t6351",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	status, out := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine", "")
	if status != 200 {
		t.Fatalf("GET state-machine = %d, want 200 (body %v)", status, out)
	}
	if out["version"] != float64(1) {
		t.Fatalf("current machine version = %v, want 1", out["version"])
	}
	if out["initial_state"] != "A" {
		t.Fatalf("current machine initial_state = %v, want A", out["initial_state"])
	}
	states, ok := out["states"].([]any)
	if !ok || len(states) != 2 {
		t.Fatalf("current machine states = %v, want the 2 seeded states", out["states"])
	}
	trans, ok := out["transitions"].([]any)
	if !ok || len(trans) != 1 {
		t.Fatalf("current machine transitions = %v, want the 1 seeded transition", out["transitions"])
	}
}

// T6.35.2 — Update creates new version: PUT appends a NEW machine version
// (P6-17 Edit path) and the response reports the new version; the current
// machine read then reflects the new definition.
func TestP625UpdateCreatesNewVersion(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	p617SeedMachine(t, h, proj, "t6352",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	body, _ := json.Marshal(map[string]any{
		"initial_state": "A",
		"states": []map[string]any{
			{"key": "A", "kind": "CONTROL"},
			{"key": "C", "kind": "ACTION"},
			{"key": "B", "kind": "TERMINAL"},
		},
		"transitions": []map[string]any{
			{"from": "A", "outcome": "GO", "to": "C"},
			{"from": "C", "outcome": "DONE", "to": "B"},
		},
	})
	status, out := putMachine(t, h, proj, string(body))
	if status != 200 {
		t.Fatalf("PUT state-machine = %d, want 200 (body %v)", status, out)
	}
	if out["version"] != float64(2) {
		t.Fatalf("PUT reported version = %v, want 2", out["version"])
	}
	// The current machine is now version 2 with the new state set.
	status, cur := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine", "")
	if status != 200 {
		t.Fatalf("GET after PUT = %d, want 200 (body %v)", status, cur)
	}
	if cur["version"] != float64(2) {
		t.Fatalf("current version after PUT = %v, want 2", cur["version"])
	}
	keys := map[string]bool{}
	for _, s := range cur["states"].([]any) {
		keys[s.(map[string]any)["key"].(string)] = true
	}
	if len(keys) != 3 || !keys["C"] {
		t.Fatalf("current states after PUT = %v, want the 3-state definition incl C", cur["states"])
	}
}

// T6.35.3 — Historical version retrievable: GET versions lists both
// versions, and GET versions/1 returns the ORIGINAL v1 definition after
// v2 exists.
func TestP625HistoricalVersionRetrievable(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	p617SeedMachine(t, h, proj, "t6353",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	raw, _ := json.Marshal(map[string]any{
		"initial_state": "A",
		"states":        []map[string]any{{"key": "A", "kind": "CONTROL"}, {"key": "B", "kind": "TERMINAL"}},
		"transitions":   []map[string]any{{"from": "A", "outcome": "GO", "to": "B"}},
	})
	status, out := putMachine(t, h, proj, string(raw))
	if status != 200 {
		t.Fatalf("PUT = %d, want 200 (body %v)", status, out)
	}
	// Versions list.
	status, list := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine/versions", "")
	if status != 200 {
		t.Fatalf("GET versions = %d, want 200 (body %v)", status, list)
	}
	vers, ok := list["versions"].([]any)
	if !ok || len(vers) != 2 {
		t.Fatalf("versions list = %v, want 2 entries", list["versions"])
	}
	// Version detail: v1 keeps the ORIGINAL 2-state definition.
	status, v1 := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine/versions/1", "")
	if status != 200 {
		t.Fatalf("GET versions/1 = %d, want 200 (body %v)", status, v1)
	}
	if v1["version"] != float64(1) {
		t.Fatalf("v1 detail version = %v, want 1", v1["version"])
	}
	if len(v1["states"].([]any)) != 2 {
		t.Fatalf("v1 detail states = %v, want the original 2 states", v1["states"])
	}
	status, v2 := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine/versions/2", "")
	if status != 200 {
		t.Fatalf("GET versions/2 = %d, want 200 (body %v)", status, v2)
	}
	if v2["version"] != float64(2) {
		t.Fatalf("v2 detail version = %v, want 2", v2["version"])
	}
	// Unknown version → 404.
	status, _ = doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine/versions/99", "")
	if status != 404 {
		t.Fatalf("GET versions/99 = %d, want 404", status)
	}
}

// T6.35.4 — Invalid machine rejected: PUT payloads with an unknown
// initial key, duplicate state keys, or a transition to an unknown state
// return 400 and change nothing.
func TestP625InvalidMachineRejected(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	p617SeedMachine(t, h, proj, "t6354",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	cases := map[string]map[string]any{
		"unknown initial": {"initial_state": "X", "states": p625DefaultStates(), "transitions": p625DefaultTransitions()},
		"duplicate key":   {"initial_state": "A", "states": []map[string]any{{"key": "A", "kind": "CONTROL"}, {"key": "A", "kind": "ACTION"}, {"key": "B", "kind": "TERMINAL"}}, "transitions": nil},
		"unknown ref":     {"initial_state": "A", "states": p625DefaultStates(), "transitions": []map[string]any{{"from": "A", "outcome": "GO", "to": "NOPE"}}},
	}
	for name, payload := range cases {
		raw, _ := json.Marshal(payload)
		status, body := putMachine(t, h, proj, string(raw))
		if status != 400 {
			t.Fatalf("%s: PUT = %d, want 400 (body %v)", name, status, body)
		}
	}
	// Nothing changed: the machine is still v1 with the original shape.
	status, cur := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine", "")
	if status != 200 || cur["version"] != float64(1) || len(cur["states"].([]any)) != 2 {
		t.Fatalf("machine after invalid PUTs = %v (status %d), want unchanged v1", cur, status)
	}
	// Unknown field rejected by strict decode.
	status, out := doJSON(t, "PUT", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine",
		`{"initial_state":"A","states":[],"transitions":[],"bogus":1}`)
	if status != 400 {
		t.Fatalf("unknown-field PUT = %d, want 400 (body %v)", status, out)
	}
	// Empty state set rejected.
	status, out = doJSON(t, "PUT", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine",
		`{"initial_state":"A","states":[],"transitions":[]}`)
	if status != 400 {
		t.Fatalf("empty-states PUT = %d, want 400 (body %v)", status, out)
	}
}

// T6.35.5 — Stale edit returns conflict: a PUT carrying an outdated
// expected_version returns 409 and creates no version; the correct
// expected_version succeeds.
func TestP625StaleEditReturnsConflict(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	p617SeedMachine(t, h, proj, "t6355",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	// The machine is at version 1: expected_version 2 is stale.
	status, out := doJSON(t, "PUT", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine",
		`{"initial_state":"A","states":[{"key":"A","kind":"CONTROL"},{"key":"B","kind":"TERMINAL"}],"transitions":[{"from":"A","outcome":"GO","to":"B"}],"expected_version":2}`)
	if status != 409 {
		t.Fatalf("stale PUT = %d, want 409 (body %v)", status, out)
	}
	// The correct expected_version succeeds (version → 2).
	status, out = doJSON(t, "PUT", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine",
		`{"initial_state":"A","states":[{"key":"A","kind":"CONTROL"},{"key":"B","kind":"TERMINAL"}],"transitions":[{"from":"A","outcome":"GO","to":"B"}],"expected_version":1}`)
	if status != 200 || out["version"] != float64(2) {
		t.Fatalf("current-version PUT = %d (%v), want 200 with version 2", status, out)
	}
	// Now 1 is stale again.
	status, out = doJSON(t, "PUT", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine",
		`{"initial_state":"A","states":[{"key":"A","kind":"CONTROL"},{"key":"B","kind":"TERMINAL"}],"transitions":[{"from":"A","outcome":"GO","to":"B"}],"expected_version":1}`)
	if status != 409 {
		t.Fatalf("second stale PUT = %d, want 409 (body %v)", status, out)
	}
}
