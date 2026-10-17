package dbrepo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const instructionColumns = `id, project_id, key, name, description, content, version, enabled, created_at, updated_at`

// InstructionRepository implements persistence.InstructionRepository on
// PostgreSQL (WBS 5.7).
type InstructionRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.InstructionRepository = (*InstructionRepository)(nil)

// NewInstructionRepository binds the repository to an existing pool.
func NewInstructionRepository(pool *pgxpool.Pool) *InstructionRepository {
	return &InstructionRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
// Delete removes the instruction row (WBS 5.23); ErrNotFound when missing.
func (r *InstructionRepository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.exec.Exec(ctx, `DELETE FROM instructions WHERE id = $1`, id)
	if err != nil {
		return TranslateDBError("delete instruction", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

func (r *InstructionRepository) WithTx(tx persistence.Transaction) persistence.InstructionRepository {
	return &InstructionRepository{exec: txExecutor{tx}}
}

func scanInstructionRow(row interface{ Scan(dest ...any) error }) (*persistence.InstructionRecord, error) {
	var (
		rec    persistence.InstructionRecord
		projID *uuid.UUID
	)
	if err := row.Scan(&rec.ID, &projID, &rec.Key, &rec.Name, &rec.Description, &rec.Content, &rec.Version, &rec.Enabled, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	rec.ProjectID = projID
	return &rec, nil
}

// Create inserts a new instruction (WBS 5.7); a scoped key duplicate yields
// persistence.ErrConflict.
func (r *InstructionRepository) Create(ctx context.Context, rec *persistence.InstructionRecord) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO instructions (id, project_id, key, name, description, content, version, enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rec.ID, rec.ProjectID, rec.Key, rec.Name, rec.Description, rec.Content, rec.Version, rec.Enabled, rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create instruction", err)
	}
	return nil
}

// Get returns the instruction with the given id; ErrNotFound when missing.
func (r *InstructionRepository) Get(ctx context.Context, id string) (*persistence.InstructionRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+instructionColumns+` FROM instructions WHERE id = $1`, id)
	rec, err := scanInstructionRow(row)
	if err != nil {
		return nil, TranslateDBError("get instruction", err)
	}
	return rec, nil
}

// ListByScope lists all instructions in one scope (nil = global), ordered by key.
func (r *InstructionRepository) ListByScope(ctx context.Context, projectID *uuid.UUID) ([]*persistence.InstructionRecord, error) {
	var (
		rows persistence.Rows
		err  error
	)
	if projectID == nil {
		rows, err = r.exec.Query(ctx, `SELECT `+instructionColumns+` FROM instructions WHERE project_id IS NULL ORDER BY key`)
	} else {
		rows, err = r.exec.Query(ctx, `SELECT `+instructionColumns+` FROM instructions WHERE project_id = $1 ORDER BY key`, *projectID)
	}
	if err != nil {
		return nil, TranslateDBError("list instructions by scope", err)
	}
	defer rows.Close()
	var out []*persistence.InstructionRecord
	for rows.Next() {
		rec, err := scanInstructionRow(rows)
		if err != nil {
			return nil, TranslateDBError("list instructions by scope", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list instructions by scope", err)
	}
	return out, nil
}
