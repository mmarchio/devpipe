package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/phase"
	"overseer/internal/service"
)

// setPhaseStatus drives a phase to a given state via direct SQL (setup only;
// lifecycle ops are out of scope for WBS 4.5).
func setPhaseStatus(t *testing.T, h *Harness, phaseID string, status phase.PhaseStatus) {
	t.Helper()
	if _, err := h.DBExec(context.Background(), "UPDATE phases SET status = '"+string(status)+"' WHERE id = '"+phaseID+"'"); err != nil {
		t.Fatalf("set phase status: %v", err)
	}
}

// phaseRow returns the persisted name/description/order for direct-SQL assertions.
func phaseRow(t *testing.T, h *Harness, phaseID string) (name, description string, order int) {
	t.Helper()
	if err := h.DBScan(context.Background(), "SELECT name, description, phase_order FROM phases WHERE id = '"+phaseID+"'", &name, &description, &order); err != nil {
		t.Fatalf("read phase row: %v", err)
	}
	return
}

// T4.5.1 — Pending phase editable: a PENDING phase's name and description are
// fully editable through the service and the changes persist.
func TestT451PendingPhaseEditable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Original", "original description")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	if p.Status != phase.StatusPENDING {
		t.Fatalf("new phase status = %s, want PENDING", p.Status)
	}
	newName, newDesc := "Renamed", "renamed description"
	updated, err := svc.UpdatePhase(ctx, projectID, p.ID.String(), &newName, &newDesc)
	if err != nil {
		t.Fatalf("UpdatePhase on PENDING: %v", err)
	}
	if updated.Name != newName || updated.Description != newDesc {
		t.Fatalf("update = (%q %q), want (%q %q)", updated.Name, updated.Description, newName, newDesc)
	}
	name, description, order := phaseRow(t, h, p.ID.String())
	if name != newName || description != newDesc {
		t.Fatalf("persisted = (%q %q), want (%q %q)", name, description, newName, newDesc)
	}
	if updated.Order != order || order != p.Order {
		t.Fatalf("order changed: %d -> %d", p.Order, order)
	}
}

// T4.5.2 — Executing phase structural edit rejected: an EXECUTING phase
// rejects name/description edits with ErrConflict and the row is unchanged.
func TestT452ExecutingPhaseEditRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Build", "building")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	setPhaseStatus(t, h, p.ID.String(), phase.StatusEXECUTING)
	beforeName, beforeDesc, beforeOrder := phaseRow(t, h, p.ID.String())

	name, desc := "Hacked", "hacked"
	_, err = svc.UpdatePhase(ctx, projectID, p.ID.String(), &name, &desc)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("UpdatePhase on EXECUTING = %v, want ErrConflict", err)
	}
	name, description, order := phaseRow(t, h, p.ID.String())
	if name != beforeName || description != beforeDesc || order != beforeOrder {
		t.Fatalf("row changed: (%q %q %d) -> (%q %q %d)", beforeName, beforeDesc, beforeOrder, name, description, order)
	}
}

// T4.5.3 — Done phase structural edit rejected: a DONE phase rejects edits
// with ErrConflict and the row is unchanged.
func TestT453DonePhaseEditRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Wrap", "finishing")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	setPhaseStatus(t, h, p.ID.String(), phase.StatusDONE)
	beforeName, beforeDesc, beforeOrder := phaseRow(t, h, p.ID.String())

	name, desc := "Late", "late change"
	_, err = svc.UpdatePhase(ctx, projectID, p.ID.String(), &name, &desc)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("UpdatePhase on DONE = %v, want ErrConflict", err)
	}
	name, description, order := phaseRow(t, h, p.ID.String())
	if name != beforeName || description != beforeDesc || order != beforeOrder {
		t.Fatalf("row changed: (%q %q %d) -> (%q %q %d)", beforeName, beforeDesc, beforeOrder, name, description, order)
	}
}

// T4.5.4 — Edit does not modify phase order: edits on editable states and
// rejected edits on protected states both leave phase_order untouched.
func TestT454EditDoesNotModifyOrder(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	for _, status := range []phase.PhaseStatus{
		phase.StatusPENDING, phase.StatusPLANNING, phase.StatusREADY, phase.StatusFAILED,
	} {
		p, err := svc.CreatePhase(ctx, projectID, "phase-"+string(status), "")
		if err != nil {
			t.Fatalf("CreatePhase: %v", err)
		}
		setPhaseStatus(t, h, p.ID.String(), status)
		name := "edited " + string(status)
		if _, err := svc.UpdatePhase(ctx, projectID, p.ID.String(), &name, nil); err != nil {
			t.Fatalf("UpdatePhase on %s: %v", status, err)
		}
		if _, _, order := phaseRow(t, h, p.ID.String()); order != p.Order {
			t.Fatalf("%s: order changed %d -> %d", status, p.Order, order)
		}
	}
	// Rejected edits on protected states also leave order untouched.
	for _, status := range []phase.PhaseStatus{
		phase.StatusEXECUTING, phase.StatusVALIDATING, phase.StatusDONE,
	} {
		p, err := svc.CreatePhase(ctx, projectID, "phase-"+string(status), "")
		if err != nil {
			t.Fatalf("CreatePhase: %v", err)
		}
		setPhaseStatus(t, h, p.ID.String(), status)
		name := "should not stick"
		if _, err := svc.UpdatePhase(ctx, projectID, p.ID.String(), &name, nil); err == nil {
			t.Fatalf("UpdatePhase on %s: expected conflict, got nil", status)
		}
		if _, _, order := phaseRow(t, h, p.ID.String()); order != p.Order {
			t.Fatalf("%s: order changed %d -> %d", status, p.Order, order)
		}
	}
}

// T4.5.5 — Protected edit returns conflict: every protected state
// (EXECUTING/VALIDATING/DONE) maps to ErrConflict, distinguishable from
// ErrInvalidInput and ErrNotFound.
func TestT455ProtectedEditReturnsConflict(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	for _, status := range []phase.PhaseStatus{
		phase.StatusEXECUTING, phase.StatusVALIDATING, phase.StatusDONE,
	} {
		p, err := svc.CreatePhase(ctx, projectID, "phase-"+string(status), "")
		if err != nil {
			t.Fatalf("CreatePhase: %v", err)
		}
		setPhaseStatus(t, h, p.ID.String(), status)
		name := "x"
		_, err = svc.UpdatePhase(ctx, projectID, p.ID.String(), &name, nil)
		if !errors.Is(err, service.ErrConflict) {
			t.Fatalf("%s: UpdatePhase = %v, want ErrConflict", status, err)
		}
		if errors.Is(err, service.ErrInvalidInput) || errors.Is(err, service.ErrNotFound) {
			t.Fatalf("%s: unexpected sentinel %v", status, err)
		}
	}
}
