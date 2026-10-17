package dbrepo

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// RuntimeStateRepository implements persistence.RuntimeStateRepository on
// PostgreSQL (WBS 6.23/6.24). One row per project: writes are upserts and
// the version column carries the optimistic-concurrency guard.
type RuntimeStateRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.RuntimeStateRepository = (*RuntimeStateRepository)(nil)

// NewRuntimeStateRepository binds the repository to an existing pool.
func NewRuntimeStateRepository(pool *pgxpool.Pool) *RuntimeStateRepository {
	return &RuntimeStateRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy of the repository that executes its operations on
// the given transaction (WBS 2.21).
func (r *RuntimeStateRepository) WithTx(tx persistence.Transaction) persistence.RuntimeStateRepository {
	return &RuntimeStateRepository{exec: txExecutor{tx}}
}

const runtimeStateColumns = `id, project_id, state_machine_id, state_machine_version,
current_state_id, phase_id, work_unit_id, entered_at, version, created_at, updated_at, execution_id`

func scanRuntimeState(row interface{ Scan(dest ...any) error }) (*persistence.RuntimeStateRecord, error) {
	var rs persistence.RuntimeStateRecord
	if err := row.Scan(
		&rs.ID, &rs.ProjectID, &rs.StateMachineID, &rs.StateMachineVersion,
		&rs.CurrentStateID, &rs.PhaseID, &rs.WorkUnitID, &rs.EnteredAt,
		&rs.Version, &rs.CreatedAt, &rs.UpdatedAt, &rs.ExecutionID); err != nil {
		return nil, err
	}
	return &rs, nil
}

// Create inserts the project's runtime state row (T6.23.1). A second
// create for the same project is rejected by the unique project_id
// constraint (translated to ErrConflict).
func (r *RuntimeStateRepository) Create(ctx context.Context, rs *persistence.RuntimeStateRecord) error {
	if rs.Version == 0 {
		rs.Version = 1
	}
	now := time.Now().UTC()
	if rs.CreatedAt.IsZero() {
		rs.CreatedAt = now
	}
	if rs.UpdatedAt.IsZero() {
		rs.UpdatedAt = now
	}
	entered := rs.EnteredAt
	if entered.IsZero() {
		entered = now
	}
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO runtime_state (id, project_id, state_machine_id, state_machine_version,
		 current_state_id, phase_id, work_unit_id, entered_at, version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		rs.ID, rs.ProjectID, rs.StateMachineID, rs.StateMachineVersion,
		rs.CurrentStateID, rs.PhaseID, rs.WorkUnitID, entered.UTC(),
		rs.Version, rs.CreatedAt.UTC(), rs.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create runtime state", err)
	}
	return nil
}

// Get returns the project's runtime state (T6.23.2); ErrNotFound when the
// project has none yet.
func (r *RuntimeStateRepository) Get(ctx context.Context, projectID uuid.UUID) (*persistence.RuntimeStateRecord, error) {
	row := r.exec.QueryRow(ctx,
		`SELECT `+runtimeStateColumns+` FROM runtime_state WHERE project_id = $1`, projectID)
	rs, err := scanRuntimeState(row)
	if err != nil {
		return nil, TranslateDBError("get runtime state", err)
	}
	return rs, nil
}

// UpdateWithVersion persists a full runtime state guarded by an
// optimistic-concurrency version (WBS 6.24, T6.24.1–T6.24.5). The row is
// only written when its stored version equals expectedVersion; on success
// the version is incremented atomically with the position change, so a
// stale writer can never win. A missing row is ErrNotFound; a stale
// version is ErrConflict (the row is probed to distinguish the two — the
// P3-24 project-version pattern).
func (r *RuntimeStateRepository) UpdateWithVersion(ctx context.Context, rs *persistence.RuntimeStateRecord, expectedVersion int) error {
	now := time.Now().UTC()
	tag, err := r.exec.Exec(ctx,
		`UPDATE runtime_state SET
		 state_machine_id = $2, state_machine_version = $3,
		 current_state_id = $4, phase_id = $5, work_unit_id = $6,
		 entered_at = $7, version = version + 1, updated_at = $8
		 WHERE project_id = $1 AND version = $9`,
		rs.ProjectID, rs.StateMachineID, rs.StateMachineVersion,
		rs.CurrentStateID, rs.PhaseID, rs.WorkUnitID,
		rs.EnteredAt.UTC(), now, expectedVersion)
	if err != nil {
		return TranslateDBError("update runtime state with version", err)
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "not found" from "stale version" by probing the row.
		var exists bool
		if err := r.exec.QueryRow(ctx,
			`SELECT true FROM runtime_state WHERE project_id = $1`, rs.ProjectID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return persistence.ErrNotFound
			}
			return TranslateDBError("probe runtime state", err)
		}
		if !exists {
			return persistence.ErrNotFound
		}
		return persistence.ErrConflict
	}
	return nil
}
