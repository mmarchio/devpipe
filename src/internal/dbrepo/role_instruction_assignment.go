package dbrepo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const assignmentColumns = `id, role_id, instruction_id, position, created_at`

// RoleInstructionAssignmentRepository implements ordered role → instruction
// assignments on PostgreSQL (WBS 5.9).
type RoleInstructionAssignmentRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.RoleInstructionAssignmentRepository = (*RoleInstructionAssignmentRepository)(nil)

// NewRoleInstructionAssignmentRepository binds the repository to an existing pool.
func NewRoleInstructionAssignmentRepository(pool *pgxpool.Pool) *RoleInstructionAssignmentRepository {
	return &RoleInstructionAssignmentRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *RoleInstructionAssignmentRepository) WithTx(tx persistence.Transaction) persistence.RoleInstructionAssignmentRepository {
	return &RoleInstructionAssignmentRepository{exec: txExecutor{tx}}
}

func scanAssignmentRow(row interface{ Scan(dest ...any) error }) (*persistence.RoleInstructionAssignmentRecord, error) {
	var rec persistence.RoleInstructionAssignmentRecord
	if err := row.Scan(&rec.ID, &rec.RoleID, &rec.InstructionID, &rec.Position, &rec.CreatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Assign links instruction to role at the next deterministic position
// (MAX(position)+1, 1 when none) under a role-scoped advisory lock so
// concurrent assigns get distinct positions (T5.9.2/T5.9.3). A duplicate
// (role, instruction) yields ErrConflict (T5.9.4).
func (r *RoleInstructionAssignmentRepository) Assign(ctx context.Context, roleID, instructionID uuid.UUID) (*persistence.RoleInstructionAssignmentRecord, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		rec, err := r.assignOnce(ctx, roleID, instructionID)
		if err == nil {
			return rec, nil
		}
		if !errors.Is(err, persistence.ErrConflict) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (r *RoleInstructionAssignmentRepository) assignOnce(ctx context.Context, roleID, instructionID uuid.UUID) (*persistence.RoleInstructionAssignmentRecord, error) {
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin assignment", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "role-assignments:"+roleID.String()); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var maxPos int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(MAX(position), 0) FROM role_instruction_assignments WHERE role_id = $1`, roleID).Scan(&maxPos); err != nil {
		return nil, TranslateDBError("max assignment position", err)
	}
	rec := &persistence.RoleInstructionAssignmentRecord{
		ID: uuid.New(), RoleID: roleID, InstructionID: instructionID, Position: maxPos + 1,
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO role_instruction_assignments (id, role_id, instruction_id, position)
		 VALUES ($1, $2, $3, $4) RETURNING `+assignmentColumns,
		rec.ID, roleID, instructionID, rec.Position)
	if err := row.Scan(&rec.ID, &rec.RoleID, &rec.InstructionID, &rec.Position, &rec.CreatedAt); err != nil {
		return nil, TranslateDBError("insert assignment", err)
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit assignment", err)
		}
	}
	return rec, nil
}

func (r *RoleInstructionAssignmentRepository) beginTx(ctx context.Context) (persistence.Transaction, bool, error) {
	if te, ok := r.exec.(txExecutor); ok {
		return te.tx, true, nil
	}
	if b, ok := r.exec.(persistence.TxBeginner); ok {
		tx, err := b.Begin(ctx)
		if err != nil {
			return nil, false, err
		}
		return tx, false, nil
	}
	return nil, false, &persistence.InfrastructureError{Op: "assignment", Cause: errors.New("executor cannot begin transactions")}
}

// ListByInstruction returns the assignments referencing an instruction
// (WBS 5.23 T5.23.5).
func (r *RoleInstructionAssignmentRepository) ListByInstruction(ctx context.Context, instructionID uuid.UUID) ([]*persistence.RoleInstructionAssignmentRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+assignmentColumns+` FROM role_instruction_assignments WHERE instruction_id = $1 ORDER BY position ASC`, instructionID)
	if err != nil {
		return nil, TranslateDBError("list assignments by instruction", err)
	}
	defer rows.Close()
	var out []*persistence.RoleInstructionAssignmentRecord
	for rows.Next() {
		rec, err := scanAssignmentRow(rows)
		if err != nil {
			return nil, TranslateDBError("list assignments by instruction", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list assignments by instruction", err)
	}
	return out, nil
}

// Unassign removes the assignment row; the instruction row is retained
// (T5.9.5 — ON DELETE CASCADE deletes only the assignment).
func (r *RoleInstructionAssignmentRepository) Unassign(ctx context.Context, roleID, instructionID uuid.UUID) error {
	tag, err := r.exec.Exec(ctx,
		`DELETE FROM role_instruction_assignments WHERE role_id = $1 AND instruction_id = $2`,
		roleID, instructionID)
	if err != nil {
		return TranslateDBError("unassign instruction", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// ListByRole returns the role's assignments in position order (T5.9.3).
func (r *RoleInstructionAssignmentRepository) ListByRole(ctx context.Context, roleID uuid.UUID) ([]*persistence.RoleInstructionAssignmentRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+assignmentColumns+` FROM role_instruction_assignments WHERE role_id = $1 ORDER BY position ASC`, roleID)
	if err != nil {
		return nil, TranslateDBError("list assignments", err)
	}
	defer rows.Close()
	var out []*persistence.RoleInstructionAssignmentRecord
	for rows.Next() {
		rec, err := scanAssignmentRow(rows)
		if err != nil {
			return nil, TranslateDBError("list assignments", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list assignments", err)
	}
	return out, nil
}
