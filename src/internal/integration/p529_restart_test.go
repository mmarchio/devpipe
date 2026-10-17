package integration

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// P5-29 restart persistence (WBS 5.31): a role built to the 5.31 fixture
// state (ENGINEER, system prompt v3, instructions ENGINEER.md v4 + TDD.md v2
// in that order, allowed states implementation + continuation, state files
// ACTIVE_TASK + PROJECT_STATE) must be reconstructed after a service restart
// with role version, system-prompt version, instruction versions, instruction
// order, allowed states, state-file requirements, and configuration hash all
// unchanged. The test boots TWO harness instances on the SAME persisted
// database (the restart/reconstruction pattern) and compares field-level
// snapshots before and after, not row counts or spot checks.

// p529Snapshot captures every answer the acceptance criteria must verify,
// read through the live service (SnapshotRole + ConfigurationHash).
type p529Snapshot struct {
	RoleVersion int
	PromptVer   int
	InsKeys     []string
	InsVersions []int
	States      []string
	Files       []string
	Hash        string
	Serial      string
}

func p529Capture(t *testing.T, h *Harness, id string) *p529Snapshot {
	t.Helper()
	svc := snapshotSvc(t, h)
	snap, err := svc.SnapshotRole(ctx0(), id)
	if err != nil {
		t.Fatal(err)
	}
	out := &p529Snapshot{
		RoleVersion: snap.RoleVersion,
		PromptVer:   snap.SystemPrompt.Version,
		Hash:        snap.ConfigurationHash(),
		Serial:      snap.Serialize(),
	}
	for _, ins := range snap.Instructions {
		out.InsKeys = append(out.InsKeys, ins.Key)
		out.InsVersions = append(out.InsVersions, ins.Version)
	}
	out.States = append([]string(nil), snap.AllowedStates...)
	sort.Strings(out.States)
	out.Files = append([]string(nil), snap.StateFiles...)
	sort.Strings(out.Files)
	return out
}

// T5.31 — Full role configuration survives a service stop/start cycle.
func TestP529RolePersistenceRestart(t *testing.T) {
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
	t.Cleanup(func() { dropDB(t, base, dbName) })

	// First boot (migrates + seeds defaults), build the 5.31 fixture role.
	h1 := NewHarnessOn(t, base, dbName)
	if !h1.App.IsReady() {
		t.Fatal("first boot not ready")
	}

	svc := snapshotSvc(t, h1)
	// The 5.31 fixture role is the boot-seeded global ENGINEER (v1 prompt
	// already seeded by SeedDefaultRoles — a CreateRole with key "engineer"
	// would collide on the normalized scoped key).
	roles, err := svc.ListRoles(ctx0(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var rec *persistence.RoleRecord
	for _, r := range roles {
		if r.Key == "ENGINEER" {
			rec = r
			break
		}
	}
	if rec == nil {
		t.Fatal("global ENGINEER default role not seeded")
	}
	// Prompt v2, v3 (fixture state: System Prompt v3).
	prompts := dbrepoPromptRepo(h1)
	if _, err := prompts.AppendVersion(ctx0(), rec.ID.String(), "engineer prompt v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := prompts.AppendVersion(ctx0(), rec.ID.String(), "engineer prompt v3"); err != nil {
		t.Fatal(err)
	}
	// Instructions: ENGINEER.md to v4, TDD.md to v2, assigned in fixture order.
	iEng := seedInstruction(t, h1, "engineer.md", "ENGINEER.md", "engineer.md v1")
	iTdd := seedInstruction(t, h1, "tdd.md", "TDD.md", "tdd.md v1")
	iv := dbrepoInstructionVersionRepo(h1)
	// seedInstruction stores no version row — each append advances one
	// version: 4 appends reach v4, 2 appends reach v2 (5.31 fixture state).
	for i := 0; i < 4; i++ {
		if _, err := iv.AppendVersion(ctx0(), iEng, fmt.Sprintf("engineer.md v%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := iv.AppendVersion(ctx0(), iTdd, fmt.Sprintf("tdd.md v%d", i+1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AssignInstruction(ctx0(), rec.ID.String(), iEng); err != nil {
		t.Fatal(err)
	}
	if err := svc.AssignInstruction(ctx0(), rec.ID.String(), iTdd); err != nil {
		t.Fatal(err)
	}
	// Allowed states + state files (fixture values).
	if _, err := svc.SetAllowedStates(ctx0(), rec.ID.String(), []string{"implementation", "continuation"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetStateFiles(ctx0(), rec.ID.String(), []string{"ACTIVE_TASK", "PROJECT_STATE"}); err != nil {
		t.Fatal(err)
	}

	before := p529Capture(t, h1, rec.ID.String())
	// Pin the fixture values so a drift from the 5.31 spec fails explicitly.
	if before.PromptVer != 3 {
		t.Fatalf("pre-restart prompt version = %d, want 3", before.PromptVer)
	}
	if len(before.InsVersions) != 2 || before.InsVersions[0] != 4 || before.InsVersions[1] != 2 {
		t.Fatalf("pre-restart instruction versions = %v, want [4 2]", before.InsVersions)
	}
	h1.Close()

	// Second boot against the SAME persisted database.
	h2 := NewHarnessOn(t, base, dbName)
	if !h2.App.IsReady() {
		t.Fatal("restart: app not ready")
	}
	// NewHarnessOn registers no cleanup; close h2 so its pool releases the
	// DB connections before the t.Cleanup dropDB runs (registered first, so
	// it runs last under LIFO).
	t.Cleanup(h2.Close)
	after := p529Capture(t, h2, rec.ID.String())

	// Field-by-field comparison.
	if after.RoleVersion != before.RoleVersion {
		t.Fatalf("role version = %d, want %d", after.RoleVersion, before.RoleVersion)
	}
	if after.PromptVer != before.PromptVer {
		t.Fatalf("system prompt version = %d, want %d", after.PromptVer, before.PromptVer)
	}
	if len(after.InsKeys) != len(before.InsKeys) {
		t.Fatalf("instruction count = %d, want %d", len(after.InsKeys), len(before.InsKeys))
	}
	for i := range before.InsKeys {
		if after.InsKeys[i] != before.InsKeys[i] {
			t.Fatalf("instruction[%d] key = %q, want %q (order changed)", i, after.InsKeys[i], before.InsKeys[i])
		}
		if after.InsVersions[i] != before.InsVersions[i] {
			t.Fatalf("instruction[%d] version = %d, want %d", i, after.InsVersions[i], before.InsVersions[i])
		}
	}
	if len(after.States) != len(before.States) {
		t.Fatalf("allowed states = %v, want %v", after.States, before.States)
	}
	for i := range before.States {
		if after.States[i] != before.States[i] {
			t.Fatalf("allowed states = %v, want %v", after.States, before.States)
		}
	}
	if len(after.Files) != len(before.Files) {
		t.Fatalf("state files = %v, want %v", after.Files, before.Files)
	}
	for i := range before.Files {
		if after.Files[i] != before.Files[i] {
			t.Fatalf("state files = %v, want %v", after.Files, before.Files)
		}
	}
	if after.Hash == "" || after.Hash != before.Hash {
		t.Fatalf("configuration hash = %q, want %q (pre-restart)", after.Hash, before.Hash)
	}
	if after.Serial != before.Serial {
		t.Fatalf("serialized snapshot changed across restart:\nA:\n%s\nB:\n%s", before.Serial, after.Serial)
	}
}

// dbrepoPromptRepo / dbrepoInstructionVersionRepo wire the version-append
// repositories to the live pool (seed helpers above only create v1 rows).
func dbrepoPromptRepo(h *Harness) *dbrepo.SystemPromptRepository {
	return dbrepo.NewSystemPromptRepository(h.App.DB().Raw())
}

func dbrepoInstructionVersionRepo(h *Harness) *dbrepo.InstructionVersionRepository {
	return dbrepo.NewInstructionVersionRepository(h.App.DB().Raw())
}
