package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P6-11 (WBS 6.3): the default development-workflow template seeded as data —
// the full machine + states + transitions — idempotently, with a valid
// initial state, a terminal PROJECT_DONE, and copy-to-project with fresh IDs.
// The service is driven directly over the harness pool (WBS 6.3 is a service
// op, no REST endpoint).

// p611Svc wires the state-template service over the harness pool.
func p611Svc(t *testing.T, h *Harness) *service.StateTemplateService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewStateTemplateService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewProjectRepository(pool),
	).WithTxPool(pool)
}

// p611Count returns the row count of a table.
func p611Count(t *testing.T, h *Harness, table string) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(), "SELECT count(*) FROM "+table, &n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// p611MachinesForProject returns the state_machines row count for a project id.
func p611MachinesForProject(t *testing.T, h *Harness, projectID string) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(),
		fmt.Sprintf("SELECT count(*) FROM state_machines WHERE project_id = '%s'", projectID), &n); err != nil {
		t.Fatalf("count machines: %v", err)
	}
	return n
}

// T6.3.1 — The default workflow seeds as data: the machine, its states, and
// its transitions are all present as rows. The default is the statefile
// workflow v3 (23 states, 40 transitions; see service.statefileTemplate).
func TestP611SeedAsData(t *testing.T) {
	h := NewHarness(t)
	svc := p611Svc(t, h)
	ctx := context.Background()

	machineID, err := svc.SeedDefaultTemplate(ctx)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := uuid.Parse(machineID); err != nil {
		t.Fatalf("seed returned non-uuid machine id %q: %v", machineID, err)
	}
	if got := p611Count(t, h, "state_machines"); got != 1 {
		t.Fatalf("state_machines rows = %d, want 1", got)
	}
	if got := p611Count(t, h, "workflow_states"); got != 23 {
		t.Fatalf("workflow_states rows = %d, want 23", got)
	}
	if got := p611Count(t, h, "state_transitions"); got != 40 {
		t.Fatalf("state_transitions rows = %d, want 40", got)
	}
}

// T6.3.2 — Seeding is idempotent: a second seed adds no duplicate rows and
// returns the same machine.
func TestP611SeedIdempotent(t *testing.T) {
	h := NewHarness(t)
	svc := p611Svc(t, h)
	ctx := context.Background()

	id1, err := svc.SeedDefaultTemplate(ctx)
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	id2, err := svc.SeedDefaultTemplate(ctx)
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("seed idempotency: machine id changed (%s -> %s)", id1, id2)
	}
	if got := p611Count(t, h, "state_machines"); got != 1 {
		t.Fatalf("state_machines after re-seed = %d, want 1", got)
	}
	if got := p611Count(t, h, "workflow_states"); got != 23 {
		t.Fatalf("workflow_states after re-seed = %d, want 23", got)
	}
	if got := p611Count(t, h, "state_transitions"); got != 40 {
		t.Fatalf("state_transitions after re-seed = %d, want 40", got)
	}
}

// T6.3.3 — Every transition endpoint and the initial state reference a state
// the seed created (no dangling references).
func TestP611ReferencedStatesExist(t *testing.T) {
	h := NewHarness(t)
	svc := p611Svc(t, h)
	ctx := context.Background()
	if _, err := svc.SeedDefaultTemplate(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Initial state must be a real seeded state.
	var iniCount int
	if err := h.DBScan(ctx,
		`SELECT count(*) FROM state_machines m
		 LEFT JOIN workflow_states s ON s.id = m.initial_state_id
		 WHERE s.id IS NULL`, &iniCount); err != nil {
		t.Fatalf("initial-state join: %v", err)
	}
	if iniCount != 0 {
		t.Fatalf("machines with missing initial state = %d, want 0", iniCount)
	}
	// Both transition endpoints must be seeded states of the same machine.
	var badEndpoints int
	if err := h.DBScan(ctx,
		`SELECT count(*) FROM state_transitions tr
		 WHERE NOT EXISTS (SELECT 1 FROM workflow_states f
		                    WHERE f.id = tr.from_state_id AND f.machine_id = tr.machine_id)
		    OR NOT EXISTS (SELECT 1 FROM workflow_states dst
		                    WHERE dst.id = tr.to_state_id AND dst.machine_id = tr.machine_id)`, &badEndpoints); err != nil {
		t.Fatalf("endpoint join: %v", err)
	}
	if badEndpoints != 0 {
		t.Fatalf("transitions with dangling endpoint = %d, want 0", badEndpoints)
	}
}

// T6.3.4 — The initial state is PROJECT_INIT and is a valid (non-terminal)
// state of the machine.
func TestP611InitialStateIsProjectInit(t *testing.T) {
	h := NewHarness(t)
	svc := p611Svc(t, h)
	ctx := context.Background()
	if _, err := svc.SeedDefaultTemplate(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var key string
	var terminal bool
	if err := h.DBScan(ctx,
		`SELECT s.key, s.terminal FROM state_machines m
		 JOIN workflow_states s ON s.id = m.initial_state_id`, &key, &terminal); err != nil {
		t.Fatalf("read initial state: %v", err)
	}
	if key != "PROJECT_INIT" {
		t.Fatalf("initial state key = %q, want PROJECT_INIT", key)
	}
	if terminal {
		t.Fatalf("initial state PROJECT_INIT is terminal, want non-terminal")
	}
}

// T6.3.5 — PROJECT_DONE is the (only) terminal state.
func TestP611ProjectDoneTerminal(t *testing.T) {
	h := NewHarness(t)
	svc := p611Svc(t, h)
	ctx := context.Background()
	if _, err := svc.SeedDefaultTemplate(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var key string
	var n int
	if err := h.DBScan(ctx,
		"SELECT key FROM workflow_states WHERE terminal = true", &key); err != nil {
		t.Fatalf("read terminal state: %v", err)
	}
	if key != "PROJECT_DONE" {
		t.Fatalf("terminal state key = %q, want PROJECT_DONE", key)
	}
	if err := h.DBScan(ctx, "SELECT count(*) FROM workflow_states WHERE terminal = true", &n); err != nil {
		t.Fatalf("count terminal: %v", err)
	}
	if n != 1 {
		t.Fatalf("terminal state count = %d, want exactly 1", n)
	}
}

// T6.3.6 — The template copies to a project with fresh IDs and intact
// structure: the copy belongs to the target project, has a new machine ID,
// the same 23 states / 40 transitions (statefile workflow v3), fresh state IDs, and a remapped initial
// state pointing at the copy's own PROJECT_INIT.
func TestP611CopyToProject(t *testing.T) {
	h := NewHarness(t)
	svc := p611Svc(t, h)
	ctx := context.Background()

	srcID, err := svc.SeedDefaultTemplate(ctx)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	targetID := createPhaseProject(t, h)
	if targetID == srcID {
		t.Fatalf("target project equals source machine id (helper collision)")
	}
	copyID, err := svc.CopyToProject(ctx, targetID)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if copyID == srcID {
		t.Fatalf("copy reused the source machine id %s", srcID)
	}
	if p611MachinesForProject(t, h, targetID) != 1 {
		t.Fatalf("target project machine count != 1")
	}
	var copyStates, copyTransitions int
	if err := h.DBScan(ctx, fmt.Sprintf("SELECT count(*) FROM workflow_states WHERE machine_id = '%s'", copyID), &copyStates); err != nil {
		t.Fatalf("count copy states: %v", err)
	}
	if err := h.DBScan(ctx, fmt.Sprintf("SELECT count(*) FROM state_transitions WHERE machine_id = '%s'", copyID), &copyTransitions); err != nil {
		t.Fatalf("count copy transitions: %v", err)
	}
	if copyStates != 23 || copyTransitions != 40 {
		t.Fatalf("copy structure = %d states / %d transitions, want 23/40", copyStates, copyTransitions)
	}
	// Fresh IDs: the copy's state ids must be disjoint from the source's.
	var overlap int
	if err := h.DBScan(ctx,
		fmt.Sprintf("SELECT count(*) FROM workflow_states WHERE machine_id = '%s' AND id IN (SELECT id FROM workflow_states WHERE machine_id = '%s')", copyID, srcID),
		&overlap); err != nil {
		t.Fatalf("overlap query: %v", err)
	}
	if overlap != 0 {
		t.Fatalf("copy shares %d state IDs with the source, want fresh ids", overlap)
	}
	// The copy's initial state is its OWN PROJECT_INIT (remapped, not the
	// source id).
	var iniKey string
	if err := h.DBScan(ctx,
		fmt.Sprintf("SELECT s.key FROM state_machines m JOIN workflow_states s ON s.id = m.initial_state_id WHERE m.id = '%s'", copyID),
		&iniKey); err != nil {
		t.Fatalf("read copy initial state: %v", err)
	}
	if iniKey != "PROJECT_INIT" {
		t.Fatalf("copy initial state = %q, want PROJECT_INIT", iniKey)
	}
	// Copy is idempotent: a second copy into the same project reuses the row.
	copyID2, err := svc.CopyToProject(ctx, targetID)
	if err != nil {
		t.Fatalf("copy 2: %v", err)
	}
	if copyID2 != copyID {
		t.Fatalf("copy idempotency: copy id changed (%s -> %s)", copyID, copyID2)
	}
	if p611MachinesForProject(t, h, targetID) != 1 {
		t.Fatalf("target project machine count after re-copy != 1")
	}
}

// Concurrency (WBS 6.3 gate) — concurrent seeding does not duplicate rows and
// returns a consistent machine id, under -race.
func TestP611SeedConcurrent(t *testing.T) {
	h := NewHarness(t)
	svc := p611Svc(t, h)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = svc.SeedDefaultTemplate(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent seed %d: %v", i, err)
		}
	}
	first := results[0]
	for i, id := range results {
		if id != first {
			t.Fatalf("concurrent seed returned divergent machine ids (index %d = %s vs %s)", i, id, first)
		}
	}
	if got := p611Count(t, h, "state_machines"); got != 1 {
		t.Fatalf("state_machines after concurrent seed = %d, want 1", got)
	}
	if got := p611Count(t, h, "workflow_states"); got != 23 {
		t.Fatalf("workflow_states after concurrent seed = %d, want 23", got)
	}
	if got := p611Count(t, h, "state_transitions"); got != 40 {
		t.Fatalf("state_transitions after concurrent seed = %d, want 40", got)
	}
}

// Sequential fallback (nil txPool) — the op still works and is idempotent
// without a transaction pool (single-goroutine scope).
func TestP611SequentialFallback(t *testing.T) {
	h := NewHarness(t)
	pool := h.App.DB().Raw()
	svc := service.NewStateTemplateService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewProjectRepository(pool),
	)
	ctx := context.Background()
	id1, err := svc.SeedDefaultTemplate(ctx)
	if err != nil {
		t.Fatalf("sequential seed: %v", err)
	}
	id2, err := svc.SeedDefaultTemplate(ctx)
	if err != nil {
		t.Fatalf("sequential reseed: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("sequential seed not idempotent: %s vs %s", id1, id2)
	}
}
