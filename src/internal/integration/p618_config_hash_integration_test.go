package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/state"
)

// P6-18 (WBS 6.21): deterministic machine configuration hash — integration
// half, driving the real repositories over the harness pool.

// p618BuildDefinition loads the persisted definition of ONE machine version
// through the existing version-scoped repositories (no duplication of the
// P6-17 versioning logic): the machine row, its version's states (with
// their current instruction version/hash, role assignments, and
// state-file requirements), and its version's transitions.
func p618BuildDefinition(t *testing.T, h *Harness, machineID string, version int) *state.MachineDefinition {
	t.Helper()
	ctx := context.Background()
	pool := h.App.DB().Raw()
	machines := dbrepo.NewStateMachineRepository(pool)
	statesRepo := dbrepo.NewWorkflowStateRepository(pool)
	transRepo := dbrepo.NewStateTransitionRepository(pool)
	instrRepo := dbrepo.NewStateInstructionRepository(pool)
	assignRepo := dbrepo.NewStateRoleAssignmentRepository(pool)
	fileRepo := dbrepo.NewStateFileRequirementRepository(pool)
	roles := dbrepo.NewRoleRepository(pool)

	mrec, err := machines.GetVersion(ctx, uuid.MustParse(machineID), version)
	if err != nil {
		t.Fatalf("get machine version: %v", err)
	}
	def := &state.MachineDefinition{
		Name: mrec.Name, Description: mrec.Description, Version: mrec.Version,
		Enabled: mrec.Enabled,
	}
	states, err := statesRepo.ListByMachine(ctx, mrec.ID, version)
	if err != nil {
		t.Fatalf("list states: %v", err)
	}
	for _, s := range states {
		sd := state.StateConfigDef{
			Key: s.Key, Name: s.Name, Description: s.Description,
			PromptTemplate: s.PromptTemplate, Kind: s.Kind,
			Terminal: s.Terminal, Enabled: s.Enabled,
		}
		cur, err := instrRepo.GetCurrentVersion(ctx, s.ID)
		if err != nil {
			t.Fatalf("current instruction version: %v", err)
		}
		if cur > 0 {
			v, err := instrRepo.GetVersion(ctx, s.ID, cur)
			if err != nil {
				t.Fatalf("instruction version: %v", err)
			}
			sd.InstructionVersion = cur
			sd.InstructionHash = v.ContentHash
		}
		asgns, err := assignRepo.ListByState(ctx, s.ID)
		if err != nil {
			t.Fatalf("list assignments: %v", err)
		}
		for _, a := range asgns {
			r, err := roles.Get(ctx, a.RoleID.String())
			if err != nil {
				t.Fatalf("get role: %v", err)
			}
			sd.Roles = append(sd.Roles, r.Key)
		}
		files, err := fileRepo.ListByState(ctx, s.ID)
		if err != nil {
			t.Fatalf("list state files: %v", err)
		}
		for _, f := range files {
			sd.StateFiles = append(sd.StateFiles, f.Category)
		}
		def.States = append(def.States, sd)
	}
	trans, err := transRepo.ListByMachine(ctx, mrec.ID, version)
	if err != nil {
		t.Fatalf("list transitions: %v", err)
	}
	keyOf := map[uuid.UUID]string{}
	for _, s := range states {
		keyOf[s.ID] = s.Key
	}
	for _, tr := range trans {
		def.Transitions = append(def.Transitions, state.TransitionConfigDef{
			From: keyOf[tr.FromStateID], To: keyOf[tr.ToStateID],
			Outcome: tr.Outcome, Priority: tr.Priority, Enabled: tr.Enabled,
		})
	}
	if mrec.InitialStateID != nil {
		def.InitialKey = keyOf[*mrec.InitialStateID]
	}
	return def
}

func p618Hash(t *testing.T, h *Harness, machineID string, version int) string {
	t.Helper()
	return state.MachineConfigurationHash(p618BuildDefinition(t, h, machineID, version))
}

// p618Seed builds a machine (v1) with three states, two transitions, a
// state instruction on "A", a role assignment on "A", and a state-file
// requirement on "A", and returns the machine (family) ID.
func p618Seed(t *testing.T, h *Harness) string {
	t.Helper()
	ctx := context.Background()
	pool := h.App.DB().Raw()
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-hash",
		[]service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"},
			{Key: "B", Kind: "ACTION"},
			{Key: "C", Kind: "TERMINAL"},
		},
		[]service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"},
			{From: "B", Outcome: "DONE", To: "C"},
		},
		"A",
	)
	var aID string
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'A'", &aID); err != nil {
		t.Fatalf("read state A: %v", err)
	}
	instr := dbrepo.NewStateInstructionRepository(pool)
	if _, err := instr.AppendVersion(ctx, uuid.MustParse(aID), "first instruction"); err != nil {
		t.Fatalf("append instruction: %v", err)
	}
	roleID := p607Role(t, h, proj, "P618-ROLE")
	assigns := dbrepo.NewStateRoleAssignmentRepository(pool)
	now := time.Now().UTC()
	if err := assigns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: uuid.MustParse(aID), RoleID: uuid.MustParse(roleID),
		Ordinal: 0, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	files := dbrepo.NewStateFileRequirementRepository(pool)
	if _, err := files.Require(ctx, uuid.MustParse(aID), "REPORT"); err != nil {
		t.Fatalf("require state file: %v", err)
	}
	return mid
}

// T6.21.1 — Same machine (unmodified) produces the same hash on repeated
// calls, over persisted data.
func TestP618SameMachineSameHashIntegration(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	h1 := p618Hash(t, h, mid, 1)
	h2 := p618Hash(t, h, mid, 1)
	if h1 == "" {
		t.Fatal("hash is empty")
	}
	if h1 != h2 {
		t.Fatalf("repeated hashes differ: %s vs %s", h1, h2)
	}
}

// T6.21.2 — A state-instruction edit changes the hash.
func TestP618StateInstructionEditChangesHashIntegration(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	before := p618Hash(t, h, mid, 1)
	ctx := context.Background()
	var aID string
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'A'", &aID); err != nil {
		t.Fatalf("read state A: %v", err)
	}
	instr := dbrepo.NewStateInstructionRepository(h.App.DB().Raw())
	if _, err := instr.AppendVersion(ctx, uuid.MustParse(aID), "edited instruction"); err != nil {
		t.Fatalf("append instruction: %v", err)
	}
	after := p618Hash(t, h, mid, 1)
	if before == after {
		t.Fatalf("hash unchanged after state-instruction edit: %s", before)
	}
}

// T6.21.3 — A transition edit changes the hash. The edit publishes a new
// version (P6-17), so the new version's hash differs from v1's.
func TestP618TransitionEditChangesHashIntegration(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	before := p618Hash(t, h, mid, 1)
	svc := p617Service(h)
	if _, err := svc.Edit(context.Background(), mid, &service.MachineEditInput{
		InitialStateKey: "A",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"},
			{Key: "B", Kind: "ACTION"},
			{Key: "C", Kind: "TERMINAL"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"},
			// Changed: B now fails back to A (was DONE -> C).
			{From: "B", Outcome: "FAIL", To: "A"},
		},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	after := p618Hash(t, h, mid, 2)
	if before == after {
		t.Fatalf("hash unchanged after transition edit: %s", before)
	}
}

// T6.21.4 — A role-assignment change changes the hash.
func TestP618RoleAssignmentChangeChangesHashIntegration(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	before := p618Hash(t, h, mid, 1)
	ctx := context.Background()
	var aID string
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'A'", &aID); err != nil {
		t.Fatalf("read state A: %v", err)
	}
	if _, err := h.DBExec(ctx, "DELETE FROM state_role_assignments WHERE state_id = '"+aID+"'"); err != nil {
		t.Fatalf("delete assignment: %v", err)
	}
	otherRole := p607Role(t, h, "", "P618-ROLE-B")
	assigns := dbrepo.NewStateRoleAssignmentRepository(h.App.DB().Raw())
	now := time.Now().UTC()
	if err := assigns.Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: uuid.MustParse(aID), RoleID: uuid.MustParse(otherRole),
		Ordinal: 0, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	after := p618Hash(t, h, mid, 1)
	if before == after {
		t.Fatalf("hash unchanged after role-assignment change: %s", before)
	}
}

// T6.21.5 — Transition ordering alone (semantic order unchanged) does not
// alter the hash: a second machine with the SAME semantic content (same
// states, instructions, role assignments, state files) whose transitions
// were seeded in the opposite input order hashes identically to the
// first. (A version edit cannot serve here: each version's state rows are
// fresh, and instruction/role attachments follow the v1 state ids, so a
// re-published version legitimately loses them — that is content change,
// not ordering.)
func TestP618SemanticOrderStabilityIntegration(t *testing.T) {
	h := NewHarness(t)
	mid1 := p618Seed(t, h)
	h1 := p618Hash(t, h, mid1, 1)

	// Second machine, same content (same name, no name uniqueness
	// constraint), opposite transition input order.
	ctx := context.Background()
	pool := h.App.DB().Raw()
	var proj string
	if err := h.DBScan(ctx, "SELECT project_id::text FROM state_machines WHERE id = '"+mid1+"'", &proj); err != nil {
		t.Fatalf("read project: %v", err)
	}
	mid2 := p617SeedMachine(t, h, proj, "m-hash",
		[]service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"},
			{Key: "B", Kind: "ACTION"},
			{Key: "C", Kind: "TERMINAL"},
		},
		[]service.MachineTransitionDef{
			{From: "B", Outcome: "DONE", To: "C"},
			{From: "A", Outcome: "GO", To: "B"},
		},
		"A",
	)
	var aID string
	if err := h.DBScan(ctx, "SELECT id::text FROM workflow_states WHERE machine_id = '"+mid2+"' AND key = 'A'", &aID); err != nil {
		t.Fatalf("read state A: %v", err)
	}
	if _, err := dbrepo.NewStateInstructionRepository(pool).AppendVersion(ctx, uuid.MustParse(aID), "first instruction"); err != nil {
		t.Fatalf("append instruction: %v", err)
	}
	// The role was already created for the first machine (same project
	// scope); look it up.
	var roleID string
	if err := h.DBScan(ctx, "SELECT id::text FROM roles WHERE key = 'P618-ROLE'", &roleID); err != nil {
		t.Fatalf("read role id: %v", err)
	}
	now := time.Now().UTC()
	if err := dbrepo.NewStateRoleAssignmentRepository(pool).Create(ctx, &persistence.StateRoleAssignmentRecord{
		ID: uuid.New(), StateID: uuid.MustParse(aID), RoleID: uuid.MustParse(roleID),
		Ordinal: 0, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	if _, err := dbrepo.NewStateFileRequirementRepository(pool).Require(ctx, uuid.MustParse(aID), "REPORT"); err != nil {
		t.Fatalf("require state file: %v", err)
	}
	h2 := p618Hash(t, h, mid2, 1)
	if h1 != h2 {
		t.Fatalf("hash changed under semantic-order permutation: %s vs %s", h1, h2)
	}
}

// Parity (sequential fallback): the same read through the service with a
// nil pool (sequential direct calls) and with the pool (one transaction)
// must produce the identical hash — the read is pure, so both paths agree.
func TestP618NilPoolParity(t *testing.T) {
	h := NewHarness(t)
	mid := p618Seed(t, h)
	pool := h.App.DB().Raw()
	mk := func() *service.MachineConfigHashService {
		return service.NewMachineConfigHashService(
			dbrepo.NewStateMachineRepository(pool),
			dbrepo.NewWorkflowStateRepository(pool),
			dbrepo.NewStateTransitionRepository(pool),
			dbrepo.NewStateInstructionRepository(pool),
			dbrepo.NewStateRoleAssignmentRepository(pool),
			dbrepo.NewStateFileRequirementRepository(pool),
			dbrepo.NewRoleRepository(pool),
		)
	}
	seq := mk() // nil pool
	tx := mk().WithTxPool(pool)
	gh, err := seq.Hash(context.Background(), mid, 1)
	if err != nil {
		t.Fatalf("sequential hash: %v", err)
	}
	ht, err := tx.Hash(context.Background(), mid, 1)
	if err != nil {
		t.Fatalf("tx hash: %v", err)
	}
	if gh != ht {
		t.Fatalf("sequential vs transactional hash differ: %s vs %s", gh, ht)
	}
	ref := p618Hash(t, h, mid, 1)
	if gh != ref {
		t.Fatalf("service hash differs from repository-built reference: %s vs %s", gh, ref)
	}
}
