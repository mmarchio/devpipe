// P6-28 (WBS 6.38): state-machine validation REST.
// GET /api/v1/projects/{project}/state-machine/validation returns
// {valid, errors[]} — one structured issue per finding, all errors at
// once, deterministic (two consecutive calls byte-identical), and
// non-mutating (the persisted definition is byte-identical before/after).
// The endpoint validates the CURRENT machine (the family's MAX-version,
// the P6-25 resolution); an unknown project reports 404.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/state"
)

func p628ValidationURL(h *Harness, projectID string) string {
	return h.BaseURL + "/api/v1/projects/" + projectID + "/state-machine/validation"
}

func p628GetValidation(t *testing.T, h *Harness, projectID string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, p628ValidationURL(h, projectID), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET validation: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("read validation body: %v", err)
	}
	return resp.StatusCode, buf.Bytes()
}

// T6.38.1 — Valid machine reports valid: GET /validation on a valid
// machine returns 200 with valid=true and NO errors (an empty array,
// never null), and the body is byte-identical across two consecutive
// calls (the determinism pin).
func TestP628ValidMachineReportsValid(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	p616ValidMachine(t, h, proj, "p628-valid", "P628-VALID-ROLE")
	status, body := p628GetValidation(t, h, proj)
	if status != 200 {
		t.Fatalf("GET validation = %d, want 200 (body %s)", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode validation body: %v", err)
	}
	if out["valid"] != true {
		t.Fatalf("valid = %v, want true (body %s)", out["valid"], body)
	}
	errs, ok := out["errors"].([]any)
	if !ok {
		t.Fatalf("errors is not a JSON array: %v", out["errors"])
	}
	if len(errs) != 0 {
		t.Fatalf("valid machine reported %d errors: %s", len(errs), body)
	}
	// Determinism: the second call's body is byte-identical to the first.
	_, body2 := p628GetValidation(t, h, proj)
	if !bytes.Equal(body, body2) {
		t.Fatalf("validation body not deterministic: %s vs %s", body, body2)
	}
}

// T6.38.2 — Invalid machine reports errors: a broken machine returns
// 200 with valid=false and structured issue codes (one per finding);
// an unknown project reports 404.
func TestP628InvalidMachineReportsErrors(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	ctx := context.Background()
	mid, _, st := p615DevMachineFixture(t, h, proj, "p628-invalid")
	// Break it in THREE ways that the P6-16 service codes deterministically,
	// with the initial state intact so the violations stay isolated:
	// a state missing its required template (template_missing), a disabled
	// role assignment (role_disabled + role_allowed_state — the disabled
	// role allows no state), and an orphan state (reachability + dead_end).
	if _, err := h.DBExec(ctx, "UPDATE workflow_states SET prompt_template = '' WHERE machine_id = '"+mid.String()+"' AND key = 'IMPLEMENT'"); err != nil {
		t.Fatalf("clear template: %v", err)
	}
	disabled := p607Role(t, h, proj, "P628-INVALID-ROLE")
	if _, err := h.DBExec(ctx, "UPDATE roles SET enabled = false WHERE id = '"+disabled+"'"); err != nil {
		t.Fatalf("disable role: %v", err)
	}
	if _, err := h.DBExec(ctx, "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+st["IMPLEMENT"].ID.String()+"', '"+disabled+"', 0, now(), now())"); err != nil {
		t.Fatalf("seed assignment: %v", err)
	}
	p615StateFixture(t, h, mid, "ORBX", state.KindControl, false, true)
	if _, err := h.DBExec(ctx, "UPDATE state_machines SET initial_state_id = '"+st["START"].ID.String()+"' WHERE id = '"+mid.String()+"'"); err != nil {
		t.Fatalf("set initial: %v", err)
	}
	status, body := p628GetValidation(t, h, proj)
	if status != 200 {
		t.Fatalf("GET validation = %d, want 200 (body %s)", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode validation body: %v", err)
	}
	if out["valid"] != false {
		t.Fatalf("broken machine reported valid=true (body %s)", body)
	}
	list, ok := out["errors"].([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("errors = %v, want a non-empty array of structured issues", out["errors"])
	}
	codes := map[string]int{}
	for _, e := range list {
		item, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("error entry is not an object: %v", e)
		}
		code, ok := item["code"].(string)
		if !ok || code == "" {
			t.Fatalf("error entry has no structured code: %v", item)
		}
		if _, ok := item["detail"].(string); !ok {
			t.Fatalf("error entry has no detail: %v", item)
		}
		codes[code]++
	}
	want := map[string]int{
		"template_missing":   2, // IMPLEMENT (cleared) + VALIDATE (the fixture sets none)
		"role_disabled":      1,
		"role_allowed_state": 1, // the disabled role allows nothing
		"reachability":       1, // ORBX (initial is intact, so nothing else is orphaned)
		"dead_end":           1, // ORBX has no outgoing transition
	}
	if len(list) != len(want)+1 {
		t.Fatalf("reported %d issues, want %d distinct codes (body %s)", len(list), len(want), body)
	}
	for code, n := range want {
		if codes[code] != n {
			t.Fatalf("codes = %v, want %v (body %s)", codes, want, body)
		}
	}
	// Unknown project → 404 (no machine family to validate).
	unknown := uuid.New().String()
	status, body = p628GetValidation(t, h, unknown)
	if status != 404 {
		t.Fatalf("GET validation on unknown project = %d, want 404 (body %s)", status, body)
	}
}

// T6.38.3 — Multiple errors returned: one machine carrying several
// independent violations yields EVERY issue in a single response, one
// structured issue per finding (mirrors T6.19.2 at the REST surface).
func TestP628MultipleErrorsReturned(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	ctx := context.Background()
	mid, _, st := p615DevMachineFixture(t, h, proj, "p628-multi")
	// Same multi-violation fixture as T6.19.2, driven at the REST surface.
	if _, err := h.DBExec(ctx, "UPDATE workflow_states SET prompt_template = '' WHERE machine_id = '"+mid.String()+"'"); err != nil {
		t.Fatalf("clear templates: %v", err)
	}
	disabled := p607Role(t, h, proj, "P628-DISABLED")
	if _, err := h.DBExec(ctx, "UPDATE roles SET enabled = false WHERE id = '"+disabled+"'"); err != nil {
		t.Fatalf("disable role: %v", err)
	}
	if _, err := h.DBExec(ctx, "INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at) VALUES (gen_random_uuid(), '"+st["START"].ID.String()+"', '"+disabled+"', 0, now(), now())"); err != nil {
		t.Fatalf("seed assignment: %v", err)
	}
	p615StateFixture(t, h, mid, "STUCKX", state.KindControl, false, true)
	p615StateFixture(t, h, mid, "ORBY", state.KindControl, false, true)
	if _, err := h.DBExec(ctx, "UPDATE state_machines SET initial_state_id = '"+st["START"].ID.String()+"' WHERE id = '"+mid.String()+"'"); err != nil {
		t.Fatalf("set initial: %v", err)
	}
	status, body := p628GetValidation(t, h, proj)
	if status != 200 {
		t.Fatalf("GET validation = %d, want 200 (body %s)", status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode validation body: %v", err)
	}
	if out["valid"] != false {
		t.Fatalf("machine with violations reported valid=true (body %s)", body)
	}
	list, ok := out["errors"].([]any)
	if !ok {
		t.Fatalf("errors is not an array: %v", out["errors"])
	}
	codes := map[string]int{}
	for _, e := range list {
		codes[e.(map[string]any)["code"].(string)]++
	}
	want := map[string]int{
		"template_missing":   2, // IMPLEMENT + VALIDATE
		"role_allowed_state": 1, // the disabled role allows nothing
		"role_disabled":      1,
		"dead_end":           2, // STUCKX + ORBY
		"reachability":       2, // STUCKX + ORBY
	}
	if len(list) != 8 {
		t.Fatalf("expected 8 issues at once, got %d (body %s)", len(list), body)
	}
	for code, n := range want {
		if codes[code] != n {
			t.Fatalf("code %q = %d, want %d (all codes: %v)", code, codes[code], n, codes)
		}
	}
}

// T6.38.4 — Validation endpoint doesn't mutate machine: the full
// persisted definition (machine row + every state + every transition)
// is byte-identical before and after the REST call.
func TestP628ValidationDoesNotMutate(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid := p616ValidMachine(t, h, proj, "p628-nomut", "P628-NOMUT-ROLE")
	before := p616MachineSnapshot(t, h, mid)
	status, _ := p628GetValidation(t, h, proj)
	if status != 200 {
		t.Fatalf("GET validation = %d, want 200", status)
	}
	p616AssertSnapshotEqual(t, h, mid, before)
}
