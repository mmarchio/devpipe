package integration

import "testing"

func validRole(t *testing.T, h *Harness) string {
	t.Helper()
	svc := snapshotSvc(t, h)
	roleID := seedSnapshotRole(t, h, svc) // prompt + instructions + states + files all set
	return roleID
}

// T5.19.1 — Valid role passes.
func TestP519ValidRolePasses(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := validRole(t, h)
	if problems := svc.ValidateRole(ctx0(), id); len(problems) != 0 {
		t.Fatalf("valid role problems = %+v, want none", problems)
	}
}

// T5.19.2 — Missing system prompt fails.
func TestP519MissingPromptFails(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedRole(t, h, nil, "engineer-x", "E") // no prompt added
	problems := svc.ValidateRole(ctx0(), id)
	var found bool
	for _, p := range problems {
		if p.Field == "system_prompt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-prompt problems = %+v, want a system_prompt problem", problems)
	}
}

// T5.19.3 — Disabled required instruction fails.
func TestP519DisabledInstructionFails(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := validRole(t, h)
	// Disable the role's first assigned instruction.
	snap, _ := svc.SnapshotRole(ctx0(), id)
	if len(snap.Instructions) == 0 {
		t.Fatal("no instructions in snapshot")
	}
	target := snap.Instructions[0].Key
	raw := h.App.DB().Raw()
	if _, err := raw.Exec(ctx0(), `UPDATE instructions SET enabled = false WHERE key = $1`, target); err != nil {
		t.Fatal(err)
	}
	problems := svc.ValidateRole(ctx0(), id)
	var found bool
	for _, p := range problems {
		if p.Field == "instructions" {
			found = true
		}
	}
	if !found {
		t.Fatalf("disabled-instruction problems = %+v, want an instructions problem", problems)
	}
}

// T5.19.4 — Missing allowed state fails.
func TestP519MissingAllowedStateFails(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := validRole(t, h)
	// Remove all allowed states.
	raw := h.App.DB().Raw()
	if _, err := raw.Exec(ctx0(), `DELETE FROM role_allowed_states WHERE role_id = $1`, parseUUID(t, id)); err != nil {
		t.Fatal(err)
	}
	problems := svc.ValidateRole(ctx0(), id)
	var found bool
	for _, p := range problems {
		if p.Field == "allowed_states" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing-state problems = %+v, want an allowed_states problem", problems)
	}
}

// T5.19.5 — Multiple validation failures returned.
func TestP519MultipleProblemsReturned(t *testing.T) {
	h := NewHarness(t)
	svc := snapshotSvc(t, h)
	id := seedRole(t, h, nil, "engineer-x", "E") // no prompt, no states
	if err := svc.Disable(ctx0(), id); err != nil {
		t.Fatal(err)
	}
	problems := svc.ValidateRole(ctx0(), id)
	if len(problems) < 3 {
		t.Fatalf("multi-failure problems = %+v, want at least 3 (disabled + missing prompt + no states)", problems)
	}
}
