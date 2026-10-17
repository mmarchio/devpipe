package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const stateFileTypeColumns = `id, project_id, key, name, description, default_filename, scope, source, required, enabled, created_at, updated_at`

// StateFileTypeRepository implements persistence.StateFileTypeRepository on
// PostgreSQL (WBS 7.1 persistence half, P7-05). Scoped-key uniqueness rides
// on the (COALESCE(project_id, sentinel), key) unique index.
type StateFileTypeRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.StateFileTypeRepository = (*StateFileTypeRepository)(nil)

// NewStateFileTypeRepository binds the repository to an existing pool.
func NewStateFileTypeRepository(pool *pgxpool.Pool) *StateFileTypeRepository {
	return &StateFileTypeRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *StateFileTypeRepository) WithTx(tx persistence.Transaction) persistence.StateFileTypeRepository {
	return &StateFileTypeRepository{exec: txExecutor{tx}}
}

func scanStateFileTypeRow(row interface{ Scan(dest ...any) error }) (*persistence.StateFileTypeRecord, error) {
	var (
		rec    persistence.StateFileTypeRecord
		projID *uuid.UUID
	)
	if err := row.Scan(&rec.ID, &projID, &rec.Key, &rec.Name, &rec.Description, &rec.DefaultFilename, &rec.Scope, &rec.Source, &rec.Required, &rec.Enabled, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	rec.ProjectID = projID
	return &rec, nil
}

// Create inserts a new state-file type (WBS 7.1); a scoped key duplicate
// yields persistence.ErrConflict (T7.1.3 DB half).
func (r *StateFileTypeRepository) Create(ctx context.Context, rec *persistence.StateFileTypeRecord) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO state_file_types (id, project_id, key, name, description, default_filename, scope, source, required, enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		rec.ID, rec.ProjectID, rec.Key, rec.Name, rec.Description, rec.DefaultFilename, rec.Scope, rec.Source, rec.Required, rec.Enabled, rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create state-file type", err)
	}
	return nil
}

// Get returns the state-file type with the given id (T7.7.5 shape:
// ErrNotFound when missing).
func (r *StateFileTypeRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.StateFileTypeRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+stateFileTypeColumns+` FROM state_file_types WHERE id = $1`, id)
	rec, err := scanStateFileTypeRow(row)
	if err != nil {
		return nil, TranslateDBError("get state-file type", err)
	}
	return rec, nil
}

// ListByScope lists all types in one scope (nil = global), ordered by key.
func (r *StateFileTypeRepository) ListByScope(ctx context.Context, projectID *uuid.UUID) ([]*persistence.StateFileTypeRecord, error) {
	var (
		rows persistence.Rows
		err  error
	)
	if projectID == nil {
		rows, err = r.exec.Query(ctx, `SELECT `+stateFileTypeColumns+` FROM state_file_types WHERE project_id IS NULL ORDER BY key`)
	} else {
		rows, err = r.exec.Query(ctx, `SELECT `+stateFileTypeColumns+` FROM state_file_types WHERE project_id = $1 ORDER BY key`, *projectID)
	}
	if err != nil {
		return nil, TranslateDBError("list state-file types by scope", err)
	}
	defer rows.Close()
	var out []*persistence.StateFileTypeRecord
	for rows.Next() {
		rec, err := scanStateFileTypeRow(rows)
		if err != nil {
			return nil, TranslateDBError("list state-file types by scope", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list state-file types by scope", err)
	}
	return out, nil
}
