// P6-27 (WBS 6.37): the transition-condition persistence (T6.37.2): the
// structured condition attached to a transition — one per transition,
// the P6-04 domain shape (field, operator, value).
package dbrepo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

// TransitionConditionRepository implements persistence.TransitionConditionRepository
// on PostgreSQL (WBS 6.37).
type TransitionConditionRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.TransitionConditionRepository = (*TransitionConditionRepository)(nil)

// NewTransitionConditionRepository binds the repository to an existing pool.
func NewTransitionConditionRepository(pool *pgxpool.Pool) *TransitionConditionRepository {
	return &TransitionConditionRepository{exec: poolAdapter{pool}}
}

// Create stores the transition's condition, replacing any prior one
// (T6.37.2): an upsert on the transition's primary key. A missing
// transition row yields ErrInvalidReference.
func (r *TransitionConditionRepository) Create(ctx context.Context, transitionID uuid.UUID, c *persistence.TransitionCondition) error {
	var literal []byte
	var cf sql.NullString
	if c.Literal != nil {
		b, err := json.Marshal(c.Literal)
		if err != nil {
			return TranslateDBError("encode condition literal", err)
		}
		literal = b
	}
	if c.ContextField != "" {
		cf = sql.NullString{String: c.ContextField, Valid: true}
	}
	_, err := r.exec.Exec(ctx,
		`INSERT INTO transition_conditions (transition_id, field, operator, literal, context_field, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, now(), now())
		 ON CONFLICT (transition_id)
		 DO UPDATE SET field = EXCLUDED.field, operator = EXCLUDED.operator,
		               literal = EXCLUDED.literal, context_field = EXCLUDED.context_field,
		               updated_at = now()`,
		transitionID, c.Field, string(c.Operator), literal, cf)
	if err != nil {
		return TranslateDBError("create transition condition", err)
	}
	return nil
}

// Get returns the transition's condition; nil, nil when it has none,
// ErrNotFound when the transition row is missing.
func (r *TransitionConditionRepository) Get(ctx context.Context, transitionID uuid.UUID) (*persistence.TransitionCondition, bool, error) {
	var (
		id       uuid.UUID
		field    string
		operator string
		literal  []byte
		contextF sql.NullString
	)
	err := r.exec.QueryRow(ctx,
		`SELECT transition_id, field, operator, literal, context_field FROM transition_conditions WHERE transition_id = $1`,
		transitionID).Scan(&id, &field, &operator, &literal, &contextF)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Distinguish "no condition" from "no transition": probe the
			// transition table.
			var exists bool
			if e := r.exec.QueryRow(ctx,
				`SELECT true FROM state_transitions WHERE id = $1`, transitionID).Scan(&exists); e != nil || !exists {
				if e != nil {
					return nil, false, TranslateDBError("check transition existence", e)
				}
				return nil, false, persistence.ErrNotFound
			}
			return nil, false, nil
		}
		return nil, false, TranslateDBError("get transition condition", err)
	}
	c := &persistence.TransitionCondition{
		Field:    field,
		Operator: operator,
	}
	if contextF.Valid {
		c.ContextField = contextF.String
	}
	if literal != nil {
		if e := json.Unmarshal(literal, &c.Literal); e != nil {
			return nil, false, TranslateDBError("decode condition literal", e)
		}
	}
	return c, true, nil
}

// Remove deletes the transition's condition (clears it). A missing
// condition row is a no-op.
func (r *TransitionConditionRepository) Remove(ctx context.Context, transitionID uuid.UUID) error {
	_, err := r.exec.Exec(ctx,
		`DELETE FROM transition_conditions WHERE transition_id = $1`, transitionID)
	if err != nil {
		return TranslateDBError("remove transition condition", err)
	}
	return nil
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *TransitionConditionRepository) WithTx(tx persistence.Transaction) persistence.TransitionConditionRepository {
	return &TransitionConditionRepository{exec: txExecutor{tx}}
}
