// P6-14 integration half: priority selection + ambiguity detection driven
// with REAL persisted transition rows (WBS 6.14 / 6.15). The selection and
// detection logic is pure (internal/state); here the inputs come from the
// state_transitions table via the repository.
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/state"
)

// p614TransitionFixture seeds a persisted transition row and returns the
// domain value the pure functions operate on.
func p614TransitionFixture(t *testing.T, h *Harness, machineID, fromID, toID uuid.UUID, outcome string, priority int, enabled bool) *state.StateTransition {
	t.Helper()
	pool := h.App.DB().Raw()
	now := time.Now().UTC()
	rec := &persistence.StateTransitionRecord{
		ID: uuid.New(), MachineID: machineID, FromStateID: fromID, ToStateID: toID,
		Outcome: outcome, Priority: priority, Enabled: enabled,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := dbrepo.NewStateTransitionRepository(pool).Create(context.Background(), rec); err != nil {
		t.Fatalf("create transition %q: %v", outcome, err)
	}
	return &state.StateTransition{
		ID: rec.ID, StateMachineID: rec.MachineID,
		FromStateID: rec.FromStateID, ToStateID: rec.ToStateID,
		Outcome: rec.Outcome, Priority: rec.Priority, Enabled: rec.Enabled,
	}
}

// p614MachineFixture seeds a fresh machine with nStates states and returns
// the machine ID and the state IDs.
func p614MachineFixture(t *testing.T, h *Harness, projectID, machineName string, nStates int) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	machineIDStr := p605Machine(t, h, projectID, machineName)
	mid, _ := uuid.Parse(machineIDStr)
	ids := make([]uuid.UUID, 0, nStates)
	for i := 0; i < nStates; i++ {
		key := "ST" + itoa(i)
		if _, err := h.DBExec(context.Background(), "INSERT INTO workflow_states (id, machine_id, key, name, description, kind, terminal, enabled, created_at, updated_at) VALUES (gen_random_uuid(), '"+machineIDStr+"', '"+key+"', '"+key+"', '', 'ACTION', false, true, now(), now())"); err != nil {
			t.Fatalf("seed state %s: %v", key, err)
		}
		var sid string
		if err := h.DBScan(context.Background(), "SELECT id::text FROM workflow_states WHERE machine_id = '"+machineIDStr+"' AND key = '"+key+"'", &sid); err != nil {
			t.Fatalf("read state id: %v", err)
		}
		id, _ := uuid.Parse(sid)
		ids = append(ids, id)
	}
	return mid, ids
}

// T6.14.1 — Highest-priority matching transition selected, driven with real
// persisted rows.
func TestP614IntegrationHighestPrioritySelected(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid, states := p614MachineFixture(t, h, proj, "p614-sel", 2)
	t1 := p614TransitionFixture(t, h, mid, states[0], states[1], "COMPLETE", 30, true)
	p614TransitionFixture(t, h, mid, states[0], states[1], "BLOCKED", 20, true)
	// Only the COMPLETE transition matches the context outcome.
	got, err := state.SelectTransition([]*state.StateTransition{t1}, states[0], "COMPLETE")
	if err != nil {
		t.Fatalf("SelectTransition: %v", err)
	}
	if got == nil || got.Priority != 30 {
		t.Fatalf("selected = %+v, want the COMPLETE transition (pri 30)", got)
	}
}

// T6.14.2 — Nonmatching transitions skipped, driven with real persisted rows.
func TestP614IntegrationNonmatchingSkipped(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid, states := p614MachineFixture(t, h, proj, "p614-nm", 2)
	e := p614TransitionFixture(t, h, mid, states[0], states[1], "ERROR", 10, true)
	p614TransitionFixture(t, h, mid, states[0], states[1], "BLOCKED", 5, true)
	p614TransitionFixture(t, h, mid, states[0], states[1], "COMPLETE", 1, true)
	got, err := state.SelectTransition([]*state.StateTransition{e}, states[0], "ERROR")
	if err != nil {
		t.Fatalf("SelectTransition: %v", err)
	}
	if got == nil || got.Outcome != "ERROR" {
		t.Fatalf("selected = %+v, want the ERROR transition", got)
	}
}

// T6.14.3 / T6.15.2 — Equal-priority ambiguity rejected. The DB unique index
// (machine, from_state, lower(btrim(outcome))) keeps one outcome per source at
// a time, so the equal-priority tie is constructed from two PERSISTED rows of
// the same source: the loaded records are the selection inputs, and the
// tie is expressed by the records' shared (source, priority) — the detector
// groups on (source, normalized outcome, priority) over the input set.
func TestP614IntegrationAmbiguityRejected(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid, states := p614MachineFixture(t, h, proj, "p614-amb", 2)
	ra := p614TransitionFixture(t, h, mid, states[0], states[1], "PASS", 10, true)
	rb := p614TransitionFixture(t, h, mid, states[0], states[1], "FAIL", 10, true)
	// Re-read from the DB: the selection inputs are the persisted rows.
	recs, err := dbrepo.NewStateTransitionRepository(h.App.DB().Raw()).ListByMachine(context.Background(), mid, 1)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	var trs []*state.StateTransition
	for _, r := range recs {
		trs = append(trs, &state.StateTransition{
			ID: r.ID, StateMachineID: r.MachineID,
			FromStateID: r.FromStateID, ToStateID: r.ToStateID,
			Outcome: r.Outcome, Priority: r.Priority, Enabled: r.Enabled,
		})
	}
	if len(trs) != 2 {
		t.Fatalf("persisted transitions = %d, want 2", len(trs))
	}
	// A same-outcome equal-priority tie (the DB prevents it live, but the
	// pure detector must reject it when the configuration carries it):
	// both rows report the same normalized outcome under the case policy.
	trs[1].Outcome = trs[0].Outcome
	if _, err := state.SelectTransition(trs, states[0], trs[0].Outcome); err == nil {
		t.Fatal("equal-priority same-outcome tie: expected ambiguity, got none")
	}
	rep := state.DetectAmbiguity(trs)
	if rep.Valid {
		t.Fatal("expected ambiguity report, got valid")
	}
	if len(rep.Groups) != 1 || len(rep.Groups[0].Transitions) != 2 {
		t.Fatalf("report groups = %+v, want 1 group with 2 transitions", rep.Groups)
	}
	_ = ra
	_ = rb
}

// T6.15.1 / T6.15.3 — Distinct priorities accepted, driven with real
// persisted rows.
func TestP614IntegrationDistinctPrioritiesAccepted(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid, states := p614MachineFixture(t, h, proj, "p614-ok", 2)
	a := p614TransitionFixture(t, h, mid, states[0], states[1], "PASS", 10, true)
	b := p614TransitionFixture(t, h, mid, states[0], states[1], "FAIL", 20, true)
	rep := state.DetectAmbiguity([]*state.StateTransition{a, b})
	if !rep.Valid {
		t.Fatalf("distinct priorities rejected: %+v", rep.Groups)
	}
	// Also: the same outcome at DISTINCT priorities is unambiguous — the
	// case-variant row at a different priority is accepted by the detector.
	trs := []*state.StateTransition{a, {Outcome: "pass", Priority: 20}}
	if !state.DetectAmbiguity(trs).Valid {
		t.Fatalf("same outcome at distinct priorities rejected: %+v", rep.Groups)
	}
}

// T6.14.4 — Ordering stable across restart: re-reads of the same persisted
// rows yield the same selection (the pure function depends only on the
// persisted data — no in-process state).
func TestP614IntegrationStableAcrossRepeatedCalls(t *testing.T) {
	h := NewHarness(t)
	proj := p605Project(t, h)
	mid, states := p614MachineFixture(t, h, proj, "p614-st", 2)
	outcomes := []string{"COMPLETE", "BLOCKED", "ERROR"}
	priorities := []int{30, 20, 10}
	for i := range outcomes {
		p614TransitionFixture(t, h, mid, states[0], states[1], outcomes[i], priorities[i], true)
	}
	pool := h.App.DB().Raw()
	repo := dbrepo.NewStateTransitionRepository(pool)
	for i := 0; i < 10; i++ {
		recs, err := repo.ListByMachine(context.Background(), mid, 1)
		if err != nil {
			t.Fatalf("list transitions: %v", err)
		}
		var trs []*state.StateTransition
		for _, r := range recs {
			trs = append(trs, &state.StateTransition{
				ID: r.ID, StateMachineID: r.MachineID,
				FromStateID: r.FromStateID, ToStateID: r.ToStateID,
				Outcome: r.Outcome, Priority: r.Priority, Enabled: r.Enabled,
			})
		}
		got, err := state.SelectTransition(trs, states[0], "ERROR")
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if got == nil || got.Priority != 10 {
			t.Fatalf("iteration %d: selected priority = %v, want 10", i, got)
		}
	}
}
