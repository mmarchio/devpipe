package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const roleColumns = `id, project_id, key, name, description, enabled, version, created_at, updated_at`

// RoleRepository implements persistence.RoleRepository on PostgreSQL (WBS 5.1).
type RoleRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.RoleRepository = (*RoleRepository)(nil)

// NewRoleRepository binds the repository to an existing pool.
func NewRoleRepository(pool *pgxpool.Pool) *RoleRepository {
	return &RoleRepository{exec: poolAdapter{pool}}
}

// SetEnabled toggles the role's enabled flag (WBS 5.13) and bumps
// UpdatedAt. Disabling is non-destructive — prompt history is untouched
// (T5.13.3). ErrNotFound when the role is missing.
func (r *RoleRepository) SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) (*persistence.RoleRecord, error) {
	row := r.exec.QueryRow(ctx,
		`UPDATE roles SET enabled = $2, updated_at = now() WHERE id = $1 RETURNING `+roleColumns, id, enabled)
	rec, err := scanRoleRow(row)
	if err != nil {
		return nil, TranslateDBError("set enabled", err)
	}
	return rec, nil
}

// Update mutates the role's editable fields and increments its version
// (WBS 5.17 project role overrides). ErrNotFound when the role is missing.
func (r *RoleRepository) Update(ctx context.Context, id uuid.UUID, name, description string) (*persistence.RoleRecord, error) {
	row := r.exec.QueryRow(ctx,
		`UPDATE roles SET name = $2, description = $3, version = version + 1, updated_at = now()
		 WHERE id = $1 RETURNING `+roleColumns, id, name, description)
	rec, err := scanRoleRow(row)
	if err != nil {
		return nil, TranslateDBError("update role", err)
	}
	return rec, nil
}

// Delete removes the role row (WBS 5.21); ErrNotFound when the role is
// missing.
func (r *RoleRepository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.exec.Exec(ctx, `DELETE FROM roles WHERE id = $1`, id)
	if err != nil {
		return TranslateDBError("delete role", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *RoleRepository) WithTx(tx persistence.Transaction) persistence.RoleRepository {
	return &RoleRepository{exec: txExecutor{tx}}
}

// scanRoleRow scans a roles row into a fresh record (never into a domain
// struct with pointer fields — project_id is nullable).
func scanRoleRow(row interface{ Scan(dest ...any) error }) (*persistence.RoleRecord, error) {
	var (
		rec    persistence.RoleRecord
		projID *uuid.UUID
	)
	if err := row.Scan(&rec.ID, &projID, &rec.Key, &rec.Name, &rec.Description, &rec.Enabled, &rec.Version, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	rec.ProjectID = projID
	return &rec, nil
}

// Create inserts a new role (WBS 5.1). A scoped key duplicate yields
// persistence.ErrConflict (DB-enforced via the scoped unique index).
func (r *RoleRepository) Create(ctx context.Context, rec *persistence.RoleRecord) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO roles (id, project_id, key, name, description, enabled, version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		rec.ID, rec.ProjectID, rec.Key, rec.Name, rec.Description, rec.Enabled, rec.Version, rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create role", err)
	}
	return nil
}

// Get returns the role with the given id; ErrNotFound when missing.
func (r *RoleRepository) Get(ctx context.Context, id string) (*persistence.RoleRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+roleColumns+` FROM roles WHERE id = $1`, id)
	rec, err := scanRoleRow(row)
	if err != nil {
		return nil, TranslateDBError("get role", err)
	}
	return rec, nil
}

// ListByScope lists all roles in one scope (nil = global), ordered by key.
func (r *RoleRepository) ListByScope(ctx context.Context, projectID *uuid.UUID) ([]*persistence.RoleRecord, error) {
	var (
		rows persistence.Rows
		err  error
	)
	if projectID == nil {
		rows, err = r.exec.Query(ctx, `SELECT `+roleColumns+` FROM roles WHERE project_id IS NULL ORDER BY key`)
	} else {
		rows, err = r.exec.Query(ctx, `SELECT `+roleColumns+` FROM roles WHERE project_id = $1 ORDER BY key`, *projectID)
	}
	if err != nil {
		return nil, TranslateDBError("list roles by scope", err)
	}
	defer rows.Close()
	var out []*persistence.RoleRecord
	for rows.Next() {
		rec, err := scanRoleRow(rows)
		if err != nil {
			return nil, TranslateDBError("list roles by scope", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list roles by scope", err)
	}
	return out, nil
}
