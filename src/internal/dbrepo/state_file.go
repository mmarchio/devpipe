package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const sfColumns = `id, project_id, type_id, scope, phase_id, work_unit_id, current_version, created_at, updated_at`

// StateFileRepository implements persistence.StateFileRepository on
// PostgreSQL (WBS 7.4/7.7 persistence half, P7-05). Cross-project phase and
// work-unit associations are rejected by the composite (project_id, id)
// foreign keys (T7.4.5/T7.4.6 DB half).
type StateFileRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.StateFileRepository = (*StateFileRepository)(nil)

// NewStateFileRepository binds the repository to an existing pool.
func NewStateFileRepository(pool *pgxpool.Pool) *StateFileRepository {
	return &StateFileRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *StateFileRepository) WithTx(tx persistence.Transaction) persistence.StateFileRepository {
	return &StateFileRepository{exec: txExecutor{tx}}
}

func scanSFRow(row interface{ Scan(dest ...any) error }) (*persistence.StateFileRecord, error) {
	var (
		rec     persistence.StateFileRecord
		phaseID *uuid.UUID
		unitID  *uuid.UUID
	)
	if err := row.Scan(&rec.ID, &rec.ProjectID, &rec.TypeID, &rec.Scope, &phaseID, &unitID, &rec.CurrentVersion, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	rec.PhaseID = phaseID
	rec.WorkUnitID = unitID
	return &rec, nil
}

// Create inserts a new logical state file (T7.7.1); a duplicate
// (type, scope, association) yields persistence.ErrConflict and a
// cross-project phase/work-unit association yields
// persistence.ErrInvalidReference (T7.4.5/T7.4.6 DB half).
func (r *StateFileRepository) Create(ctx context.Context, rec *persistence.StateFileRecord) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO state_files (id, project_id, type_id, scope, phase_id, work_unit_id, current_version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		rec.ID, rec.ProjectID, rec.TypeID, rec.Scope, rec.PhaseID, rec.WorkUnitID, rec.CurrentVersion, rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create state-file", err)
	}
	return nil
}

// Get returns the state file with the given id; ErrNotFound when missing
// (T7.7.5).
func (r *StateFileRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.StateFileRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+sfColumns+` FROM state_files WHERE id = $1`, id)
	rec, err := scanSFRow(row)
	if err != nil {
		return nil, TranslateDBError("get state-file", err)
	}
	return rec, nil
}

// GetByTypeAndScope returns the unique file for a project's (type, scope,
// association) pair (T7.7.4 scope query); phaseID/workUnitID are nil for
// associations that do not apply. The project scoping is LOAD-BEARING
// (WBS 7.46): migration 00037 made (type, scope, association) uniqueness
// per-project, so a PROJECT-scoped lookup without project_id can return a
// DIFFERENT project's file (P7-25 caught this cross-project leak).
func (r *StateFileRepository) GetByTypeAndScope(ctx context.Context, projectID, typeID uuid.UUID, scope string, phaseID, workUnitID *uuid.UUID) (*persistence.StateFileRecord, error) {
	row := r.exec.QueryRow(ctx,
		`SELECT `+sfColumns+` FROM state_files
		 WHERE project_id = $1 AND type_id = $2 AND scope = $3 AND phase_id IS NOT DISTINCT FROM $4 AND work_unit_id IS NOT DISTINCT FROM $5`,
		projectID, typeID, scope, phaseID, workUnitID)
	rec, err := scanSFRow(row)
	if err != nil {
		return nil, TranslateDBError("get state-file by type and scope", err)
	}
	return rec, nil
}

func listStateFiles(ctx context.Context, r *StateFileRepository, op, clause string, arg uuid.UUID) ([]*persistence.StateFileRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+sfColumns+` FROM state_files WHERE `+clause+` ORDER BY id`, arg)
	if err != nil {
		return nil, TranslateDBError(op, err)
	}
	defer rows.Close()
	var out []*persistence.StateFileRecord
	for rows.Next() {
		rec, err := scanSFRow(rows)
		if err != nil {
			return nil, TranslateDBError(op, err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError(op, err)
	}
	return out, nil
}

// ListByProject lists all files of a project, ordered by id.
func (r *StateFileRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.StateFileRecord, error) {
	return listStateFiles(ctx, r, "list state-files by project", "project_id = $1", projectID)
}

// ListByPhase lists all files associated with a phase, ordered by id.
func (r *StateFileRepository) ListByPhase(ctx context.Context, phaseID uuid.UUID) ([]*persistence.StateFileRecord, error) {
	return listStateFiles(ctx, r, "list state-files by phase", "phase_id = $1", phaseID)
}

// ListByWorkUnit lists all files associated with a work unit, ordered by id.
func (r *StateFileRepository) ListByWorkUnit(ctx context.Context, workUnitID uuid.UUID) ([]*persistence.StateFileRecord, error) {
	return listStateFiles(ctx, r, "list state-files by work-unit", "work_unit_id = $1", workUnitID)
}
