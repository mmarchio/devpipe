package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/role"
	"overseer/internal/service"
)

// P5-33 (WBS 5.35) Phase 5 end-to-end acceptance: create a project,
// initialize its default role set, edit the PROJECT-scope ENGINEER prompt
// (v1 -> v2), assign its three instructions (ENGINEER.md, TDD.md,
// CODING_STANDARDS.md), set allowed states [implementation continuation] and
// state files [ACTIVE_TASK PROJECT_STATE ARCHITECTURE] (three entries per
// 5.35), capture Snapshot A, advance TDD.md to v2, capture Snapshot B, verify
// A pins TDD v1 / B pins TDD v2 with different configuration hashes, then
// restart (two harnesses on the same persisted database) and verify both
// historical configurations remain reconstructable. Global ENGINEER must stay
// untouched throughout.

const (
	p533PromptV2 = "engineer system prompt revision 2"

	p533EngV1 = "engineer instruction v1"
	p533TDDV1 = "tdd instruction v1"
	p533TDDV2 = "tdd instruction v2"
	p533CSSV1 = "coding standards instruction v1"
)

// p533Fixture builds the 5.35 fixture through service ops on the given
// harness and returns every id/state the test pins.
type p533Fixture struct {
	projID       string
	engID        string
	engInstrID   string
	tddInstrID   string
	cssInstrID   string
	globalEngID  string
	promptV1Hash string
}

func p533BuildFixture(t *testing.T, h *Harness) *p533Fixture {
	t.Helper()
	svc := snapshotSvc(t, h)
	ctx := ctx0()

	f := &p533Fixture{}

	// 1. Create a project (service path, as in the p401 seed pattern).
	f.projID = createPhaseProject(t, h)

	// 2. Initialize the project's default role set (project-scoped copies with
	// independent IDs; the global defaults remain v1).
	if _, err := svc.InitializeProjectRoles(ctx, f.projID); err != nil {
		t.Fatalf("InitializeProjectRoles: %v", err)
	}
	roles, err := svc.ListRoles(ctx, &f.projID)
	if err != nil {
		t.Fatalf("ListRoles(project): %v", err)
	}
	for _, r := range roles {
		if r.Key == "ENGINEER" {
			f.engID = r.ID.String()
		}
	}
	if f.engID == "" {
		t.Fatal("project ENGINEER role not created by initialization")
	}
	globals, err := svc.ListRoles(ctx, nil)
	if err != nil {
		t.Fatalf("ListRoles(global): %v", err)
	}
	for _, r := range globals {
		if r.Key == "ENGINEER" {
			f.globalEngID = r.ID.String()
		}
	}
	if f.globalEngID == "" {
		t.Fatal("global ENGINEER role not seeded")
	}
	if f.engID == f.globalEngID {
		t.Fatal("project ENGINEER shares the global ID — copies are not independent")
	}

	// 3. Pin the copied v1 prompt content (the global's seeded v1) for the
	// isolation assertions; its hash is recomputed independently.
	prompts := dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
	v1, err := prompts.GetVersion(ctx, f.engID, 1)
	if err != nil {
		t.Fatalf("GetVersion(project prompt v1): %v", err)
	}
	f.promptV1Hash = role.HashSystemPromptContent(v1.Content)

	// 4. Edit the PROJECT-scope ENGINEER prompt once (v1 -> v2).
	if _, err := svc.UpdateSystemPrompt(ctx, f.engID, p533PromptV2, 1); err != nil {
		t.Fatalf("UpdateSystemPrompt v2: %v", err)
	}

	// 5. Create the three project-scoped instruction documents at v1 and
	// assign them in the 5.35 order.
	f.engInstrID = p533Doc(t, svc, "ENGINEER", "ENGINEER.md", f.projID, p533EngV1)
	f.tddInstrID = p533Doc(t, svc, "TDD", "TDD.md", f.projID, p533TDDV1)
	f.cssInstrID = p533Doc(t, svc, "CODING_STANDARDS", "CODING_STANDARDS.md", f.projID, p533CSSV1)
	for _, id := range []string{f.engInstrID, f.tddInstrID, f.cssInstrID} {
		if err := svc.AssignInstruction(ctx, f.engID, id); err != nil {
			t.Fatalf("AssignInstruction: %v", err)
		}
	}

	// 6. Allowed states + state files (5.35: three state files).
	if _, err := svc.SetAllowedStates(ctx, f.engID, []string{"implementation", "continuation"}); err != nil {
		t.Fatalf("SetAllowedStates: %v", err)
	}
	files, err := svc.SetStateFiles(ctx, f.engID, []string{"ACTIVE_TASK", "PROJECT_STATE", "ARCHITECTURE"})
	if err != nil {
		t.Fatalf("SetStateFiles: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("stored state files = %v, want 3 entries", files)
	}
	return f
}

// p533Doc creates a PROJECT-scoped instruction document with the given
// content as its v1 (CreateInstruction writes the first version row — a
// fresh document, unlike the boot-seeded global fixture).
func p533Doc(t *testing.T, svc *service.RoleService, key, name, projID, content string) string {
	t.Helper()
	view, err := svc.CreateInstruction(ctx0(), &projID, key, name, "", content)
	if err != nil {
		t.Fatalf("CreateInstruction(%s): %v", key, err)
	}
	if view.Version != 1 {
		t.Fatalf("instruction %s version = %d, want 1", key, view.Version)
	}
	return view.ID
}

// p533Snap is the 5.35 per-role read: the full RoleSnapshot plus the
// configuration hash and serialization (for the restart comparison).
type p533Snap struct {
	*role.RoleSnapshot
	Hash   string
	Serial string
}

func p533Capture(t *testing.T, h *Harness, id string) *p533Snap {
	t.Helper()
	svc := snapshotSvc(t, h)
	snap, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatalf("SnapshotRole: %v", err)
	}
	return &p533Snap{
		RoleSnapshot: snap,
		Hash:         snap.ConfigurationHash(),
		Serial:       snap.Serialize(),
	}
}

// p533CompareSnap asserts every snapshot field of a matches b (field-for-
// field, per the 5.35 restart criterion — not row counts or spot checks).
func p533CompareSnap(t *testing.T, label string, a, b *p533Snap) {
	t.Helper()
	if a.RoleKey != b.RoleKey || a.RoleVersion != b.RoleVersion {
		t.Fatalf("%s: identity = %s@%d, want %s@%d", label, a.RoleKey, a.RoleVersion, b.RoleKey, b.RoleVersion)
	}
	if a.SystemPrompt.Version != b.SystemPrompt.Version || a.SystemPrompt.Content != b.SystemPrompt.Content || a.SystemPrompt.ContentHash != b.SystemPrompt.ContentHash {
		t.Fatalf("%s: system prompt = v%d/%q, want v%d/%q", label, a.SystemPrompt.Version, a.SystemPrompt.Content, b.SystemPrompt.Version, b.SystemPrompt.Content)
	}
	if len(a.Instructions) != len(b.Instructions) {
		t.Fatalf("%s: instruction count = %d, want %d", label, len(a.Instructions), len(b.Instructions))
	}
	for i := range a.Instructions {
		x, y := a.Instructions[i], b.Instructions[i]
		if x.Key != y.Key || x.Name != y.Name || x.Version != y.Version || x.Content != y.Content || x.ContentHash != y.ContentHash || x.Classification != y.Classification {
			t.Fatalf("%s: instruction[%d] = %+v, want %+v", label, i, x, y)
		}
	}
	if !setEq(a.AllowedStates, b.AllowedStates) {
		t.Fatalf("%s: allowed states = %v, want %v", label, a.AllowedStates, b.AllowedStates)
	}
	if !setEq(a.StateFiles, b.StateFiles) {
		t.Fatalf("%s: state files = %v, want %v", label, a.StateFiles, b.StateFiles)
	}
	if a.Hash != b.Hash {
		t.Fatalf("%s: configuration hash = %s, want %s", label, a.Hash, b.Hash)
	}
	if a.Serial != b.Serial {
		t.Fatalf("%s: serialized snapshots differ:\nA:\n%s\nB:\n%s", label, a.Serial, b.Serial)
	}
}

// p533TDDVersion returns the snapshot's TDD instruction version.
func p533TDDVersion(t *testing.T, s *p533Snap) int {
	t.Helper()
	for _, ins := range s.Instructions {
		if ins.Key == "TDD" {
			return ins.Version
		}
	}
	t.Fatal("no TDD instruction in snapshot")
	return 0
}

// p533Reconstruct builds a RoleSnapshot from version history at the pinned
// (historical) component versions — the 5.35 "both historical configurations
// remain reconstructable" check. Current values are read from the role row
// (key, version) and the assignment/state rows; the prompt and each
// instruction are read at the requested version from the append-only
// history tables.
func p533Reconstruct(t *testing.T, h *Harness, f *p533Fixture, promptVer, tddVer int) *p533Snap {
	t.Helper()
	ctx := ctx0()
	raw := h.App.DB().Raw()
	roles := dbrepo.NewRoleRepository(raw)
	rec, err := roles.Get(ctx, f.engID)
	if err != nil {
		t.Fatalf("role Get: %v", err)
	}
	prompts := dbrepo.NewSystemPromptRepository(raw)
	pv, err := prompts.GetVersion(ctx, f.engID, promptVer)
	if err != nil {
		t.Fatalf("prompt GetVersion(%d): %v", promptVer, err)
	}
	iv := dbrepo.NewInstructionVersionRepository(raw)
	tddv, err := iv.GetVersion(ctx, f.tddInstrID, tddVer)
	if err != nil {
		t.Fatalf("tdd GetVersion(%d): %v", tddVer, err)
	}
	assigns, err := dbrepo.NewRoleInstructionAssignmentRepository(raw).ListByRole(ctx, rec.ID)
	if err != nil {
		t.Fatalf("assignments ListByRole: %v", err)
	}
	instrRepo := dbrepo.NewInstructionRepository(raw)
	var insSnap []role.InstructionSnapshot
	for _, a := range assigns {
		inst, err := instrRepo.Get(ctx, a.InstructionID.String())
		if err != nil {
			t.Fatalf("instruction Get(%s): %v", a.InstructionID.String(), err)
		}
		ver, content := tddVer, tddv.Content
		if a.InstructionID.String() != f.tddInstrID {
			cur, err := iv.GetCurrentVersion(ctx, a.InstructionID.String())
			if err != nil {
				t.Fatalf("GetCurrentVersion: %v", err)
			}
			vr, err := iv.GetVersion(ctx, a.InstructionID.String(), cur)
			if err != nil {
				t.Fatalf("GetVersion: %v", err)
			}
			ver, content = cur, vr.Content
		}
		insSnap = append(insSnap, role.InstructionSnapshot{
			Key:            inst.Key,
			Name:           inst.Name,
			Version:        ver,
			Content:        content,
			ContentHash:    role.HashSystemPromptContent(content),
			Classification: role.ClassificationRoleInstruction,
		})
	}
	states, err := dbrepo.NewRoleAllowedStateRepository(raw).ListByRole(ctx, rec.ID)
	if err != nil {
		t.Fatalf("states ListByRole: %v", err)
	}
	var stateKeys []string
	for _, st := range states {
		stateKeys = append(stateKeys, st.StateKey)
	}
	fileRows, err := dbrepo.NewRoleStateFileRepository(raw).ListByRole(ctx, rec.ID)
	if err != nil {
		t.Fatalf("state files ListByRole: %v", err)
	}
	var fileKeys []string
	for _, fl := range fileRows {
		fileKeys = append(fileKeys, fl.StateFileKey)
	}
	snap := &role.RoleSnapshot{
		RoleID:      rec.ID,
		RoleKey:     rec.Key,
		RoleVersion: rec.Version,
		SystemPrompt: role.PromptSnapshot{
			Version:        pv.Version,
			Content:        pv.Content,
			ContentHash:    role.HashSystemPromptContent(pv.Content),
			Classification: role.ClassificationSystem,
		},
		Instructions:  insSnap,
		AllowedStates: stateKeys,
		StateFiles:    fileKeys,
	}
	return &p533Snap{RoleSnapshot: snap, Hash: snap.ConfigurationHash(), Serial: snap.Serialize()}
}

// p533GlobalIsolation verifies the global ENGINEER role was untouched by the
// whole 5.35 flow: prompt still exactly v1 and no instructions assigned.
func p533GlobalIsolation(t *testing.T, h *Harness, f *p533Fixture) {
	t.Helper()
	ctx := ctx0()
	raw := h.App.DB().Raw()
	prompts := dbrepo.NewSystemPromptRepository(raw)
	cur, err := prompts.GetCurrentVersion(ctx, f.globalEngID)
	if err != nil {
		t.Fatalf("global prompt GetCurrentVersion: %v", err)
	}
	if cur != 1 {
		t.Fatalf("global ENGINEER prompt version = %d, want 1 (untouched)", cur)
	}
	v1, err := prompts.GetVersion(ctx, f.globalEngID, 1)
	if err != nil {
		t.Fatalf("global prompt GetVersion(1): %v", err)
	}
	if role.HashSystemPromptContent(v1.Content) != f.promptV1Hash {
		t.Fatalf("global ENGINEER v1 prompt content changed (project isolation violated)")
	}
	assigns, err := dbrepo.NewRoleInstructionAssignmentRepository(raw).ListByRole(ctx, mustParse(t, f.globalEngID))
	if err != nil {
		t.Fatalf("global assignments ListByRole: %v", err)
	}
	if len(assigns) != 0 {
		t.Fatalf("global ENGINEER has %d instruction assignments, want 0 (untouched)", len(assigns))
	}
}

func mustParse(t *testing.T, s string) uuid.UUID {
	t.Helper()
	return parseUUID(t, s)
}

// T5.35 — Phase 5 end-to-end acceptance.
func TestP533Phase5E2EAcceptance(t *testing.T) {
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	// NewHarnessOn registers no cleanup; close h2 (LIFO: runs before the
	// dropDB) and drop the caller-owned DB at the end.
	t.Cleanup(func() { dropDB(t, base, dbName) })

	// Boot 1: build the full 5.35 fixture and capture Snapshots A and B.
	h1 := NewHarnessOn(t, base, dbName)
	if !h1.App.IsReady() {
		t.Fatal("first boot not ready")
	}
	f := p533BuildFixture(t, h1)

	// Snapshot A: pre-TDD-edit configuration (prompt v2 + TDD v1).
	a := p533Capture(t, h1, f.engID)
	if a.SystemPrompt.Version != 2 {
		t.Fatalf("snapshot A system prompt version = %d, want 2", a.SystemPrompt.Version)
	}
	if a.SystemPrompt.ContentHash != role.HashSystemPromptContent(p533PromptV2) {
		t.Fatalf("snapshot A system prompt hash = %q, want the independently computed v2 hash", a.SystemPrompt.ContentHash)
	}
	if got := p533TDDVersion(t, a); got != 1 {
		t.Fatalf("snapshot A TDD version = %d, want 1 (old)", got)
	}
	if a.Instructions[0].Key != "ENGINEER" || a.Instructions[1].Key != "TDD" || a.Instructions[2].Key != "CODING_STANDARDS" {
		t.Fatalf("snapshot A instruction order = %v, want [ENGINEER TDD CODING_STANDARDS]", []string{a.Instructions[0].Key, a.Instructions[1].Key, a.Instructions[2].Key})
	}
	if !setEq(a.AllowedStates, []string{"implementation", "continuation"}) {
		t.Fatalf("snapshot A allowed states = %v, want [implementation continuation]", a.AllowedStates)
	}
	if len(a.StateFiles) != 3 || a.StateFiles[0] != "ACTIVE_TASK" || a.StateFiles[1] != "PROJECT_STATE" || a.StateFiles[2] != "ARCHITECTURE" {
		t.Fatalf("snapshot A state files = %v, want [ACTIVE_TASK PROJECT_STATE ARCHITECTURE]", a.StateFiles)
	}

	// Modify TDD.md once (v1 -> v2) and capture Snapshot B.
	if _, err := snapshotSvc(t, h1).UpdateInstruction(ctx0(), f.tddInstrID, p533TDDV2, 1); err != nil {
		t.Fatalf("UpdateInstruction TDD v2: %v", err)
	}
	b := p533Capture(t, h1, f.engID)
	if got := p533TDDVersion(t, b); got != 2 {
		t.Fatalf("snapshot B TDD version = %d, want 2 (new)", got)
	}
	if b.Instructions[1].Content != p533TDDV2 {
		t.Fatalf("snapshot B TDD content = %q, want pinned v2 content", b.Instructions[1].Content)
	}
	if b.Instructions[1].ContentHash != role.HashSystemPromptContent(p533TDDV2) {
		t.Fatalf("snapshot B TDD hash = %q, want the independently computed v2 hash", b.Instructions[1].ContentHash)
	}
	if a.Serial == b.Serial {
		t.Fatal("snapshot A == snapshot B (5.35 requires A != B)")
	}
	if a.Hash == b.Hash {
		t.Fatal("configuration hash A == configuration hash B (5.35 requires them to differ)")
	}

	// Global ENGINEER untouched after the full pre-restart flow.
	p533GlobalIsolation(t, h1, f)

	h1.Close()

	// Boot 2 (restart) against the SAME persisted database.
	h2 := NewHarnessOn(t, base, dbName)
	if !h2.App.IsReady() {
		t.Fatal("restart: app not ready")
	}
	t.Cleanup(h2.Close)

	// Current configuration (TDD v2) reconstructs identically after the
	// restart — field-for-field, hash included.
	cur := p533Capture(t, h2, f.engID)
	p533CompareSnap(t, "current", cur, b)
	if p533TDDVersion(t, cur) != 2 {
		t.Fatalf("post-restart TDD version = %d, want 2", p533TDDVersion(t, cur))
	}

	// Pre-edit configuration (prompt v2 + TDD v1) remains reconstructable
	// from version history and hashes identically to the pre-restart A.
	pred := p533Reconstruct(t, h2, f, 2, 1)
	if p533TDDVersion(t, pred) != 1 {
		t.Fatalf("reconstructed pre-edit TDD version = %d, want 1", p533TDDVersion(t, pred))
	}
	if pred.Hash != a.Hash {
		t.Fatalf("pre-edit reconstructed configuration hash = %s, want the pre-restart A hash %s", pred.Hash, a.Hash)
	}
	if pred.Serial != a.Serial {
		t.Fatalf("pre-edit reconstruction differs from snapshot A:\nA:\n%s\nR:\n%s", a.Serial, pred.Serial)
	}
	if pred.Hash == b.Hash {
		t.Fatal("pre-edit reconstruction hashes like the current configuration — not distinct")
	}

	// Global ENGINEER untouched after the restart as well.
	p533GlobalIsolation(t, h2, f)
}
