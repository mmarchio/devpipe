package dbrepo

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// RoleAllowedStateRepository implements role allowed-state definitions on
// PostgreSQL (WBS 5.11).
type RoleAllowedStateRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.RoleAllowedStateRepository = (*RoleAllowedStateRepository)(nil)

// NewRoleAllowedStateRepository binds the repository to an existing pool.
func NewRoleAllowedStateRepository(pool *pgxpool.Pool) *RoleAllowedStateRepository {
	return &RoleAllowedStateRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *RoleAllowedStateRepository) WithTx(tx persistence.Transaction) persistence.RoleAllowedStateRepository {
	return &RoleAllowedStateRepository{exec: txExecutor{tx}}
}

// AssignState records a role → state-key association (WBS 5.11); a duplicate
// yields persistence.ErrConflict (T5.11.3).
func (r *RoleAllowedStateRepository) AssignState(ctx context.Context, roleID uuid.UUID, stateKey string) (*persistence.RoleAllowedStateRecord, error) {
	rec := &persistence.RoleAllowedStateRecord{
		ID: uuid.New(), RoleID: roleID, StateKey: stateKey,
	}
	row := r.exec.QueryRow(ctx,
		`INSERT INTO role_allowed_states (id, role_id, state_key)
		 VALUES ($1, $2, $3) RETURNING id, role_id, state_key`,
		rec.ID, roleID, stateKey)
	if err := row.Scan(&rec.ID, &rec.RoleID, &rec.StateKey); err != nil {
		return nil, TranslateDBError("assign state", err)
	}
	return rec, nil
}

// UnassignState removes the association; ErrNotFound when absent.
func (r *RoleAllowedStateRepository) UnassignState(ctx context.Context, roleID uuid.UUID, stateKey string) error {
	tag, err := r.exec.Exec(ctx,
		`DELETE FROM role_allowed_states WHERE role_id = $1 AND state_key = $2`, roleID, stateKey)
	if err != nil {
		return TranslateDBError("unassign state", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// ListByRole returns the role's allowed state keys in a stable (sorted)
// order.
func (r *RoleAllowedStateRepository) ListByRole(ctx context.Context, roleID uuid.UUID) ([]*persistence.RoleAllowedStateRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT id, role_id, state_key FROM role_allowed_states WHERE role_id = $1 ORDER BY state_key ASC`, roleID)
	if err != nil {
		return nil, TranslateDBError("list allowed states", err)
	}
	defer rows.Close()
	var out []*persistence.RoleAllowedStateRecord
	for rows.Next() {
		var rec persistence.RoleAllowedStateRecord
		if err := rows.Scan(&rec.ID, &rec.RoleID, &rec.StateKey); err != nil {
			return nil, TranslateDBError("list allowed states", err)
		}
		out = append(out, &rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list allowed states", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StateKey < out[j].StateKey })
	return out, nil
}

// IsEligible reports whether the role may be assigned to the given state key
// (T5.11.5).
func (r *RoleAllowedStateRepository) IsEligible(ctx context.Context, roleID uuid.UUID, stateKey string) (bool, error) {
	var n int
	if err := r.exec.QueryRow(ctx,
		`SELECT count(*) FROM role_allowed_states WHERE role_id = $1 AND state_key = $2`, roleID, stateKey).Scan(&n); err != nil {
		return false, TranslateDBError("is eligible", err)
	}
	return n > 0, nil
}
