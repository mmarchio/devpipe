package integration

import (
	"context"
	"testing"
)

// P4-26 (WBS 4.26): the dedicated integration test for the core append
// requirement during active development. A RUNNING project with phases
// 1-5 (Foundation DONE, Persistence DONE, API EXECUTING, State Machine
// PENDING, Prompt Engine PENDING) appends "Security Hardening". The append
// must add order 6, leave phases 1-5's IDs and orders untouched, keep
// phase 3 current, leave the new phase PENDING and WBS-capable, and leave
// the project RUNNING. Statuses are seeded by direct SQL (a test op cannot
// reach this DONE/DONE/EXECUTING/PENDING/PENDING combination on its own),
// matching the established p2_progress / p329 pattern.
func TestP426AppendDuringActiveDevelopment(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	names := []string{"Foundation", "Persistence", "API", "State Machine", "Prompt Engine"}
	created := make([]string, len(names))
	for i, name := range names {
		p, err := svc.CreatePhase(ctx, projectID, name, "")
		if err != nil {
			t.Fatalf("CreatePhase(%q): %v", name, err)
		}
		created[i] = p.ID.String()
	}

	// Seed the WBS 4.26 statuses directly: 1/2 DONE, 3 EXECUTING, 4/5 PENDING
	// (the default), project RUNNING.
	if _, err := h.DBExec(ctx,
		"UPDATE phases SET status = 'DONE', completed_at = now() WHERE id = '"+created[0]+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx,
		"UPDATE phases SET status = 'DONE', completed_at = now() WHERE id = '"+created[1]+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx,
		"UPDATE phases SET status = 'EXECUTING', started_at = now() WHERE id = '"+created[2]+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx,
		"UPDATE projects SET status = 'RUNNING' WHERE id = '"+projectID+"'"); err != nil {
		t.Fatal(err)
	}

	// Capture the before-state: each phase's ID and order keyed by name.
	beforeID := map[string]string{}
	beforeOrder := map[string]int{}
	for _, name := range names {
		var id string
		var order int
		if err := h.DBScan(ctx,
			"SELECT id::text, phase_order FROM phases WHERE project_id = '"+projectID+"' AND name = '"+name+"'",
			&id, &order); err != nil {
			t.Fatalf("read before phase %q: %v", name, err)
		}
		beforeID[name] = id
		beforeOrder[name] = order
	}

	// Append "Security Hardening" while the project is RUNNING.
	newPhase, err := svc.CreatePhase(ctx, projectID, "Security Hardening", "")
	if err != nil {
		t.Fatalf("CreatePhase(Security Hardening) while RUNNING: %v", err)
	}
	newID := newPhase.ID.String()

	// Phase 6 created with order 6.
	if newPhase.Order != 6 {
		t.Fatalf("appended phase order = %d, want 6 (max+1)", newPhase.Order)
	}
	var storedOrder int
	if err := h.DBScan(ctx,
		"SELECT phase_order FROM phases WHERE id = '"+newID+"'", &storedOrder); err != nil {
		t.Fatal(err)
	}
	if storedOrder != 6 {
		t.Fatalf("stored appended order = %d, want 6", storedOrder)
	}

	// Phases 1-5 retain their IDs and orders.
	for _, name := range names {
		var id string
		var order int
		if err := h.DBScan(ctx,
			"SELECT id::text, phase_order FROM phases WHERE project_id = '"+projectID+"' AND name = '"+name+"'",
			&id, &order); err != nil {
			t.Fatalf("read after phase %q: %v", name, err)
		}
		if id != beforeID[name] {
			t.Fatalf("phase %q ID changed: %s -> %s", name, beforeID[name], id)
		}
		if order != beforeOrder[name] {
			t.Fatalf("phase %q order renumbered: %d -> %d", name, beforeOrder[name], order)
		}
	}

	// The full list is now exactly the 6 phases in the WBS 4.26 order.
	list, err := svc.ListPhases(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 6 {
		t.Fatalf("phase count = %d, want 6", len(list))
	}
	for i := 0; i < 5; i++ {
		if list[i].Order != i+1 || list[i].ID.String() != created[i] {
			t.Fatalf("position %d = order %d id %s, want order %d id %s (phase %s unchanged)",
				i+1, list[i].Order, list[i].ID, i+1, created[i], names[i])
		}
	}
	if list[5].Order != 6 || list[5].ID.String() != newID {
		t.Fatalf("position 6 = order %d id %s, want order 6 id %s (the appended phase)",
			list[5].Order, list[5].ID, newID)
	}

	// Phase 6 is PENDING.
	var newStatus string
	if err := h.DBScan(ctx, "SELECT status FROM phases WHERE id = '"+newID+"'", &newStatus); err != nil {
		t.Fatal(err)
	}
	if newStatus != "PENDING" {
		t.Fatalf("new phase status = %q, want PENDING", newStatus)
	}

	// Phase 3 remains the current phase (lowest ordered not-DONE): the service
	// read and a direct-SQL cross-check both point at order 3 "API".
	cur, err := svc.GetCurrentPhase(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ID.String() != created[2] || cur.Order != 3 {
		t.Fatalf("current phase = order %d %q, want order 3 API (phase 3 unchanged by append)", cur.Order, cur.Name)
	}
	var curOrder int
	var curName string
	if err := h.DBScan(ctx,
		"SELECT phase_order, name FROM phases WHERE project_id = '"+projectID+"' AND status <> 'DONE' ORDER BY phase_order ASC LIMIT 1",
		&curOrder, &curName); err != nil {
		t.Fatal(err)
	}
	if curOrder != 3 || curName != "API" {
		t.Fatalf("next incomplete = order %d %q, want order 3 API", curOrder, curName)
	}

	// Phase 6 can receive a WBS: CreateWBS succeeds and stores version 1.
	wbsSvc := newVersionedWBSDocService(t, h)
	if _, err := wbsSvc.CreateWBS(ctx, projectID, newID, "# Security Hardening\n"); err != nil {
		t.Fatalf("CreateWBS for appended phase: %v", err)
	}
	var vCount int
	if err := h.DBScan(ctx, "SELECT count(*) FROM wbs_versions WHERE phase_id = '"+newID+"'", &vCount); err != nil {
		t.Fatal(err)
	}
	if vCount != 1 {
		t.Fatalf("appended phase wbs_versions rows = %d, want 1 (WBS-capable)", vCount)
	}

	// Project remains RUNNING after the append.
	var projStatus string
	if err := h.DBScan(ctx, "SELECT status FROM projects WHERE id = '"+projectID+"'", &projStatus); err != nil {
		t.Fatal(err)
	}
	if projStatus != "RUNNING" {
		t.Fatalf("project status = %q, want RUNNING after append", projStatus)
	}
}
