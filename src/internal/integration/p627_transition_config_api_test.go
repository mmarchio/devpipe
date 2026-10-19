// P6-27 (WBS 6.37): the transition-configuration REST surface over the
// real HTTP server. The machine is seeded through the p617 repository
// helper (machine creation is service-only, the established P6-25 pattern);
// the fixture reuses the p626 seed (project + machine + states A/B + the
// A->B transition + a role).
package integration

import (
	"context"
	"net/http"
	"testing"
)

var p627Ctx = context.Background()

// p627TransitionID fetches the current machine's transitions and returns
// the id of the one with the given (from, outcome) pair; it fatals when
// the pair is not present.
func p627TransitionID(t *testing.T, h *Harness, proj, from, outcome string) string {
	t.Helper()
	status, out := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine/transitions", "")
	if status != http.StatusOK {
		t.Fatalf("GET transitions = %d", status)
	}
	list, _ := out["transitions"].([]any)
	for _, e := range list {
		tr := e.(map[string]any)
		if tr["from"] == from && tr["outcome"] == outcome {
			id, _ := tr["id"].(string)
			return id
		}
	}
	t.Fatalf("transition %s--%s not found in %v", from, outcome, list)
	return ""
}

// T6.37.1 — Transition created: POST adds a transition to the current
// machine definition; it is returned with its view and the machine read
// is unchanged in version (within-version edit).
func TestP627CreateTransition(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/transitions"
	status, out := doJSON(t, "POST", base, "{\"from\":\"a\",\"to\":\"b\",\"outcome\":\"retry\",\"priority\":5}")
	if status != http.StatusCreated {
		t.Fatalf("POST transitions = %d (body %v)", status, out)
	}
	if out["from"] != "A" || out["to"] != "B" || out["priority"] != float64(5) || out["enabled"] != true {
		t.Fatalf("created transition = %v, want A->B priority 5 enabled", out)
	}
	// Within-version edit: no full-definition version bump (still v1).
	status, cur := doJSON(t, "GET", h.BaseURL+"/api/v1/projects/"+proj+"/state-machine", "")
	if status != 200 || cur["version"] != float64(1) {
		t.Fatalf("machine after create = %d %v, want still v1", status, cur)
	}
}

// T6.37.2 — Condition persisted: a created transition carries a condition
// that round-trips, and the configure-condition op persists an update (the
// DB ground truth is checked directly — the response reflects the new
// value either way).
func TestP627ConditionPersisted(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/transitions"
	status, out := doJSON(t, "POST", base, "{\"from\":\"a\",\"to\":\"b\",\"outcome\":\"go2\",\"condition\":{\"field\":\"context_usage\",\"operator\":\">=\",\"literal\":0.8}}")
	if status != http.StatusCreated {
		t.Fatalf("POST transition with condition = %d (body %v)", status, out)
	}
	cond, _ := out["condition"].(map[string]any)
	if cond["field"] != "context_usage" || cond["operator"] != ">=" {
		t.Fatalf("condition = %v, want the round-tripped one", cond)
	}
	id, _ := out["id"].(string)
	// Ground truth: the condition row exists in the database.
	var field, operator string
	var literal []byte
	if err := h.DBScan(p627Ctx, "SELECT field, operator, literal FROM transition_conditions WHERE transition_id = '"+id+"'", &field, &operator, &literal); err != nil {
		t.Fatalf("condition row missing from DB: %v", err)
	}
	if field != "context_usage" || operator != ">=" {
		t.Fatalf("DB condition = %v/%v, want context_usage/>=", field, operator)
	}
	// Configure-condition persists an update (context-field value form).
	status, out = doJSON(t, "PUT", base+"/"+id+"/condition", "{\"field\":\"remaining_work_units\",\"operator\":\"==\",\"context_field\":\"project.context_threshold\"}")
	if status != http.StatusOK {
		t.Fatalf("configure condition = %d (body %v)", status, out)
	}
	var cf string
	if err := h.DBScan(p627Ctx, "SELECT field, context_field FROM transition_conditions WHERE transition_id = '"+id+"'", &field, &cf); err != nil {
		t.Fatalf("updated condition row missing: %v", err)
	}
	if field != "remaining_work_units" || cf != "project.context_threshold" {
		t.Fatalf("DB condition after configure = %v/%v", field, cf)
	}
}

// T6.37.3 — Priority persisted: the configure-priority and update ops store
// the value (the response and the transition list reflect it).
func TestP627PriorityPersisted(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/transitions"
	status, _ := doJSON(t, "POST", base, "{\"from\":\"a\",\"to\":\"b\",\"outcome\":\"go3\"}")
	if status != http.StatusCreated {
		t.Fatalf("POST transition = %d", status)
	}
	id := p627TransitionID(t, h, proj, "A", "go3")
	status, out := doJSON(t, "PUT", base+"/"+id+"/priority", "{\"priority\":10}")
	if status != http.StatusOK || out["priority"] != float64(10) {
		t.Fatalf("configure priority = %d %v, want 200 + 10", status, out)
	}
	// The update surface persists too.
	if status, out := doJSON(t, "PUT", base+"/"+id, "{\"priority\":20}"); status != 200 || out["priority"] != float64(20) {
		t.Fatalf("update priority = %d %v, want 200 + 20", status, out)
	}
	// The list surface reflects the stored value.
	status, list := doJSON(t, "GET", base, "")
	if status != 200 {
		t.Fatalf("GET transitions = %d", status)
	}
	for _, e := range list["transitions"].([]any) {
		tr := e.(map[string]any)
		if tr["outcome"] == "go3" && tr["priority"] == float64(20) {
			return
		}
	}
	t.Fatalf("transition list = %v, want go3 at priority 20", list["transitions"])
}

// T6.37.4 — Invalid state reference rejected: an unknown from/to state key
// is a 404 (the state key is not in the current machine).
func TestP627InvalidStateReference(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/transitions"
	if status, out := doJSON(t, "POST", base, "{\"from\":\"ghost\",\"to\":\"b\",\"outcome\":\"x\"}"); status != http.StatusNotFound {
		t.Fatalf("unknown from = %d, want 404 (body %v)", status, out)
	}
	if status, out := doJSON(t, "POST", base, "{\"from\":\"a\",\"to\":\"ghost\",\"outcome\":\"x\"}"); status != http.StatusNotFound {
		t.Fatalf("unknown to = %d, want 404 (body %v)", status, out)
	}
	// Unknown transition id → 404 on the update surface.
	if status, out := doJSON(t, "PUT", base+"/nope", "{\"priority\":1}"); status != http.StatusNotFound {
		t.Fatalf("unknown transition = %d, want 404 (body %v)", status, out)
	}
}

// T6.37.5 — Ambiguous transition configuration rejected: a same-source,
// same-outcome, equal-priority configuration is a conflict. The DB unique
// index (migration 00023) already rejects a same from/outcome CREATE pair
// (409, any priorities), and the P6-14 detector rejects the equal-priority
// configuration through the UPDATE surface — the same from + normalized
// outcome pair already persists with a distinct priority.
func TestP627AmbiguityRejected(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/transitions"
	if status, out := doJSON(t, "POST", base, "{\"from\":\"a\",\"to\":\"b\",\"outcome\":\"go4\",\"priority\":10}"); status != http.StatusCreated {
		t.Fatalf("first create = %d (body %v)", status, out)
	}
	// Same from + same outcome (case-insensitive) create → 409 (the 00023
	// unique index; the ambiguity class at the DB level).
	if status, out := doJSON(t, "POST", base, "{\"from\":\"A\",\"to\":\"b\",\"outcome\":\" GO4 \",\"priority\":20}"); status != http.StatusConflict {
		t.Fatalf("duplicate-outcome create = %d, want 409 (body %v)", status, out)
	}
	// A distinct priority is legal (the index allows any priority).
	if status, out := doJSON(t, "POST", base, "{\"from\":\"a\",\"to\":\"b\",\"outcome\":\"go6\",\"priority\":20}"); status != http.StatusCreated {
		t.Fatalf("second create = %d, want 201 (body %v)", status, out)
	}
	// An update into an equal-priority same-outcome configuration is
	// rejected (the P6-14 detector: same from + normalized outcome + equal
	// priority).
	status, list := doJSON(t, "GET", base, "")
	if status != 200 {
		t.Fatalf("GET transitions = %d", status)
	}
	var id10, id20 string
	for _, e := range list["transitions"].([]any) {
		tr := e.(map[string]any)
		if tr["outcome"] == "go4" && tr["priority"] == float64(10) {
			id10, _ = tr["id"].(string)
		}
		if tr["outcome"] == "go6" && tr["priority"] == float64(20) {
			id20, _ = tr["id"].(string)
		}
	}
	if id10 == "" || id20 == "" {
		t.Fatalf("fixture rows not found in %v", list["transitions"])
	}
	// Equal priority is fine while the outcomes differ.
	if status, out := doJSON(t, "PUT", base+"/"+id20, "{\"priority\":10}"); status != http.StatusOK {
		t.Fatalf("distinct-outcome equal-priority update = %d, want 200 (body %v)", status, out)
	}
	// Now move go6 onto the same normalized outcome as go4 at the same
	// priority → the detector rejects (409).
	if status, out := doJSON(t, "PUT", base+"/"+id20, "{\"outcome\":\" GO4 \",\"priority\":10}"); status != http.StatusConflict {
		t.Fatalf("ambiguous update = %d, want 409 (body %v)", status, out)
	}
	// The rejected update did not mutate the row.
	status, chk := doJSON(t, "GET", base, "")
	if status != 200 {
		t.Fatalf("GET transitions = %d", status)
	}
	for _, e := range chk["transitions"].([]any) {
		tr := e.(map[string]any)
		if tr["id"] == id20 {
			if tr["outcome"] != "go6" || tr["priority"] != float64(10) {
				t.Fatalf("rejected update mutated the row: %v", tr)
			}
		}
	}
}

// T6.37.5 (guard) — the DB-level duplicate rule applies to disabled rows
// too: a same from/outcome create pair is 409 regardless of liveness (the
// 00023 unique index has no liveness exception; the detector-skip path for
// a disabled candidate is pinned by the unit half).
func TestP627AmbiguitySkipsDisabled(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p626Seed(t, h)
	base := h.BaseURL + "/api/v1/projects/" + proj + "/state-machine/transitions"
	status, out := doJSON(t, "POST", base, "{\"from\":\"a\",\"to\":\"b\",\"outcome\":\"go5b\",\"priority\":7,\"enabled\":false}")
	if status != http.StatusCreated {
		t.Fatalf("disabled create (distinct outcome) = %d, want 201 (body %v)", status, out)
	}
	// Same from + same outcome (disabled) → 409: the DB index has no
	// liveness exception.
	if status, out := doJSON(t, "POST", base, "{\"from\":\"a\",\"to\":\"b\",\"outcome\":\"go5b\",\"priority\":8}"); status != http.StatusConflict {
		t.Fatalf("same-outcome create = %d, want 409 (body %v)", status, out)
	}
}

// The migration (00032) is applied and the max version pin moved to 32.
func TestP627MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	var version int
	if err := h.DBScan(p627Ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// 31 -> 32 by P6-27 (migration 00032, transition_conditions) —
	// accepted contract change.
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("max migration version = %d, want %d", version, want)
	}
	var tableName string
	if err := h.DBScan(p627Ctx,
		"SELECT table_name FROM information_schema.tables WHERE table_name = 'transition_conditions'", &tableName); err != nil {
		t.Fatalf("transition_conditions missing: %v", err)
	}
	if tableName != "transition_conditions" {
		t.Fatalf("table = %q", tableName)
	}
}
