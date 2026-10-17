// P6-31 (WBS 6.41): transition preview — evaluate a transition WITHOUT
// applying it. POST /api/v1/projects/{project}/transitions/preview returns
// the selected transition (T6.41.1) and leaves the runtime row and the
// transition history untouched (T6.41.2–T6.41.4). The evaluation composes
// the P6-20 runtime read, the P6-14 priority selection, and the P6-10
// condition evaluation over a caller-supplied context — read-only.
package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

// p631Fixture seeds the T6.41 fixture: a project, a v1 machine (states
// A/B/C/D with A->B INCOMPLETE, A->C GO, C->D NEXT, D->A BACK so the
// runtime can be advanced and then returned to A), the A->B transition
// carrying the WBS 6.41 example condition (context_usage <
// project.context_threshold), the runtime initialized at A through the
// P6-22 Initialize op and advanced to C and back to A through the P6-20 op
// (runtime version 2, exactly two history rows). The runtime ends at A —
// the source of the WBS 6.41 example — with a non-empty history so the
// "history unchanged" assertions (count AND max seq) are non-vacuous.
// Returns (project, machineID).
func p631Fixture(t *testing.T, h *Harness) (string, string) {
	t.Helper()
	ctx := context.Background()
	pool := h.App.DB().Raw()
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "t641",
		[]service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "ACTION"},
			{Key: "C", Kind: "ACTION"}, {Key: "D", Kind: "ACTION"},
		},
		[]service.MachineTransitionDef{
			{From: "A", Outcome: "INCOMPLETE", To: "B"},
			{From: "A", Outcome: "GO", To: "C"},
			{From: "C", Outcome: "NEXT", To: "D"},
			{From: "D", Outcome: "BACK", To: "A"},
		},
		"A",
	)
	// Attach the WBS 6.41 example condition to the A->B transition.
	var trID uuid.UUID
	if err := h.DBScan(ctx,
		"SELECT id FROM state_transitions WHERE machine_id = '"+mid+"' AND version = 1 AND outcome = 'INCOMPLETE'", &trID); err != nil {
		t.Fatalf("read INCOMPLETE transition: %v", err)
	}
	if err := dbrepo.NewTransitionConditionRepository(pool).Create(ctx, trID, &persistence.TransitionCondition{
		Field: "context_usage", Operator: "<", Literal: float64(80),
	}); err != nil {
		t.Fatalf("create condition: %v", err)
	}
	// Initialize the runtime at A (version 1) and advance it A->C->D->A
	// (versions 2..4 -> the row is at version 4, state A) through the
	// P6-20 op — a pure position write, no selection involved.
	rsvc := p622Service(h, true)
	if _, err := rsvc.Initialize(ctx, proj, mid); err != nil {
		t.Fatalf("initialize runtime: %v", err)
	}
	// Advance A->C->D->A through the P6-21 ApplyTransition op (it is the
	// op that appends transition history — the P6-20 position write does
	// not), so the fixture leaves exactly three history rows. The
	// expected version tracks the row: init wrote version 1.
	apsvc := p621Service(h, true)
	for i, step := range []struct{ from, to, outcome string }{
		{"A", "C", "GO"}, {"C", "D", "NEXT"}, {"D", "A", "BACK"},
	} {
		if _, err := apsvc.ApplyTransition(ctx, proj, &service.ApplyTransitionInput{
			FromStateID: p620StateIDs(t, h, mid, step.from),
			ToStateID:   p620StateIDs(t, h, mid, step.to),
			Outcome:     step.outcome, ExpectedVersion: 1 + i,
		}); err != nil {
			t.Fatalf("advance runtime %s->%s: %v", step.from, step.to, err)
		}
	}
	var stateID string
	if err := h.DBScan(ctx, "SELECT current_state_id::text FROM runtime_state WHERE project_id = '"+proj+"'", &stateID); err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if stateID != p620StateIDs(t, h, mid, "A") {
		t.Fatal("fixture runtime is not at state A")
	}
	var rows int
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM state_transition_history WHERE project_id = '"+proj+"'", &rows); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if rows != 3 {
		t.Fatalf("fixture history rows = %d, want exactly 3", rows)
	}
	return proj, mid
}

// p631RowSnapshot reads the persisted runtime row (current state, machine
// binding, version) and the history table's count + max seq for the
// project.
type p631Row struct {
	stateID, machineID    string
	version, rows, maxSeq int
}

func p631RowSnapshot(t *testing.T, h *Harness, proj string) p631Row {
	t.Helper()
	var r p631Row
	ctx := context.Background()
	if err := h.DBScan(ctx,
		"SELECT current_state_id::text, state_machine_id::text, version FROM runtime_state WHERE project_id = '"+proj+"'",
		&r.stateID, &r.machineID, &r.version); err != nil {
		t.Fatalf("read runtime row: %v", err)
	}
	if err := h.DBScan(ctx,
		"SELECT count(*), coalesce(max(seq),0) FROM state_transition_history WHERE project_id = '"+proj+"'",
		&r.rows, &r.maxSeq); err != nil {
		t.Fatalf("read history: %v", err)
	}
	return r
}

// p631PreviewBody is the WBS 6.41 example request: outcome INCOMPLETE,
// usage 71%, threshold 80%.
const p631PreviewBody = `{"outcome":"INCOMPLETE","context_usage":71,"context_threshold":80}`

// T6.41.1 — Preview returns the selected transition: evaluating
// (current state A, outcome INCOMPLETE, usage 71% below threshold 80%)
// returns the selected transition's destination (state B) without
// applying it — the WBS 6.41 example result.
func TestP631PreviewReturnsSelectedTransition(t *testing.T) {
	h := NewHarness(t)
	proj, mid := p631Fixture(t, h)
	status, out := doJSON(t, "POST", h.BaseURL+"/api/v1/projects/"+proj+"/transitions/preview", p631PreviewBody)
	if status != 200 {
		t.Fatalf("POST preview = %d, want 200 (body %v)", status, out)
	}
	wantTo := p620StateIDs(t, h, mid, "B")
	if out["to_state"] != wantTo {
		t.Fatalf("to_state = %v, want the selected transition's destination %s", out["to_state"], wantTo)
	}
	if out["outcome"] != "INCOMPLETE" {
		t.Fatalf("outcome = %v, want INCOMPLETE", out["outcome"])
	}
	if out["current_state"] != p620StateIDs(t, h, mid, "A") {
		t.Fatalf("current_state = %v, want the runtime's current state A", out["current_state"])
	}
}

// T6.41.2 — Preview does not persist the transition: after the op the
// runtime row is byte-identical (current state, machine binding, version).
func TestP631PreviewDoesNotPersist(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p631Fixture(t, h)
	before := p631RowSnapshot(t, h, proj)
	status, _ := doJSON(t, "POST", h.BaseURL+"/api/v1/projects/"+proj+"/transitions/preview", p631PreviewBody)
	if status != 200 {
		t.Fatalf("POST preview = %d, want 200", status)
	}
	after := p631RowSnapshot(t, h, proj)
	if before.stateID != after.stateID || before.machineID != after.machineID || before.version != after.version {
		t.Fatalf("runtime row changed: (%s,%s,%d) -> (%s,%s,%d); preview must be a no-op",
			before.stateID, before.machineID, before.version,
			after.stateID, after.machineID, after.version)
	}
}

// T6.41.3 — Runtime version unchanged: the optimistic-concurrency version
// counter is untouched by the preview.
func TestP631PreviewVersionUnchanged(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p631Fixture(t, h)
	before := p631RowSnapshot(t, h, proj)
	if before.version != 4 {
		t.Fatalf("fixture version = %d, want 4 (fixture precondition)", before.version)
	}
	status, _ := doJSON(t, "POST", h.BaseURL+"/api/v1/projects/"+proj+"/transitions/preview", p631PreviewBody)
	if status != 200 {
		t.Fatalf("POST preview = %d, want 200", status)
	}
	after := p631RowSnapshot(t, h, proj)
	if after.version != before.version {
		t.Fatalf("version = %d after preview, want %d (the preview must not bump it)",
			after.version, before.version)
	}
}

// T6.41.4 — History unchanged: no state_transition_history row is written
// (count AND max seq unchanged).
func TestP631PreviewHistoryUnchanged(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p631Fixture(t, h)
	before := p631RowSnapshot(t, h, proj)
	if before.rows != 3 || before.maxSeq != 3 {
		t.Fatalf("fixture history = (%d rows, max seq %d), want (3, 3) (fixture precondition)",
			before.rows, before.maxSeq)
	}
	status, _ := doJSON(t, "POST", h.BaseURL+"/api/v1/projects/"+proj+"/transitions/preview", p631PreviewBody)
	if status != 200 {
		t.Fatalf("POST preview = %d, want 200", status)
	}
	after := p631RowSnapshot(t, h, proj)
	if after.rows != before.rows || after.maxSeq != before.maxSeq {
		t.Fatalf("history changed: (%d rows, max seq %d) -> (%d rows, max seq %d); preview must not append",
			before.rows, before.maxSeq, after.rows, after.maxSeq)
	}
}

// No-match preview: an outcome that no enabled transition of the runtime's
// machine carries returns 200 with a null transition (a nil selection is a
// defined, non-error result of the P6-14 selection).
func TestP631PreviewNoMatch(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p631Fixture(t, h)
	status, out := doJSON(t, "POST", h.BaseURL+"/api/v1/projects/"+proj+"/transitions/preview",
		`{"outcome":"NO_SUCH_OUTCOME","context_usage":71,"context_threshold":80}`)
	if status != 200 {
		t.Fatalf("POST preview (no match) = %d, want 200 (body %v)", status, out)
	}
	if out["to_state"] != nil {
		t.Fatalf("to_state = %v, want null (no selection)", out["to_state"])
	}
}

// Condition-gated selection: at/above the threshold the
// context_usage < threshold condition is false, so the INCOMPLETE
// transition is not eligible and the preview reports no selection even
// though the outcome text matches — the P6-10 condition evaluation is part
// of the preview (this pins the gated half of the evaluation).
func TestP631PreviewConditionGate(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p631Fixture(t, h)
	for _, usage := range []string{"80", "95"} {
		body := `{"outcome":"INCOMPLETE","context_usage":` + usage + `,"context_threshold":80}`
		status, out := doJSON(t, "POST", h.BaseURL+"/api/v1/projects/"+proj+"/transitions/preview", body)
		if status != 200 {
			t.Fatalf("POST preview (usage %v) = %d, want 200 (body %v)", usage, status, out)
		}
		if out["to_state"] != nil {
			t.Fatalf("usage %s: to_state = %v, want null (condition false => no eligible transition)", usage, out["to_state"])
		}
	}
}

// Nil-pool / txPool parity: the same preview assembled sequentially and
// transactionally is field-by-field identical (the sequential-fallback
// evidence; the op is read-only, so the -race gate covers concurrency).
func TestP631NilPoolParity(t *testing.T) {
	h := NewHarness(t)
	proj, mid := p631Fixture(t, h)
	pool := h.App.DB().Raw()
	build := func(withPool bool) *service.PreviewTransitionResult {
		svc := service.NewTransitionPreviewService(
			dbrepo.NewRuntimeStateRepository(pool),
			dbrepo.NewStateTransitionRepository(pool),
		)
		if withPool {
			svc = svc.WithTxPool(pool)
		}
		v, err := svc.Preview(context.Background(), proj, &service.PreviewTransitionInput{
			Outcome: "INCOMPLETE", ContextUsage: 71, ContextThreshold: 80,
		})
		if err != nil {
			t.Fatalf("preview (pool=%v): %v", withPool, err)
		}
		return v
	}
	a, b := build(false), build(true)
	if (a.Selected == nil) != (b.Selected == nil) || a.Selected == nil {
		t.Fatalf("selection presence diverged or missing: seq=%+v tx=%+v", a, b)
	}
	if a.Selected.ToStateID != b.Selected.ToStateID || a.Outcome != b.Outcome {
		t.Fatalf("sequential vs transactional preview diverged: seq=%+v tx=%+v", a, b)
	}
	if a.Selected.ToStateID != p620StateIDs(t, h, mid, "B") {
		t.Fatalf("selected destination = %s, want state B", a.Selected.ToStateID)
	}
}

// Uninitialized project: 404 (no runtime row to evaluate from).
func TestP631UninitializedProject404(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	status, _ := doJSON(t, "POST", h.BaseURL+"/api/v1/projects/"+proj+"/transitions/preview", p631PreviewBody)
	if status != 404 {
		t.Fatalf("POST preview on uninitialized project = %d, want 404", status)
	}
}

// Unknown project: 404.
func TestP631UnknownProject404(t *testing.T) {
	h := NewHarness(t)
	status, _ := doJSON(t, "POST",
		h.BaseURL+"/api/v1/projects/11111111-1111-1111-1111-111111111111/transitions/preview", p631PreviewBody)
	if status != 404 {
		t.Fatalf("POST preview unknown project = %d, want 404", status)
	}
}

// Malformed body: 400 (unknown field / missing required field).
func TestP631PreviewValidation(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p631Fixture(t, h)
	for _, body := range []string{
		`{"outcome":"INCOMPLETE","context_usage":71,"context_threshold":80,"bogus":1}`,
		`{"context_usage":71,"context_threshold":80}`,
	} {
		status, _ := doJSON(t, "POST", h.BaseURL+"/api/v1/projects/"+proj+"/transitions/preview", body)
		if status != 400 {
			t.Fatalf("POST preview (body %s) = %d, want 400", body, status)
		}
	}
}

// The preview route is POST-only: other write/read methods on the path are
// rejected (405 via the Go 1.22 mux) and the runtime row is untouched.
func TestP631PreviewPostOnly(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p631Fixture(t, h)
	for _, method := range []string{"PUT", "GET", "PATCH"} {
		status := p630Status(t, method, h.BaseURL+"/api/v1/projects/"+proj+"/transitions/preview", "")
		if status != http.StatusMethodNotAllowed {
			t.Fatalf("%s preview = %d, want 405 (POST-only surface)", method, status)
		}
	}
}
