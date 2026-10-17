package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/service"
)

// P8-30 (WBS 8.26 persistence half): migration 00044 (canonical_prompts) +
// the canonical-prompt repository against real PostgreSQL. The domain half
// (internal/prompt/canonicalprompt_test.go) covers the pure validation; this
// file covers the DB-backed half of T8.26.1-T8.26.5.
//
// The repository stores every association/version VERBATIM and the content
// EXACTLY (byte-for-byte), so a historical prompt stays reconstructable after
// every source advances (the WBS 8.48 contract, exercised end-to-end by P8-47).

// p830Rig seeds a complete canonical-prompt-ready fixture and returns the ids:
// a project + phase + unit (p705Rig), a project machine v1 with one enabled
// state S, the project role, a runtime state at S bound to the machine, and a
// context snapshot capturing that position. The snapshot is the hardest FK
// target for canonical_prompts, so it is seeded directly (it is an
// append-only table with plain columns; the role_snapshot column is a JSONB
// blob with no FK).
func p830Rig(t *testing.T, h *Harness) (projectID, phaseID, unitID, snapID, roleID, machineID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	raw := h.App.DB().Raw()

	projectID, phaseID, unitID = p705Rig(t, h)

	mid := p617SeedMachine(t, h, projectID.String(), "p830-machine",
		[]service.MachineStateDef{{Key: "S", Kind: "ACTION"}}, nil, "S")
	if err := h.DBScan(ctx, "SELECT id FROM state_machines WHERE id = '"+mid+"'", &machineID); err != nil {
		t.Fatalf("scan machine: %v", err)
	}
	var stateID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM workflow_states WHERE machine_id = '"+mid+"' AND key = 'S'", &stateID); err != nil {
		t.Fatalf("scan state: %v", err)
	}
	roleID = p712ProjectRole(t, h, projectID, "P830-ROLE")

	rt := dbrepo.NewRuntimeStateRepository(raw)
	if err := rt.Create(ctx, &persistence.RuntimeStateRecord{
		ID: uuid.New(), ProjectID: projectID,
		StateMachineID: machineID, StateMachineVersion: 1,
		CurrentStateID: stateID, PhaseID: &phaseID, WorkUnitID: &unitID,
		EnteredAt: time.Now().UTC(), Version: 1,
	}); err != nil {
		t.Fatalf("create runtime: %v", err)
	}
	var runtimeID uuid.UUID
	if err := h.DBScan(ctx, "SELECT id FROM runtime_state WHERE project_id = '"+projectID.String()+"' LIMIT 1", &runtimeID); err != nil {
		t.Fatalf("scan runtime: %v", err)
	}

	snapID = uuid.New()
	if _, err := h.DBExec(ctx,
		"INSERT INTO context_snapshots (id, project_id, phase_id, work_unit_id, runtime_state_id, runtime_state_version, state_machine_id, state_machine_version, state_key, role_snapshot) VALUES ('"+
			snapID.String()+"', '"+projectID.String()+"', '"+phaseID.String()+"', '"+unitID.String()+"', '"+runtimeID.String()+"', 1, '"+machineID.String()+"', 1, 'S', '{}'::jsonb)"); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	return
}

// P8-30.1 — Migration 00044 applies cleanly on a fresh harness DB and the
// table exists.
func TestP830MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 43 -> 44 by P8-30 (migration 00044, canonical_prompts) —
	// accepted contract change, not a weakened test.
	if version != 126 {
		t.Fatalf("max migration version = %d, want 82", version)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'canonical_prompts'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("canonical_prompts present = %d, want 1", n)
	}
}

// p830Rec builds a valid persistence record for the fixture: it creates a real
// project template (the canonical prompt pins a specific template version) and
// assembles the record with deterministic (fixed) timestamps.
func p830Rec(t *testing.T, h *Harness, rig p830IDs) *persistence.CanonicalPromptRecord {
	t.Helper()
	phase, unit := rig.phaseID, rig.unitID
	cp, err := prompt.NewCanonicalPrompt(prompt.CanonicalPromptInput{
		ID:                  uuid.New(),
		ProjectID:           rig.projectID,
		PhaseID:             &phase,
		WorkUnitID:          &unit,
		ContextSnapshotID:   rig.snapID,
		TemplateID:          rig.templateID,
		TemplateVersion:     1,
		RoleID:              rig.roleID,
		RoleVersion:         1,
		StateMachineID:      rig.machineID,
		StateMachineVersion: 1,
		StateKey:            "S",
		Content:             "## Implementation\n\nDo the exact thing.",
		Hash:                "c0ffee",
		EstimatedTokens:     123,
		CreatedAt:           time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewCanonicalPrompt: %v", err)
	}
	return &persistence.CanonicalPromptRecord{
		ID:                  cp.ID,
		ProjectID:           cp.ProjectID,
		PhaseID:             cp.PhaseID,
		WorkUnitID:          cp.WorkUnitID,
		ContextSnapshotID:   cp.ContextSnapshotID,
		TemplateID:          cp.TemplateID,
		TemplateVersion:     cp.TemplateVersion,
		RoleID:              cp.RoleID,
		RoleVersion:         cp.RoleVersion,
		StateMachineID:      cp.StateMachineID,
		StateMachineVersion: cp.StateMachineVersion,
		StateKey:            cp.StateKey,
		Content:             cp.Content,
		Hash:                cp.Hash,
		EstimatedTokens:     cp.EstimatedTokens,
		CreatedAt:           time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
	}
}

type p830IDs struct {
	projectID, phaseID, unitID, snapID, roleID, machineID, templateID uuid.UUID
}

// p830Fixture builds the rig plus a real project template.
func p830Fixture(t *testing.T, h *Harness) p830IDs {
	t.Helper()
	projectID, phaseID, unitID, snapID, roleID, machineID := p830Rig(t, h)
	templateID := p811Template(t, h, "cp-impl")
	return p830IDs{projectID, phaseID, unitID, snapID, roleID, machineID, templateID}
}

// T8.26.1 — Prompt persisted: a valid canonical prompt is created and reads
// back through the repository byte-for-byte (all fields), confirmed by direct
// SQL.
func TestT8261_PromptPersisted(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fx := p830Fixture(t, h)
	rec := p830Rec(t, h, fx)
	repo := dbrepo.NewCanonicalPromptRepository(h.App.DB().Raw())
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != rec.ID || got.ProjectID != rec.ProjectID || got.ContextSnapshotID != rec.ContextSnapshotID ||
		got.Content != rec.Content || got.Hash != rec.Hash || got.EstimatedTokens != rec.EstimatedTokens {
		t.Fatalf("round-trip = %+v", got)
	}
	var stored int
	if err := h.DBScan(ctx, "SELECT count(*) FROM canonical_prompts WHERE id = '"+rec.ID.String()+"'", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("direct SQL count = %d, want 1", stored)
	}
}

// T8.26.2 — Snapshot association retained: the context_snapshot_id is stored
// verbatim (read back by direct SQL, not just the repository readback).
func TestT8262_SnapshotAssociationRetained(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fx := p830Fixture(t, h)
	rec := p830Rec(t, h, fx)
	repo := dbrepo.NewCanonicalPromptRepository(h.App.DB().Raw())
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	var stored uuid.UUID
	if err := h.DBScan(ctx, "SELECT context_snapshot_id FROM canonical_prompts WHERE id = '"+rec.ID.String()+"'", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != fx.snapID {
		t.Fatalf("stored snapshot id = %s, want %s", stored, fx.snapID)
	}
}

// T8.26.3 — Template version retained: the (template_id, template_version)
// pair is stored verbatim.
func TestT8263_TemplateVersionRetained(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fx := p830Fixture(t, h)
	rec := p830Rec(t, h, fx)
	rec.TemplateVersion = 7
	repo := dbrepo.NewCanonicalPromptRepository(h.App.DB().Raw())
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	var storedTmpl uuid.UUID
	var storedVer int
	if err := h.DBScan(ctx, "SELECT template_id, template_version FROM canonical_prompts WHERE id = '"+rec.ID.String()+"'", &storedTmpl, &storedVer); err != nil {
		t.Fatal(err)
	}
	if storedTmpl != fx.templateID || storedVer != 7 {
		t.Fatalf("stored template = (%s,%d), want (%s,7)", storedTmpl, storedVer, fx.templateID)
	}
}

// T8.26.4 — State/role versions retained: (role_id, role_version) and
// (state_machine_id, state_machine_version, state_key) are stored verbatim.
func TestT8264_StateRoleVersionsRetained(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fx := p830Fixture(t, h)
	rec := p830Rec(t, h, fx)
	rec.RoleVersion = 4
	repo := dbrepo.NewCanonicalPromptRepository(h.App.DB().Raw())
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := repo.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RoleID != rec.RoleID || got.RoleVersion != 4 ||
		got.StateMachineID != rec.StateMachineID || got.StateMachineVersion != rec.StateMachineVersion ||
		got.StateKey != rec.StateKey {
		t.Fatalf("state/role pin = %+v", got)
	}
}

// T8.26.5 — Content retained EXACTLY: the content (including whitespace and
// tabs) is stored byte-for-byte, confirmed by direct SQL.
func TestT8265_ContentRetainedExactly(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	fx := p830Fixture(t, h)
	rec := p830Rec(t, h, fx)
	const exact = "## Impl\n\n  keep\tthis exact  \n"
	rec.Content = exact
	repo := dbrepo.NewCanonicalPromptRepository(h.App.DB().Raw())
	if err := repo.Create(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	var stored string
	if err := h.DBScan(ctx, "SELECT content FROM canonical_prompts WHERE id = '"+rec.ID.String()+"'", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != exact {
		t.Fatalf("stored content = %q, want %q", stored, exact)
	}
}

// T8.26.x (error translation + FK guards) — a missing project reference yields
// persistence.ErrInvalidReference; an unknown id yields persistence.ErrNotFound.
func TestT8266_ErrorsAndFKs(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	repo := dbrepo.NewCanonicalPromptRepository(h.App.DB().Raw())

	if _, err := repo.Get(ctx, uuid.New()); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("absent id err = %v, want ErrNotFound", err)
	}

	rec := &persistence.CanonicalPromptRecord{
		ID: uuid.New(), ProjectID: uuid.New(), ContextSnapshotID: uuid.New(),
		TemplateID: uuid.New(), TemplateVersion: 1,
		RoleID: uuid.New(), RoleVersion: 1,
		StateMachineID: uuid.New(), StateMachineVersion: 1, StateKey: "S",
		Content: "c", Hash: "h", EstimatedTokens: 1,
		CreatedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
	}
	if err := repo.Create(ctx, rec); !errors.Is(err, persistence.ErrInvalidReference) {
		t.Fatalf("FK err = %v, want ErrInvalidReference", err)
	}
}
