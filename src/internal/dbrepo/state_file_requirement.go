package dbrepo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const stateFileRequirementColumns = `id, state_id, category, ordinal, created_at`

// StateFileRequirementRepository implements per-state required state-file
// categories on PostgreSQL (WBS 6.6), mirroring the P6-07 ordered-list
// pattern: ordinal allocation under a state-scoped advisory lock with
// bounded retry on the UNIQUE (state_id, category) constraint.
type StateFileRequirementRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.StateFileRequirementRepository = (*StateFileRequirementRepository)(nil)

// NewStateFileRequirementRepository binds the repository to an existing pool.
func NewStateFileRequirementRepository(pool *pgxpool.Pool) *StateFileRequirementRepository {
	return &StateFileRequirementRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *StateFileRequirementRepository) WithTx(tx persistence.Transaction) persistence.StateFileRequirementRepository {
	return &StateFileRequirementRepository{exec: txExecutor{tx}}
}

func scanStateFileRequirementRow(row interface{ Scan(dest ...any) error }) (*persistence.StateFileRequirementRecord, error) {
	var rec persistence.StateFileRequirementRecord
	if err := row.Scan(&rec.ID, &rec.StateID, &rec.Category, &rec.Ordinal, &rec.CreatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Require records a required state-file category at the next deterministic
// ordinal (MAX(ordinal)+1, 0 when none) under a state-scoped advisory lock
// so concurrent requires get distinct ordinals. A duplicate (state, category)
// yields ErrConflict (T6.6.3); a missing state yields ErrInvalidReference.
func (r *StateFileRequirementRepository) Require(ctx context.Context, stateID uuid.UUID, category string) (*persistence.StateFileRequirementRecord, error) {
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		rec, err := r.requireOnce(ctx, stateID, category)
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

func (r *StateFileRequirementRepository) requireOnce(ctx context.Context, stateID uuid.UUID, category string) (*persistence.StateFileRequirementRecord, error) {
	tx, joined, err := r.beginTx(ctx)
	if err != nil {
		return nil, TranslateDBError("begin state-file requirement", err)
	}
	defer func() {
		if !joined {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "state-file-requirements:"+stateID.String()); err != nil {
		return nil, TranslateDBError("advisory lock", err)
	}
	var maxOrdinal int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(MAX(ordinal), -1) FROM state_file_requirements WHERE state_id = $1`, stateID).Scan(&maxOrdinal); err != nil {
		return nil, TranslateDBError("max state-file requirement ordinal", err)
	}
	rec := &persistence.StateFileRequirementRecord{
		ID: uuid.New(), StateID: stateID, Category: category, Ordinal: maxOrdinal + 1,
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO state_file_requirements (id, state_id, category, ordinal)
		 VALUES ($1, $2, $3, $4) RETURNING `+stateFileRequirementColumns,
		rec.ID, stateID, rec.Category, rec.Ordinal)
	if err := row.Scan(&rec.ID, &rec.StateID, &rec.Category, &rec.Ordinal, &rec.CreatedAt); err != nil {
		return nil, TranslateDBError("insert state-file requirement", err)
	}
	if !joined {
		if err := tx.Commit(ctx); err != nil {
			return nil, TranslateDBError("commit state-file requirement", err)
		}
	}
	return rec, nil
}

func (r *StateFileRequirementRepository) beginTx(ctx context.Context) (persistence.Transaction, bool, error) {
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
	return nil, false, &persistence.InfrastructureError{Op: "state-file requirement", Cause: errors.New("executor cannot begin transactions")}
}

// ListByState returns the state's required state-file categories in ordinal
// order (deterministic, T6.6.4).
func (r *StateFileRequirementRepository) ListByState(ctx context.Context, stateID uuid.UUID) ([]*persistence.StateFileRequirementRecord, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+stateFileRequirementColumns+` FROM state_file_requirements WHERE state_id = $1 ORDER BY ordinal ASC`, stateID)
	if err != nil {
		return nil, TranslateDBError("list state-file requirements", err)
	}
	defer rows.Close()
	var out []*persistence.StateFileRequirementRecord
	for rows.Next() {
		rec, err := scanStateFileRequirementRow(rows)
		if err != nil {
			return nil, TranslateDBError("list state-file requirements", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list state-file requirements", err)
	}
	return out, nil
}
