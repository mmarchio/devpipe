package integration

import (
	"regexp"
	"testing"

	"overseer/internal/role"
	"overseer/internal/service"
)

// WBS 5.34 — pinned content for the ENGINEER configuration. Fixed values so
// the snapshot hash assertions verify the exact expected bytes (independent
// recomputation via role.HashSystemPromptContent), not just self-consistency.
const (
	p532PromptV2 = "engineer system prompt revision 2"
	p532PromptV3 = "engineer system prompt revision 3"

	p532EngV1 = "engineer instruction v1"
	p532EngV2 = "engineer instruction v2"
	p532EngV3 = "engineer instruction v3"
	p532EngV4 = "engineer instruction v4"

	p532TDDV1 = "tdd instruction v1"
	p532TDDV2 = "tdd instruction v2"

	p532CSSV1 = "coding standards instruction v1"
)

var p532Hash64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// p532Doc creates (when absent) and advances the global instruction document
// with the given key to exactly len(versions) versions: CreateInstruction
// writes v1, then one UpdateInstruction per further version (the seeded v1
// from the boot role fixture never exists here — this test creates its own
// documents, so the version number equals the number of writes).
func p532Doc(t *testing.T, svc *service.RoleService, key, name string, versions []string) string {
	t.Helper()
	ctx := ctx0()
	view, err := svc.CreateInstruction(ctx, nil, key, name, "", versions[0])
	if err != nil {
		t.Fatalf("CreateInstruction(%s): %v", key, err)
	}
	for i, v := range versions[1:] {
		view, err = svc.UpdateInstruction(ctx, view.ID, v, i+1)
		if err != nil {
			t.Fatalf("UpdateInstruction(%s) to v%d: %v", key, i+2, err)
		}
	}
	if view.Version != len(versions) {
		t.Fatalf("instruction %s version = %d, want %d", key, view.Version, len(versions))
	}
	return view.ID
}

// TestP532EngineerSnapshotIntegration — WBS 5.34: construct the ENGINEER
// configuration (system prompt v3; instructions ENGINEER.md v4, TDD.md v2,
// CODING_STANDARDS.md v1 in that assignment order; allowed states
// implementation + continuation; state files ACTIVE_TASK + PROJECT_STATE),
// request a snapshot, and verify every version, hash, and ordering
// relationship.
func TestP532EngineerSnapshotIntegration(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	ctx := ctx0()

	// The boot seed created the global ENGINEER role with its v1 system
	// prompt; two prompt appends bring it to exactly v3.
	roles, err := svc.ListRoles(ctx, nil)
	if err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	var engineerID string
	for _, r := range roles {
		if r.Key == "ENGINEER" {
			engineerID = r.ID.String()
		}
	}
	if engineerID == "" {
		t.Fatal("global ENGINEER role not seeded by boot")
	}

	if _, err := svc.UpdateSystemPrompt(ctx, engineerID, p532PromptV2, 1); err != nil {
		t.Fatalf("UpdateSystemPrompt v2: %v", err)
	}
	if _, err := svc.UpdateSystemPrompt(ctx, engineerID, p532PromptV3, 2); err != nil {
		t.Fatalf("UpdateSystemPrompt v3: %v", err)
	}

	// The three instruction documents at their exact versions, created in the
	// snapshot's assignment order.
	engID := p532Doc(t, svc, "ENGINEER", "ENGINEER.md",
		[]string{p532EngV1, p532EngV2, p532EngV3, p532EngV4})
	tddID := p532Doc(t, svc, "TDD", "TDD.md",
		[]string{p532TDDV1, p532TDDV2})
	cssID := p532Doc(t, svc, "CODING_STANDARDS", "CODING_STANDARDS.md",
		[]string{p532CSSV1})

	// Assign in the WBS 5.34 order.
	for _, instrID := range []string{engID, tddID, cssID} {
		if err := svc.AssignInstruction(ctx, engineerID, instrID); err != nil {
			t.Fatalf("AssignInstruction: %v", err)
		}
	}
	states, err := svc.SetAllowedStates(ctx, engineerID, []string{"implementation", "continuation"})
	if err != nil {
		t.Fatalf("SetAllowedStates: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("stored allowed states = %v, want 2 entries", states)
	}
	files, err := svc.SetStateFiles(ctx, engineerID, []string{"ACTIVE_TASK", "PROJECT_STATE"})
	if err != nil {
		t.Fatalf("SetStateFiles: %v", err)
	}
	if len(files) != 2 || files[0] != "ACTIVE_TASK" || files[1] != "PROJECT_STATE" {
		t.Fatalf("stored state files = %v, want [ACTIVE_TASK PROJECT_STATE]", files)
	}

	snap, err := svc.SnapshotRole(ctx, engineerID)
	if err != nil {
		t.Fatalf("SnapshotRole: %v", err)
	}

	// Role identity: key ENGINEER, version >= 1 (the seeded v1 role row; the
	// WBS 5.34 example value 5 is illustrative, not a fixture invariant —
	// prompt/instruction appends do not bump the role-row version).
	if snap.RoleKey != "ENGINEER" {
		t.Fatalf("snapshot role key = %q, want ENGINEER", snap.RoleKey)
	}
	if snap.RoleVersion < 1 {
		t.Fatalf("snapshot role version = %d, want >= 1", snap.RoleVersion)
	}

	// System prompt: exactly version 3 with the independently computed hash.
	if snap.SystemPrompt.Version != 3 {
		t.Fatalf("system prompt version = %d, want 3", snap.SystemPrompt.Version)
	}
	if snap.SystemPrompt.Content != p532PromptV3 {
		t.Fatalf("system prompt content = %q, want pinned v3 content", snap.SystemPrompt.Content)
	}
	if want := role.HashSystemPromptContent(p532PromptV3); snap.SystemPrompt.ContentHash != want {
		t.Fatalf("system prompt hash = %q, want %q", snap.SystemPrompt.ContentHash, want)
	}

	// Instructions: exactly three, in assignment order, each at its exact
	// version with its independently computed hash.
	type expected struct {
		key     string
		version int
		content string
	}
	want := []expected{
		{"ENGINEER", 4, p532EngV4},
		{"TDD", 2, p532TDDV2},
		{"CODING_STANDARDS", 1, p532CSSV1},
	}
	if len(snap.Instructions) != len(want) {
		t.Fatalf("snapshot instructions = %d entries, want %d", len(snap.Instructions), len(want))
	}
	for i, w := range want {
		got := snap.Instructions[i]
		if got.Key != w.key {
			t.Fatalf("instruction[%d] key = %q, want %q (ordering violated)", i, got.Key, w.key)
		}
		if got.Version != w.version {
			t.Fatalf("instruction %s version = %d, want %d", w.key, got.Version, w.version)
		}
		if got.Content != w.content {
			t.Fatalf("instruction %s content = %q, want pinned v%d content", w.key, got.Content, w.version)
		}
		if hsh := role.HashSystemPromptContent(w.content); got.ContentHash != hsh {
			t.Fatalf("instruction %s hash = %q, want %q", w.key, got.ContentHash, hsh)
		}
	}

	// Allowed states: the exact WBS 5.34 set.
	if !setEq(snap.AllowedStates, []string{"implementation", "continuation"}) {
		t.Fatalf("snapshot allowed states = %v, want [implementation continuation]", snap.AllowedStates)
	}
	// State files: the exact ordered WBS 5.34 list.
	if len(snap.StateFiles) != 2 || snap.StateFiles[0] != "ACTIVE_TASK" || snap.StateFiles[1] != "PROJECT_STATE" {
		t.Fatalf("snapshot state files = %v, want [ACTIVE_TASK PROJECT_STATE]", snap.StateFiles)
	}

	// Configuration hash: present, 64-hex, and deterministic across two
	// independent reads.
	if !p532Hash64.MatchString(snap.ConfigurationHash()) {
		t.Fatalf("configuration hash = %q, want 64-hex", snap.ConfigurationHash())
	}
	snap2, err := svc.SnapshotRole(ctx, engineerID)
	if err != nil {
		t.Fatalf("SnapshotRole (2nd read): %v", err)
	}
	if snap2.ConfigurationHash() != snap.ConfigurationHash() {
		t.Fatalf("configuration hash not deterministic: %q vs %q", snap.ConfigurationHash(), snap2.ConfigurationHash())
	}
}
