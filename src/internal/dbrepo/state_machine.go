package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const machineColumns = `id, project_id, family_id, name, description, version, enabled, initial_state_id, created_at, updated_at`

// StateMachineRepository implements persistence.StateMachineRepository on
// PostgreSQL (WBS 6.1).
type StateMachineRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.StateMachineRepository = (*StateMachineRepository)(nil)

// NewStateMachineRepository binds the repository to an existing pool.
func NewStateMachineRepository(pool *pgxpool.Pool) *StateMachineRepository {
	return &StateMachineRepository{exec: poolAdapter{pool}}
}

// Create inserts a new machine (WBS 6.1); a missing project row yields
// ErrInvalidReference (T6.1.6).
func (r *StateMachineRepository) Create(ctx context.Context, rec *persistence.MachineRecord) error {
	if rec.FamilyID == uuid.Nil {
		rec.FamilyID = rec.ID // a fresh machine is the root of its family
	}
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO state_machines (id, project_id, family_id, name, description, version, enabled, initial_state_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rec.ID, rec.ProjectID, rec.FamilyID, rec.Name, rec.Description, rec.Version, rec.Enabled, rec.InitialStateID, rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create machine", err)
	}
	return nil
}

// Get returns the machine with the given id; ErrNotFound when missing.
func (r *StateMachineRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.MachineRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+machineColumns+` FROM state_machines WHERE id = $1`, id)
	rec, err := scanMachineRow(row)
	if err != nil {
		return nil, TranslateDBError("get machine", err)
	}
	return rec, nil
}

// ListByProject lists all of a project's machines, ordered by version.
func (r *StateMachineRepository) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.MachineRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+machineColumns+` FROM state_machines WHERE project_id = $1 ORDER BY version`, projectID)
	if err != nil {
		return nil, TranslateDBError("list machines", err)
	}
	defer rows.Close()
	var out []*persistence.MachineRecord
	for rows.Next() {
		rec, err := scanMachineRow(rows)
		if err != nil {
			return nil, TranslateDBError("list machines", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list machines", err)
	}
	return out, nil
}

// SetInitialState points the machine's initial state at an existing state
// (WBS 6.3); ErrNotFound when the machine is missing.
func (r *StateMachineRepository) SetInitialState(ctx context.Context, id uuid.UUID, stateID uuid.UUID) error {
	tag, err := r.exec.Exec(ctx, `UPDATE state_machines SET initial_state_id = $2, updated_at = now() WHERE id = $1`, id, stateID)
	if err != nil {
		return TranslateDBError("set initial state", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// GetVersion returns the machine row for the given (machine, version)
// binding (WBS 6.20); unknown versions yield ErrNotFound.
func (r *StateMachineRepository) GetVersion(ctx context.Context, id uuid.UUID, version int) (*persistence.MachineRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+machineColumns+` FROM state_machines WHERE family_id = $1 AND version = $2`, id, version)
	rec, err := scanMachineRow(row)
	if err != nil {
		return nil, TranslateDBError("get machine version", err)
	}
	return rec, nil
}

// ListVersions returns all stored versions of a machine in ascending version
// order (WBS 6.20).
func (r *StateMachineRepository) ListVersions(ctx context.Context, id uuid.UUID) ([]*persistence.MachineRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+machineColumns+` FROM state_machines WHERE family_id = $1 ORDER BY version`, id)
	if err != nil {
		return nil, TranslateDBError("list machine versions", err)
	}
	defer rows.Close()
	var out []*persistence.MachineRecord
	for rows.Next() {
		rec, err := scanMachineRow(rows)
		if err != nil {
			return nil, TranslateDBError("list machine versions", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list machine versions", err)
	}
	return out, nil
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *StateMachineRepository) WithTx(tx persistence.Transaction) persistence.StateMachineRepository {
	return &StateMachineRepository{exec: txExecutor{tx}}
}

// scanMachineRow scans a state_machines row into a fresh record (initial
// state id is nullable).
func scanMachineRow(row interface{ Scan(dest ...any) error }) (*persistence.MachineRecord, error) {
	var (
		rec persistence.MachineRecord
		ini *uuid.UUID
	)
	if err := row.Scan(&rec.ID, &rec.ProjectID, &rec.FamilyID, &rec.Name, &rec.Description, &rec.Version, &rec.Enabled, &ini, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	rec.InitialStateID = ini
	return &rec, nil
}
