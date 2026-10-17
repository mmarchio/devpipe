// P6-26 (WBS 6.36): additive repository surface for the state-configuration
// service: state update/delete, referenced-state transition lookups,
// state-file requirement removal, and state-role assignment removal.

package dbrepo

import (
	"context"

	"github.com/google/uuid"

	"overseer/internal/persistence"
)

// Update mutates the state's editable definition fields (name,
// description, prompt template, kind, terminal, enabled) — the
// state-configuration surface (WBS 6.36). The identity fields (machine,
// version, key) are immutable; updated_at is advanced. ErrNotFound when
// the state row is missing.
func (r *WorkflowStateRepository) Update(ctx context.Context, rec *persistence.WorkflowStateRecord) (*persistence.WorkflowStateRecord, error) {
	tag, err := r.exec.Exec(ctx,
		`UPDATE workflow_states
		 SET name = $2, description = $3, prompt_template = $4,
		     kind = $5, terminal = $6, enabled = $7, updated_at = now()
		 WHERE id = $1`,
		rec.ID, rec.Name, rec.Description, rec.PromptTemplate,
		rec.Kind, rec.Terminal, rec.Enabled)
	if err != nil {
		return nil, TranslateDBError("update workflow state", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, persistence.ErrNotFound
	}
	return r.Get(ctx, rec.ID)
}

// Delete removes the state row (WBS 6.36 destructive delete). Referenced
// states (initial state, transitions, runtime state, transition history)
// are protected by the service layer (T6.36.5) BEFORE calling; child rows
// (assignments, instructions, file requirements) cascade at the DB level.
// ErrNotFound when the state row is missing.
func (r *WorkflowStateRepository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.exec.Exec(ctx, `DELETE FROM workflow_states WHERE id = $1`, id)
	if err != nil {
		return TranslateDBError("delete workflow state", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// ReferencedByStates reports the set of state ids referenced by any
// transition of the given machine version (from- or to-endpoint) (WBS
// 6.36, T6.36.5 protection check).
func (r *StateTransitionRepository) ReferencedByStates(ctx context.Context, machineID uuid.UUID, version int) (map[uuid.UUID]bool, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT from_state_id, to_state_id FROM state_transitions
		 WHERE machine_id = $1 AND version = $2`, machineID, version)
	if err != nil {
		return nil, TranslateDBError("list referenced states", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var fromID, toID uuid.UUID
		if err := rows.Scan(&fromID, &toID); err != nil {
			return nil, TranslateDBError("scan referenced states", err)
		}
		out[fromID] = true
		out[toID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("iterate referenced states", err)
	}
	return out, nil
}

// ListByStateID lists the history rows that pass through the given state
// (from- or to-endpoint), across the machine versions of the project
// (WBS 6.36, T6.36.5 protection check).
func (r *TransitionHistoryRepository) ListByStateID(ctx context.Context, stateID uuid.UUID) ([]*persistence.TransitionHistoryRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT id, project_id, state_machine_id, state_machine_version,
		        COALESCE(from_state_id, '00000000-0000-0000-0000-000000000000'::uuid), to_state_id, outcome, phase_id, work_unit_id,
		        runtime_version, seq, created_at
		 FROM state_transition_history
		 WHERE from_state_id = $1 OR to_state_id = $1`, stateID)
	if err != nil {
		return nil, TranslateDBError("list history by state", err)
	}
	defer rows.Close()
	out := []*persistence.TransitionHistoryRecord{}
	for rows.Next() {
		var rec persistence.TransitionHistoryRecord
		if err := rows.Scan(&rec.ID, &rec.ProjectID, &rec.StateMachineID, &rec.StateMachineVersion,
			&rec.FromStateID, &rec.ToStateID, &rec.Outcome, &rec.PhaseID, &rec.WorkUnitID,
			&rec.RuntimeVersion, &rec.Seq, &rec.CreatedAt); err != nil {
			return nil, TranslateDBError("scan history by state", err)
		}
		out = append(out, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("iterate history by state", err)
	}
	return out, nil
}

// Remove deletes the state's requirement for the given category (WBS 6.36
// state-file replacement). A missing (state, category) row is a no-op: the
// caller drives a replace from a prior read.
func (r *StateFileRequirementRepository) Remove(ctx context.Context, stateID uuid.UUID, category string) error {
	_, err := r.exec.Exec(ctx,
		`DELETE FROM state_file_requirements WHERE state_id = $1 AND category = $2`,
		stateID, category)
	if err != nil {
		return TranslateDBError("remove state-file requirement", err)
	}
	return nil
}

// Remove deletes the state-role assignment for the given role (WBS 6.36).
// A missing (state, role) row is a no-op.
func (r *StateRoleAssignmentRepository) Remove(ctx context.Context, stateID, roleID uuid.UUID) error {
	_, err := r.exec.Exec(ctx,
		`DELETE FROM state_role_assignments WHERE state_id = $1 AND role_id = $2`,
		stateID, roleID)
	if err != nil {
		return TranslateDBError("remove state-role assignment", err)
	}
	return nil
}
