package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const workflowStateColumns = `id, machine_id, version, key, name, description, prompt_template, kind, terminal, enabled, created_at, updated_at`

// WorkflowStateRepository implements persistence.WorkflowStateRepository on
// PostgreSQL (WBS 6.2).
type WorkflowStateRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.WorkflowStateRepository = (*WorkflowStateRepository)(nil)

// NewWorkflowStateRepository binds the repository to an existing pool.
func NewWorkflowStateRepository(pool *pgxpool.Pool) *WorkflowStateRepository {
	return &WorkflowStateRepository{exec: poolAdapter{pool}}
}

// Create inserts a new state (WBS 6.2); a duplicate (machine_id, key)
// yields ErrConflict and a missing machine row yields ErrInvalidReference.
func (r *WorkflowStateRepository) Create(ctx context.Context, rec *persistence.WorkflowStateRecord) error {
	if rec.Version == 0 {
		rec.Version = 1 // a fresh machine's states belong to its v1
	}
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO workflow_states (id, machine_id, version, key, name, description, prompt_template, kind, terminal, enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		rec.ID, rec.MachineID, rec.Version, rec.Key, rec.Name, rec.Description, rec.PromptTemplate, rec.Kind, rec.Terminal, rec.Enabled, rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create state", err)
	}
	return nil
}

// Get returns the state with the given id; ErrNotFound when missing.
func (r *WorkflowStateRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.WorkflowStateRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+workflowStateColumns+` FROM workflow_states WHERE id = $1`, id)
	rec, err := scanStateRow(row)
	if err != nil {
		return nil, TranslateDBError("get state", err)
	}
	return rec, nil
}

// ListByMachine lists one machine-version's states, ordered by key
// (WBS 6.20).
func (r *WorkflowStateRepository) ListByMachine(ctx context.Context, machineID uuid.UUID, version int) ([]*persistence.WorkflowStateRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+workflowStateColumns+` FROM workflow_states WHERE machine_id = $1 AND version = $2 ORDER BY key`, machineID, version)
	if err != nil {
		return nil, TranslateDBError("list states", err)
	}
	defer rows.Close()
	var out []*persistence.WorkflowStateRecord
	for rows.Next() {
		rec, err := scanStateRow(rows)
		if err != nil {
			return nil, TranslateDBError("list states", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list states", err)
	}
	return out, nil
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *WorkflowStateRepository) WithTx(tx persistence.Transaction) persistence.WorkflowStateRepository {
	return &WorkflowStateRepository{exec: txExecutor{tx}}
}

// scanStateRow scans a workflow_states row into a fresh record.
func scanStateRow(row interface{ Scan(dest ...any) error }) (*persistence.WorkflowStateRecord, error) {
	var rec persistence.WorkflowStateRecord
	if err := row.Scan(&rec.ID, &rec.MachineID, &rec.Version, &rec.Key, &rec.Name, &rec.Description, &rec.PromptTemplate, &rec.Kind, &rec.Terminal, &rec.Enabled, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}
