package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const stateAssignmentColumns = `id, state_id, role_id, ordinal, created_at, updated_at`

// StateRoleAssignmentRepository implements persistence.
// StateRoleAssignmentRepository on PostgreSQL (WBS 6.4).
type StateRoleAssignmentRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.StateRoleAssignmentRepository = (*StateRoleAssignmentRepository)(nil)

// NewStateRoleAssignmentRepository binds the repository to an existing pool.
func NewStateRoleAssignmentRepository(pool *pgxpool.Pool) *StateRoleAssignmentRepository {
	return &StateRoleAssignmentRepository{exec: poolAdapter{pool}}
}

// Create inserts a new assignment (WBS 6.4). The role must exist (T6.4.2,
// FK); a cross-project role is rejected by the DB-level trigger (T6.4.6); a
// duplicate (state, role) yields ErrConflict.
func (r *StateRoleAssignmentRepository) Create(ctx context.Context, rec *persistence.StateRoleAssignmentRecord) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO state_role_assignments (id, state_id, role_id, ordinal, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		rec.ID, rec.StateID, rec.RoleID, rec.Ordinal, rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create state role assignment", err)
	}
	return nil
}

// Get returns the assignment with the given id; ErrNotFound when missing.
func (r *StateRoleAssignmentRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.StateRoleAssignmentRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+stateAssignmentColumns+` FROM state_role_assignments WHERE id = $1`, id)
	rec, err := scanStateAssignmentRow(row)
	if err != nil {
		return nil, TranslateDBError("get state role assignment", err)
	}
	return rec, nil
}

// ListByState returns the state's ordered participating roles (T6.4.5),
// ordered by ordinal.
func (r *StateRoleAssignmentRepository) ListByState(ctx context.Context, stateID uuid.UUID) ([]*persistence.StateRoleAssignmentRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+stateAssignmentColumns+` FROM state_role_assignments WHERE state_id = $1 ORDER BY ordinal`, stateID)
	if err != nil {
		return nil, TranslateDBError("list state role assignments", err)
	}
	defer rows.Close()
	var out []*persistence.StateRoleAssignmentRecord
	for rows.Next() {
		rec, err := scanStateAssignmentRow(rows)
		if err != nil {
			return nil, TranslateDBError("list state role assignments", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list state role assignments", err)
	}
	return out, nil
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *StateRoleAssignmentRepository) WithTx(tx persistence.Transaction) persistence.StateRoleAssignmentRepository {
	return &StateRoleAssignmentRepository{exec: txExecutor{tx}}
}

// scanStateAssignmentRow scans a state_role_assignments row into a fresh record.
func scanStateAssignmentRow(row interface{ Scan(dest ...any) error }) (*persistence.StateRoleAssignmentRecord, error) {
	var rec persistence.StateRoleAssignmentRecord
	if err := row.Scan(&rec.ID, &rec.StateID, &rec.RoleID, &rec.Ordinal, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}
