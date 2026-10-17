package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/state"
)

// P6-29 (WBS 6.39): the state-machine visualization read model — a
// service read over the CURRENT machine. Every state is a node (STATE,
// ROLE, SYSTEM PROMPT VERSION, STATE INSTRUCTION VERSION, STATE FILE
// REQUIREMENTS, PROMPT TEMPLATE, TERMINAL?); every transition is an
// edge (OUTCOME, CONDITION, PRIORITY, DESTINATION); deterministic
// ordering (T6.39.1-T6.39.5). No REST surface: the op is driven over
// the harness pool, read-only — the -race suite covers concurrency and
// the nil-pool parity test is the sequential-fallback evidence.

// p629Viz returns the visualization read model of the project's current
// machine through the snapshot service's Visualization op; usePool
// selects the transactional (wired pool) vs the sequential (nil pool)
// path.
func p629Viz(t *testing.T, h *Harness, projectID string, usePool bool) *state.StateMachineVisualization {
	t.Helper()
	pool := h.App.DB().Raw()
	svc := service.NewStateMachineSnapshotService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewStateInstructionRepository(pool),
		dbrepo.NewStateRoleAssignmentRepository(pool),
		dbrepo.NewStateFileRequirementRepository(pool),
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewSystemPromptRepository(pool),
		dbrepo.NewTransitionConditionRepository(pool),
	)
	if usePool {
		svc = svc.WithTxPool(pool)
	}
	viz, err := svc.Visualization(context.Background(), projectID)
	if err != nil {
		t.Fatalf("visualization: %v", err)
	}
	return viz
}

func p629Node(viz *state.StateMachineVisualization, key string) *state.VizNode {
	for i := range viz.Nodes {
		if viz.Nodes[i].Key == key {
			return &viz.Nodes[i]
		}
	}
	return nil
}

// p629ProjectOf returns the project id that owns machineID.
func p629ProjectOf(t *testing.T, h *Harness, machineID string) string {
	t.Helper()
	var proj string
	if err := h.DBScan(context.Background(), "SELECT project_id::text FROM state_machines WHERE id = '"+machineID+"'", &proj); err != nil {
		t.Fatalf("read project: %v", err)
	}
	return proj
}

// p629Seed builds the T6.39 fixture: the p618 machine (states A/B/C,
// transitions A-GO->B and B-DONE->C, an instruction on A, the role
// assignment on A, a REPORT state-file requirement on A) extended with
// a role system-prompt history (v1..v3 — the CURRENT version is 3), a
// second state-instruction version on A (current = 2), and a third
// transition A-RETRY->C with a structured condition and a distinct
// priority (the ordering test needs same-outcome, distinct-priority and
// distinct-outcome edges).
func p629Seed(t *testing.T, h *Harness) (projectID, machineID string) {
	t.Helper()
	ctx := context.Background()
	pool := h.App.DB().Raw()
	machineID = p618Seed(t, h)
	proj := p629ProjectOf(t, h, machineID)

	var roleID, aID string
	if err := h.DBScan(ctx, "SELECT id::text FROM roles WHERE key = 'P618-ROLE'", &roleID); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+machineID+"' AND key = 'A'", &aID); err != nil {
		t.Fatalf("read state A: %v", err)
	}
	// Role system-prompt history: the node exposes the CURRENT version (3).
	prompts := dbrepo.NewSystemPromptRepository(pool)
	for _, content := range []string{"prompt v1", "prompt v2", "prompt v3"} {
		if _, err := prompts.AppendVersion(ctx, roleID, content); err != nil {
			t.Fatalf("append prompt: %v", err)
		}
	}
	// Second state-instruction version on A: the node's instruction
	// version is the CURRENT one (2).
	instr := dbrepo.NewStateInstructionRepository(pool)
	if _, err := instr.AppendVersion(ctx, uuid.MustParse(aID), "second version"); err != nil {
		t.Fatalf("append instruction: %v", err)
	}
	// Flag C as a terminal state so the node's TERMINAL? field is exercised.
	if _, err := h.DBExec(ctx, "UPDATE workflow_states SET terminal = true WHERE machine_id = '"+machineID+"' AND key = 'C'"); err != nil {
		t.Fatalf("flag C terminal: %v", err)
	}
	// Third transition A-RETRY->C (priority 3) with a structured
	// condition (T6.39.2 edge CONDITION source: P6-27).
	trans := dbrepo.NewStateTransitionRepository(pool)
	var cID string
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+machineID+"' AND key = 'C'", &cID); err != nil {
		t.Fatalf("read state C: %v", err)
	}
	now := time.Now().UTC()
	tid := uuid.New()
	if err := trans.Create(ctx, &persistence.StateTransitionRecord{
		ID: tid, MachineID: uuid.MustParse(machineID), Version: 1,
		FromStateID: uuid.MustParse(aID), ToStateID: uuid.MustParse(cID),
		Outcome: "RETRY", Priority: 3, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create transition: %v", err)
	}
	if err := dbrepo.NewTransitionConditionRepository(pool).Create(ctx, tid, &persistence.TransitionCondition{
		Field: "validation_passed", Operator: "==", Literal: true,
	}); err != nil {
		t.Fatalf("create condition: %v", err)
	}
	return proj, machineID
}

// T6.39.1 + T6.39.5 — All nodes returned, in deterministic node order
// (state key ascending), each carrying its full node metadata: the
// terminal flag, the prompt template, and the state-file requirements.
func TestP629AllNodes(t *testing.T) {
	h := NewHarness(t)
	proj, mid := p629Seed(t, h)
	viz := p629Viz(t, h, proj, false)
	if viz.MachineID.String() != mid || viz.Version != 1 {
		t.Fatalf("viz = machine %s version %d, want %s version 1 (current machine)", viz.MachineID, viz.Version, mid)
	}
	got := make([]string, 0, len(viz.Nodes))
	for _, n := range viz.Nodes {
		got = append(got, n.Key)
	}
	want := []string{"A", "B", "C"}
	if len(got) != 3 {
		t.Fatalf("node keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("node keys = %v, want %v (state-key-ascending order)", got, want)
		}
	}
	c := p629Node(viz, "C")
	if c == nil || !c.Terminal {
		t.Fatalf("state C terminal = %+v, want TERMINAL=true", c)
	}
	a := p629Node(viz, "A")
	if a == nil || a.Terminal {
		t.Fatalf("state A terminal = %+v, want TERMINAL=false", a)
	}
	if a == nil || len(a.StateFiles) != 1 || a.StateFiles[0] != "REPORT" {
		t.Fatalf("state A state files = %+v, want [REPORT]", a)
	}
}

// T6.39.2 — All edges returned, addressed by state key, with OUTCOME,
// CONDITION (the P6-27 structured condition, nil when absent), PRIORITY,
// DESTINATION — in deterministic edge order (outcome, then priority,
// then from key, then destination key).
func TestP629AllEdges(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p629Seed(t, h)
	viz := p629Viz(t, h, proj, false)
	type edgeKey struct{ outcome, from, dest string }
	got := []edgeKey{{"DONE", "B", "C"}, {"GO", "A", "B"}, {"RETRY", "A", "C"}}
	if len(viz.Edges) != 3 {
		t.Fatalf("edge count = %d, want 3: %+v", len(viz.Edges), viz.Edges)
	}
	for i, e := range viz.Edges {
		k := edgeKey{e.Outcome, e.From, e.Destination}
		if k != got[i] {
			t.Fatalf("edge %d = %+v, want %+v (outcome/priority-ascending order)", i, k, got[i])
		}
	}
	byOutcome := map[string]*state.VizEdge{}
	for i := range viz.Edges {
		byOutcome[viz.Edges[i].Outcome] = &viz.Edges[i]
	}
	if byOutcome["RETRY"].Priority != 3 {
		t.Fatalf("RETRY priority = %d, want 3", byOutcome["RETRY"].Priority)
	}
	wantCond := &state.VizEdgeCondition{Field: "validation_passed", Operator: "==", Literal: true}
	if byOutcome["RETRY"].Condition == nil ||
		byOutcome["RETRY"].Condition.Field != wantCond.Field ||
		byOutcome["RETRY"].Condition.Operator != wantCond.Operator {
		t.Fatalf("RETRY condition = %+v, want %+v", byOutcome["RETRY"].Condition, wantCond)
	}
	if lit, ok := byOutcome["RETRY"].Condition.Literal.(bool); !ok || !lit {
		t.Fatalf("RETRY condition literal = %v, want true", byOutcome["RETRY"].Condition.Literal)
	}
	if byOutcome["GO"].Condition != nil || byOutcome["DONE"].Condition != nil {
		t.Fatalf("GO/DONE conditions = %+v / %+v, want nil", byOutcome["GO"].Condition, byOutcome["DONE"].Condition)
	}
}

// T6.39.3 — Node role metadata: the role key resolves from the persisted
// assignment and the system-prompt version is the CURRENT version of the
// role's append-only history (3 after three appends); a state without
// assignments carries an empty role section.
func TestP629RoleMetadata(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p629Seed(t, h)
	viz := p629Viz(t, h, proj, false)
	a := p629Node(viz, "A")
	if a == nil {
		t.Fatal("state A missing")
	}
	if len(a.Roles) != 1 || a.Roles[0].Key != "P618-ROLE" || a.Roles[0].SystemPromptVersion != 3 {
		t.Fatalf("state A roles = %+v, want [{P618-ROLE 3}]", a.Roles)
	}
	b := p629Node(viz, "B")
	if b == nil {
		t.Fatal("state B missing")
	}
	if len(b.Roles) != 0 {
		t.Fatalf("state B roles = %+v, want empty section", b.Roles)
	}
}

// T6.39.4 — Node instruction versions: the state's CURRENT state-instruction
// version from the instruction_versions history (2 after two appends on
// A); a state without an instruction carries a nil version section.
func TestP629InstructionVersions(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p629Seed(t, h)
	viz := p629Viz(t, h, proj, false)
	a := p629Node(viz, "A")
	if a == nil {
		t.Fatal("state A missing")
	}
	if a.InstructionVersion == nil || *a.InstructionVersion != 2 {
		t.Fatalf("state A instruction version = %v, want 2", a.InstructionVersion)
	}
	b := p629Node(viz, "B")
	if b == nil {
		t.Fatal("state B missing")
	}
	if b.InstructionVersion != nil {
		t.Fatalf("state B instruction version = %v, want nil", b.InstructionVersion)
	}
}

// T6.39.5 — Deterministic ordering: the read model is byte-stable — two
// consecutive calls (and both assembly paths) produce identical
// marshalling, so the future web editor sees a stable document.
func TestP629Deterministic(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p629Seed(t, h)
	seq := p629Viz(t, h, proj, false)
	again := p629Viz(t, h, proj, false)
	txv := p629Viz(t, h, proj, true)
	marshal := func(v *state.StateMachineVisualization) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return string(b)
	}
	if marshal(seq) != marshal(again) {
		t.Fatal("two consecutive reads differ")
	}
	if marshal(seq) != marshal(txv) {
		t.Fatal("sequential and transactional reads differ")
	}
}

// Parity (sequential fallback): the nil-pool (sequential direct calls)
// and the wired-pool (one transaction) assembly paths agree byte-for-byte
// on the full node/edge model.
func TestP629NilPoolParity(t *testing.T) {
	h := NewHarness(t)
	proj, _ := p629Seed(t, h)
	pool := h.App.DB().Raw()
	svc := service.NewStateMachineSnapshotService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewStateInstructionRepository(pool),
		dbrepo.NewStateRoleAssignmentRepository(pool),
		dbrepo.NewStateFileRequirementRepository(pool),
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewSystemPromptRepository(pool),
		dbrepo.NewTransitionConditionRepository(pool),
	)
	// No WithTxPool: sequential fallback.
	seq, err := svc.Visualization(context.Background(), proj)
	if err != nil {
		t.Fatalf("sequential visualization: %v", err)
	}
	tx, err := svc.WithTxPool(pool).Visualization(context.Background(), proj)
	if err != nil {
		t.Fatalf("transactional visualization: %v", err)
	}
	b1, err := json.Marshal(seq)
	if err != nil {
		t.Fatalf("marshal seq: %v", err)
	}
	b2, err := json.Marshal(tx)
	if err != nil {
		t.Fatalf("marshal tx: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("sequential vs transactional differ: %s vs %s", b1, b2)
	}
}
