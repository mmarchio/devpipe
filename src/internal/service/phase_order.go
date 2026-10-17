package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/phase"
)

// Phase reordering. Only phases that have not started can move: a PENDING
// phase with no execution plan that an active execution is not working on
// (its planning files are named by its order). Started phases keep their
// positions; movable phases are reshuffled among their own order numbers.

// PhaseOrderError explains a rejected reorder. It wraps ErrInvalidInput
// (malformed order) or ErrConflict (a phase that cannot move).
type PhaseOrderError struct {
	Message string
	kind    error
}

func (e *PhaseOrderError) Error() string { return e.Message }
func (e *PhaseOrderError) Unwrap() error { return e.kind }

// PhaseLockReason returns why a phase cannot be reordered ("" = movable).
func PhaseLockReason(status string, hasPlan, activeExecutionPhase bool) string {
	switch {
	case status != string(phase.StatusPENDING):
		return "the phase has started (" + status + ")"
	case hasPlan:
		return "the phase has an execution plan"
	case activeExecutionPhase:
		return "the active execution is working on this phase"
	}
	return ""
}

// activeExecutionStatuses are the statuses of a project's (single) active
// execution.
var activeExecutionStatuses = []string{"INITIALIZING", "READY", "RUNNING", "WAITING", "PAUSED", "RECOVERY_REQUIRED"}

type orderedPhase struct {
	id      uuid.UUID
	order   int
	name    string
	status  string
	hasPlan bool
}

// ReorderPhases sets the project's phase order to phaseIDs (every phase,
// each once, first to last). Phases that cannot move must keep their
// positions.
func (s *PhaseService) ReorderPhases(ctx context.Context, projectID string, phaseIDs []string) ([]*phase.Phase, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrNotFound
	}
	if s.txPool == nil {
		return nil, ErrInfrastructure
	}
	want := make([]uuid.UUID, len(phaseIDs))
	for i, raw := range phaseIDs {
		if want[i], err = uuid.Parse(raw); err != nil {
			return nil, &PhaseOrderError{Message: fmt.Sprintf("%q is not a phase id", raw), kind: ErrInvalidInput}
		}
	}
	err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		// Serializes with phase appends (which update the project row).
		var found bool
		if err := tx.QueryRow(ctx, `SELECT true FROM projects WHERE id = $1 FOR UPDATE`, pid).Scan(&found); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return translate(err)
		}
		rows, err := tx.Query(ctx, `SELECT ph.id, ph.phase_order, ph.name, ph.status,
			EXISTS (SELECT 1 FROM execution_plans pl WHERE pl.phase_id = ph.id)
			FROM phases ph WHERE ph.project_id = $1 ORDER BY ph.phase_order`, pid)
		if err != nil {
			return translate(err)
		}
		var cur []orderedPhase
		for rows.Next() {
			var p orderedPhase
			if err := rows.Scan(&p.id, &p.order, &p.name, &p.status, &p.hasPlan); err != nil {
				rows.Close()
				return translate(err)
			}
			cur = append(cur, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return translate(err)
		}
		var activePhase *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT current_phase_id FROM executions WHERE project_id = $1 AND status = ANY($2) LIMIT 1`,
			pid, activeExecutionStatuses).Scan(&activePhase); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return translate(err)
		}
		moves, err := planPhaseOrder(cur, want, activePhase)
		if err != nil {
			return err
		}
		if len(moves) == 0 {
			return nil
		}
		// Two steps keep (project_id, phase_order) unique and positive:
		// park the moved phases above every order, then place them.
		offset := cur[len(cur)-1].order
		ids := make([]uuid.UUID, 0, len(moves))
		for id := range moves {
			ids = append(ids, id)
		}
		if _, err := tx.Exec(ctx, `UPDATE phases SET phase_order = phase_order + $2 WHERE id = ANY($1)`, ids, offset); err != nil {
			return translate(err)
		}
		for id, order := range moves {
			if _, err := tx.Exec(ctx, `UPDATE phases SET phase_order = $2, updated_at = now() WHERE id = $1`, id, order); err != nil {
				return translate(err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.ListPhases(ctx, projectID)
}

// planPhaseOrder checks the requested order against the current phases
// (ascending order) and returns the new order of every phase that moves.
func planPhaseOrder(cur []orderedPhase, want []uuid.UUID, activePhase *uuid.UUID) (map[uuid.UUID]int, error) {
	if len(want) != len(cur) {
		return nil, &PhaseOrderError{Message: fmt.Sprintf("the order lists %d phases; the project has %d (the phases may have changed — reload)",
			len(want), len(cur)), kind: ErrConflict}
	}
	known := map[uuid.UUID]bool{}
	for _, p := range cur {
		known[p.id] = true
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range want {
		if !known[id] {
			return nil, &PhaseOrderError{Message: "the order names a phase that is not in this project (reload the page)", kind: ErrConflict}
		}
		if seen[id] {
			return nil, &PhaseOrderError{Message: "the order lists a phase more than once", kind: ErrInvalidInput}
		}
		seen[id] = true
	}
	moves := map[uuid.UUID]int{}
	for i, p := range cur {
		reason := PhaseLockReason(p.status, p.hasPlan, activePhase != nil && *activePhase == p.id)
		if reason != "" && want[i] != p.id {
			return nil, &PhaseOrderError{Message: fmt.Sprintf("phase %d — %s cannot move: %s", p.order, p.name, reason), kind: ErrConflict}
		}
		if want[i] != p.id {
			moves[want[i]] = p.order
		}
	}
	return moves, nil
}
