// P6-30 (WBS 6.40): runtime-state REST. GET
// /api/v1/projects/{project}/runtime-state returns the current state, its
// phase (order + name), the current work unit (id + name), and the runtime
// version (T6.40.1-T6.40.5) — read-only; no endpoint accepts an arbitrary
// runtime state. The endpoint drives the real HTTP server through the
// harness; the fixture is initialized through the P6-22 service (the only
// writer) and advanced through the P6-20 Transition op so the read serves
// a non-initial state.
package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// p630Fixture seeds the T6.40 fixture: a project, a v1 machine (states A/B,
// transition A-GO->B), the runtime initialized to A through the P6-22
// Initialize op (the only writer), phase 1 "Auth" (RUNNING), an active plan
// on that phase, and the phase/work-unit association written through the
// P6-20 Transition op (version 2, state B).
// Returns (project, machineID, unitID, unitName).
func p630Fixture(t *testing.T, h *Harness) (string, string, string, string) {
	t.Helper()
	ctx := context.Background()
	pool := h.App.DB().Raw()
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "t640",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	svc := p622Service(h, true)
	if _, err := svc.Initialize(ctx, proj, mid); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	phaseID := seedPhaseStatus(t, h, uuid.MustParse(proj), 1, "Auth", "EXECUTING")
	planID := seedPlan(t, h, phaseID, 1, 1, "EXECUTING")
	unitName := "compose-registration"
	unitID := uuid.New()
	if _, err := h.DBExec(ctx, "INSERT INTO work_units (id, execution_plan_id, project_id, sequence, name, description, kind, status, objective, implementation_instructions, acceptance_criteria, required_tests, constraints, created_at) VALUES ('"+
		unitID.String()+"', '"+planID.String()+"', '"+proj+"', 1, '"+unitName+"', '', 'ATOMIC', 'EXECUTING', '', '', '[]', '[]', '[]', now())"); err != nil {
		t.Fatalf("seed work unit: %v", err)
	}
	// Advance the runtime to state B with the phase + unit association
	// through the P6-20 Transition op (expected version 1 -> 2).
	phase, unit := phaseID.String(), unitID.String()
	rsvc := service.NewRuntimeStateService(dbrepo.NewRuntimeStateRepository(pool))
	st, err := rsvc.Transition(ctx, proj, &service.TransitionInput{
		StateMachineID: mid, StateMachineVersion: 1,
		CurrentStateID: p620StateIDs(t, h, mid, "B"),
		PhaseID:        &phase, WorkUnitID: &unit, ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if st.Version != 2 {
		t.Fatalf("transition reported version %d, want 2", st.Version)
	}
	return proj, mid, unitID.String(), unitName
}

// T6.40.1 — Current state returned: GET returns the runtime's current
// state (B, after the fixture's transition).
// T6.40.2 — Phase returned: the runtime's associated phase order + name.
// T6.40.3 — Work unit returned: the runtime's associated work unit id + name.
// T6.40.4 — Runtime version returned: the row's optimistic-concurrency
// version counter (2 after the fixture's transition).
func TestP630RuntimeStateRead(t *testing.T) {
	h := NewHarness(t)
	proj, mid, unitID, unitName := p630Fixture(t, h)
	status, out := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/runtime-state", "")
	if status != 200 {
		t.Fatalf("GET runtime-state = %d, want 200 (body %v)", status, out)
	}
	wantState := p620StateIDs(t, h, mid, "B")
	if out["state"] != wantState {
		t.Fatalf("state = %v, want the current state B %s", out["state"], wantState)
	}
	ph, ok := out["phase"].(map[string]any)
	if !ok {
		t.Fatalf("phase section = %v, want an object", out["phase"])
	}
	if ph["order"] != float64(1) || ph["name"] != "Auth" {
		t.Fatalf("phase = %v, want order 1 name Auth", ph)
	}
	wu, ok := out["work_unit"].(map[string]any)
	if !ok {
		t.Fatalf("work_unit section = %v, want an object", out["work_unit"])
	}
	if wu["id"] != unitID || wu["name"] != unitName {
		t.Fatalf("work_unit = %v, want id %s name %s", wu, unitID, unitName)
	}
	if out["runtime_version"] != float64(2) {
		t.Fatalf("runtime_version = %v, want 2", out["runtime_version"])
	}
}

// p630Status performs the request and returns only the status code: the
// Go 1.22 mux's 405 body is plain text, which doJSON's JSON decode rejects.
func p630Status(t *testing.T, method, url, body string) int {
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
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// T6.40.5 — Direct arbitrary state mutation unavailable: no REST surface
// accepts an arbitrary runtime state. The route is registered GET-only:
// PUT/POST/PATCH bodies carrying a state are rejected (405 via the Go 1.22
// mux), and the stored row is untouched afterwards.
func TestP630NoArbitraryMutationEndpoint(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _ := p630Fixture(t, h)
	for _, method := range []string{"PUT", "POST", "PATCH"} {
		status := p630Status(t, method, h.BaseURL+"/api/v1/projects/"+proj+"/runtime-state",
			`{"state":"X","phase":{"order":1,"name":"Auth"},"work_unit":{"id":"11111111-1111-1111-1111-111111111111","name":"n"},"runtime_version":2}`)
		if status != 405 {
			t.Fatalf("%s runtime-state = %d, want 405 (read-only surface)", method, status)
		}
	}
	// The stored row is untouched: version still 2.
	var version int
	if err := h.DBScan(context.Background(),
		"SELECT version FROM runtime_state WHERE project_id = '"+proj+"'", &version); err != nil {
		t.Fatalf("read row version: %v", err)
	}
	if version != 2 {
		t.Fatalf("row version = %d, want 2 (mutations must be impossible)", version)
	}
}

// No initialized runtime: a project with no Initialize call reports 404
// (the P6-22 Initialize op is the only writer).
func TestP630UninitializedProject404(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	status, _ := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/runtime-state", "")
	if status != 404 {
		t.Fatalf("GET on uninitialized project = %d, want 404", status)
	}
}

// Unknown project: 404.
func TestP630UnknownProject404(t *testing.T) {
	h := NewHarness(t)
	status, _ := doJSON(t, "GET",
		h.BaseURL+"/api/v1/projects/11111111-1111-1111-1111-111111111111/runtime-state", "")
	if status != 404 {
		t.Fatalf("GET unknown project = %d, want 404", status)
	}
}

// Nil-pool / txPool parity: the same read assembled sequentially and
// transactionally is field-by-field identical (the sequential-fallback
// evidence).
func TestP630NilPoolParity(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _ := p630Fixture(t, h)
	pool := h.App.DB().Raw()
	build := func(withPool bool) *service.RuntimeStateView {
		svc := service.NewRuntimeStateAPIService(
			dbrepo.NewRuntimeStateRepository(pool),
			dbrepo.NewPhaseRepository(pool),
			dbrepo.NewPlanRepository(pool),
			dbrepo.NewWorkUnitRepository(pool),
		)
		if withPool {
			svc = svc.WithTxPool(pool)
		}
		v, err := svc.Get(context.Background(), proj)
		if err != nil {
			t.Fatalf("get (pool=%v): %v", withPool, err)
		}
		return v
	}
	a, b := build(false), build(true)
	if a.State != b.State || a.Phase == nil || b.Phase == nil ||
		a.Phase.Order != b.Phase.Order || a.Phase.Name != b.Phase.Name ||
		(a.WorkUnit == nil) != (b.WorkUnit == nil) || a.RuntimeVersion != b.RuntimeVersion {
		t.Fatalf("sequential vs transactional read diverged:\n seq=%+v\n tx=%+v", a, b)
	}
	if a.WorkUnit != nil && b.WorkUnit != nil &&
		(a.WorkUnit.ID != b.WorkUnit.ID || a.WorkUnit.Name != b.WorkUnit.Name) {
		t.Fatalf("work unit sections diverged: %+v vs %+v", a.WorkUnit, b.WorkUnit)
	}
}

// Cross-project isolation: a phase from ANOTHER project is never served
// for this project (the association is resolved against the runtime row's
// stored phase id, which is project-scoped by construction of the row).
func TestP630CrossProjectIsolation(t *testing.T) {
	h := NewHarness(t)
	proj, _, _, _ := p630Fixture(t, h)
	other := p605Project(t, h)
	otherPhase := seedPhaseStatus(t, h, uuid.MustParse(other), 1, "Foreign", "EXECUTING")
	// Point the fixture project's runtime at the FOREIGN phase id.
	if _, err := h.DBExec(context.Background(),
		"UPDATE runtime_state SET phase_id = '"+otherPhase.String()+"' WHERE project_id = '"+proj+"'"); err != nil {
		t.Fatalf("repoint phase: %v", err)
	}
	status, out := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/runtime-state", "")
	if status != 200 {
		t.Fatalf("GET = %d, want 200", status)
	}
	if ph, _ := out["phase"].(map[string]any); ph != nil {
		t.Fatalf("phase = %v, want null (cross-project association not served)", ph)
	}
}

// The service surfaces persistence.ErrNotFound as service.ErrNotFound —
// the sentinel the handler maps to 404 (pin it through the service, not a
// hand-rolled error).
func TestP630ServiceNotFoundSentinel(t *testing.T) {
	h := NewHarness(t)
	pool := h.App.DB().Raw()
	svc := service.NewRuntimeStateAPIService(
		dbrepo.NewRuntimeStateRepository(pool),
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewPlanRepository(pool),
		dbrepo.NewWorkUnitRepository(pool),
	)
	proj := p605Project(t, h)
	_, err := svc.Get(context.Background(), proj)
	if !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("uninitialized Get err = %v, want service.ErrNotFound", err)
	}
}
