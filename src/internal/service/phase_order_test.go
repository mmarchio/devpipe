package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPlanPhaseOrder(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	cur := []orderedPhase{
		{id: ids[0], order: 1, name: "Done", status: "DONE", hasPlan: true},
		{id: ids[1], order: 2, name: "Next", status: "PENDING"},
		{id: ids[2], order: 4, name: "Running", status: "EXECUTING", hasPlan: true},
		{id: ids[3], order: 5, name: "Later", status: "PENDING"},
		{id: ids[4], order: 7, name: "Last", status: "PENDING"},
	}
	// Movable phases reshuffle among their own order numbers (2, 5, 7);
	// started phases keep theirs.
	moves, err := planPhaseOrder(cur, []uuid.UUID{ids[0], ids[4], ids[2], ids[1], ids[3]}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 3 || moves[ids[4]] != 2 || moves[ids[1]] != 5 || moves[ids[3]] != 7 {
		t.Fatalf("moves %v", moves)
	}
	// Unchanged order: nothing to do.
	if moves, err := planPhaseOrder(cur, ids, nil); err != nil || len(moves) != 0 {
		t.Fatalf("no-op: %v %v", moves, err)
	}
	reject := func(want []uuid.UUID, active *uuid.UUID, kind error, msg string) {
		t.Helper()
		_, err := planPhaseOrder(cur, want, active)
		var oe *PhaseOrderError
		if !errors.As(err, &oe) || !errors.Is(err, kind) || !strings.Contains(oe.Message, msg) {
			t.Fatalf("want %v %q, got %v", kind, msg, err)
		}
	}
	// A started phase cannot be displaced.
	reject([]uuid.UUID{ids[1], ids[0], ids[2], ids[3], ids[4]}, nil, ErrConflict, "phase 1 — Done cannot move: the phase has started (DONE)")
	reject([]uuid.UUID{ids[0], ids[2], ids[1], ids[3], ids[4]}, nil, ErrConflict, "phase 4 — Running cannot move: the phase has started (EXECUTING)")
	// The phase an active execution is on (still PENDING while planned)
	// cannot move.
	reject([]uuid.UUID{ids[0], ids[3], ids[2], ids[1], ids[4]}, &ids[1], ErrConflict, "the active execution is working on this phase")
	// Malformed orders.
	reject(ids[:4], nil, ErrConflict, "the order lists 4 phases; the project has 5")
	reject([]uuid.UUID{ids[0], ids[1], ids[2], ids[3], uuid.New()}, nil, ErrConflict, "not in this project")
	reject([]uuid.UUID{ids[0], ids[1], ids[2], ids[3], ids[3]}, nil, ErrInvalidInput, "more than once")
}

func TestPhaseLockReason(t *testing.T) {
	for _, c := range []struct {
		status       string
		plan, active bool
		movable      bool
	}{{"PENDING", false, false, true}, {"PLANNING", false, false, false}, {"PENDING", true, false, false}, {"PENDING", false, true, false}} {
		if got := PhaseLockReason(c.status, c.plan, c.active) == ""; got != c.movable {
			t.Errorf("%+v: movable %v", c, got)
		}
	}
}
