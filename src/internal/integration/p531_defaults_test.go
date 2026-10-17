package integration

import (
	"sort"
	"testing"

	"overseer/internal/role"
	"overseer/internal/service"
)

// WBS 5.33 expected allowed states per default role.
var p533AllowedStates = map[string][]string{
	"ARCHITECT":       {"architecture", "phase_planning"},
	"PLANNER":         {"phase_planning", "task_planning"},
	"ENGINEER":        {"implementation", "continuation"},
	"TESTER":          {"testing", "validation"},
	"PROJECT_MANAGER": {"review", "acceptance"},
}

// p533RoleFixture wires the role service and completes each default role's
// configuration through service operations: it resolves the role's default
// instruction-document keys (creating the missing global instruction document
// first — WBS 5.2 leaves document creation to the instruction domain),
// assigns them, and installs the WBS 5.33 allowed-state set. The boot seed is
// the source of truth for the role row, its enabled state, and its v1 system
// prompt; it deliberately assigns no instructions/states, so this fixture
// supplies them.
func p533RoleFixture(t *testing.T, h *Harness) (map[string]string, *service.RoleService) {
	t.Helper()
	svc := snapshotSvc(t, h)
	ctx := ctx0()

	roles, err := svc.ListRoles(ctx, nil)
	if err != nil {
		t.Fatalf("ListRoles(global): %v", err)
	}
	ids := map[string]string{}
	for _, r := range roles {
		ids[r.Key] = r.ID.String()
	}
	for key := range p533AllowedStates {
		if _, ok := ids[key]; !ok {
			t.Fatalf("global default role %s not seeded by boot", key)
		}
	}

	// Resolve (creating when absent) the global instruction documents.
	var instrKeys []string
	for _, def := range role.DefaultRoles() {
		instrKeys = append(instrKeys, def.Instructions...)
	}
	unique := map[string]struct{}{}
	for _, k := range instrKeys {
		unique[k] = struct{}{}
	}
	instrIDs := map[string]string{}
	for k := range unique {
		existing, err := svc.ListInstructions(ctx, nil)
		if err != nil {
			t.Fatalf("ListInstructions: %v", err)
		}
		for _, ins := range existing {
			if ins.Key == k {
				instrIDs[k] = ins.ID
				break
			}
		}
		if _, ok := instrIDs[k]; !ok {
			v, err := svc.CreateInstruction(ctx, nil, k, k, "fixture instruction", "instruction body: "+k)
			if err != nil {
				t.Fatalf("CreateInstruction(%s): %v", k, err)
			}
			instrIDs[k] = v.ID
		}
	}

	// Assign each role's default instructions and set its WBS 5.33 states.
	for _, def := range role.DefaultRoles() {
		for _, k := range def.Instructions {
			if err := svc.AssignInstruction(ctx, ids[def.Key], instrIDs[k]); err != nil {
				t.Fatalf("AssignInstruction(%s, %s): %v", def.Key, k, err)
			}
		}
		if _, err := svc.SetAllowedStates(ctx, ids[def.Key], p533AllowedStates[def.Key]); err != nil {
			t.Fatalf("SetAllowedStates(%s): %v", def.Key, err)
		}
	}
	return ids, svc
}

func setEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// WBS 5.33 — the five default roles coexist, each exists and is enabled.
func TestP533DefaultRolesCoexistAndEnabled(t *testing.T) {
	h := NewHarness(t)
	ids, svc := p533RoleFixture(t, h)
	for key, id := range ids {
		r, err := svc.GetRole(ctx0(), id)
		if err != nil {
			t.Fatalf("GetRole(%s): %v", key, err)
		}
		if r.ProjectID != nil {
			t.Fatalf("role %s is project-scoped, want global", key)
		}
		ok, err := svc.IsEligible(ctx0(), id)
		if err != nil {
			t.Fatalf("IsEligible(%s): %v", key, err)
		}
		if !ok {
			t.Fatalf("role %s not enabled/eligible", key)
		}
	}
}

// WBS 5.33 (3) — every role has a system prompt (the seeded v1).
func TestP533DefaultRolesHaveSystemPrompt(t *testing.T) {
	h := NewHarness(t)
	ids, svc := p533RoleFixture(t, h)
	for key, id := range ids {
		snap, err := svc.SnapshotRole(ctx0(), id)
		if err != nil {
			t.Fatalf("SnapshotRole(%s): %v", key, err)
		}
		if snap.SystemPrompt.Version != 1 || snap.SystemPrompt.Content == "" {
			t.Fatalf("role %s system prompt = v%d/%q, want non-empty seeded v1",
				key, snap.SystemPrompt.Version, snap.SystemPrompt.Content)
		}
	}
}

// WBS 5.33 (4) — every role has appropriate instructions: its resolved
// instruction set matches the role's default document keys exactly.
func TestP533DefaultRolesHaveAppropriateInstructions(t *testing.T) {
	h := NewHarness(t)
	ids, svc := p533RoleFixture(t, h)
	for _, def := range role.DefaultRoles() {
		got, err := svc.ResolveRoleInstructions(ctx0(), ids[def.Key])
		if err != nil {
			t.Fatalf("ResolveRoleInstructions(%s): %v", def.Key, err)
		}
		var keys []string
		for _, r := range got {
			keys = append(keys, r.Key)
		}
		if !setEq(keys, def.Instructions) {
			t.Fatalf("role %s resolved instructions = %v, want %v", def.Key, keys, def.Instructions)
		}
	}
}

// WBS 5.33 (5) — every role has its expected allowed states.
func TestP533DefaultRolesHaveAllowedStates(t *testing.T) {
	h := NewHarness(t)
	ids, svc := p533RoleFixture(t, h)
	for key, want := range p533AllowedStates {
		got, err := svc.ListAllowedStates(ctx0(), ids[key])
		if err != nil {
			t.Fatalf("ListAllowedStates(%s): %v", key, err)
		}
		if !setEq(got, want) {
			t.Fatalf("role %s allowed states = %v, want %v", key, got, want)
		}
	}
}

// WBS 5.33 (6) — configuration hash determinism: two consecutive reads per
// role are byte-identical, and the five distinct configurations hash
// differently.
func TestP533ConfigurationHashDeterministic(t *testing.T) {
	h := NewHarness(t)
	ids, svc := p533RoleFixture(t, h)
	seen := map[string]string{}
	for key, id := range ids {
		a, err := svc.SnapshotRole(ctx0(), id)
		if err != nil {
			t.Fatalf("SnapshotRole(%s) 1: %v", key, err)
		}
		b, err := svc.SnapshotRole(ctx0(), id)
		if err != nil {
			t.Fatalf("SnapshotRole(%s) 2: %v", key, err)
		}
		ha, hb := a.ConfigurationHash(), b.ConfigurationHash()
		if ha == "" || ha != hb {
			t.Fatalf("role %s hash not deterministic: %q vs %q", key, ha, hb)
		}
		if prevKey, dup := seen[ha]; dup {
			t.Fatalf("roles %s and %s share configuration hash %q despite distinct configurations", prevKey, key, ha)
		}
		seen[ha] = key
	}
}

// WBS 5.33 (7) — every role produces a RoleSnapshot consistent with its
// captured state.
func TestP533DefaultRolesProduceConsistentSnapshot(t *testing.T) {
	h := NewHarness(t)
	ids, svc := p533RoleFixture(t, h)
	for _, def := range role.DefaultRoles() {
		snap, err := svc.SnapshotRole(ctx0(), ids[def.Key])
		if err != nil {
			t.Fatalf("SnapshotRole(%s): %v", def.Key, err)
		}
		if snap.RoleKey != def.Key || snap.RoleVersion < 1 {
			t.Fatalf("snapshot role = %s v%d, want %s", snap.RoleKey, snap.RoleVersion, def.Key)
		}
		if snap.SystemPrompt.Version != 1 {
			t.Fatalf("role %s snapshot prompt version = %d, want 1", def.Key, snap.SystemPrompt.Version)
		}
		var keys []string
		for _, ins := range snap.Instructions {
			keys = append(keys, ins.Key)
		}
		if !setEq(keys, def.Instructions) {
			t.Fatalf("role %s snapshot instructions = %v, want %v", def.Key, keys, def.Instructions)
		}
		if !setEq(snap.AllowedStates, p533AllowedStates[def.Key]) {
			t.Fatalf("role %s snapshot allowed states = %v, want %v", def.Key, snap.AllowedStates, p533AllowedStates[def.Key])
		}
	}
}

// WBS 5.33 — "coexist and be resolved independently": mutating one role's
// configuration leaves every other role's resolution and hash untouched.
func TestP533RolesResolvedIndependently(t *testing.T) {
	h := NewHarness(t)
	ids, svc := p533RoleFixture(t, h)
	snap := func(id string) string {
		s, err := svc.SnapshotRole(ctx0(), id)
		if err != nil {
			t.Fatalf("SnapshotRole: %v", err)
		}
		return s.ConfigurationHash()
	}
	before := map[string]string{}
	for key, id := range ids {
		before[key] = snap(id)
	}
	// Mutate TESTER only: append a new instruction document and assign it.
	extra, err := svc.CreateInstruction(ctx0(), nil, "EXTRA", "EXTRA", "", "mutation")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AssignInstruction(ctx0(), ids["TESTER"], extra.ID); err != nil {
		t.Fatal(err)
	}
	after := snap(ids["TESTER"])
	if after == before["TESTER"] {
		t.Fatal("TESTER hash did not change after its own mutation")
	}
	for key, id := range ids {
		if key == "TESTER" {
			continue
		}
		if snap(id) != before[key] {
			t.Fatalf("role %s configuration changed when only TESTER was mutated", key)
		}
	}
}
