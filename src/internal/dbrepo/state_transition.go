package dbrepo

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
)

const transitionColumns = `t.id, t.machine_id, t.version, t.from_state_id, t.to_state_id, t.outcome, t.priority, t.enabled, t.created_at, t.updated_at, tc.field, tc.operator, tc.literal, tc.context_field`

const transitionFromClause = ` FROM state_transitions t
	LEFT JOIN transition_conditions tc ON tc.transition_id = t.id`

// StateTransitionRepository implements persistence.StateTransitionRepository
// on PostgreSQL (WBS 6.8).
type StateTransitionRepository struct {
	exec executor
}

// Compile-time interface check.
var _ persistence.StateTransitionRepository = (*StateTransitionRepository)(nil)

// NewStateTransitionRepository binds the repository to an existing pool.
func NewStateTransitionRepository(pool *pgxpool.Pool) *StateTransitionRepository {
	return &StateTransitionRepository{exec: poolAdapter{pool}}
}

// Create inserts a new transition (WBS 6.8); a duplicate (machine_id,
// from_state_id, outcome) yields ErrConflict, and a transition whose
// endpoints do not both belong to the machine is rejected by the DB-level
// same-machine check (T6.8.4).
func (r *StateTransitionRepository) Create(ctx context.Context, rec *persistence.StateTransitionRecord) error {
	if rec.Version == 0 {
		rec.Version = 1 // a fresh machine's transitions belong to its v1
	}
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO state_transitions (id, machine_id, version, from_state_id, to_state_id, outcome, priority, enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rec.ID, rec.MachineID, rec.Version, rec.FromStateID, rec.ToStateID, rec.Outcome, rec.Priority, rec.Enabled, rec.CreatedAt.UTC(), rec.UpdatedAt.UTC()); err != nil {
		return TranslateDBError("create transition", err)
	}
	return nil
}

// Get returns the transition with the given id; ErrNotFound when missing.
func (r *StateTransitionRepository) Get(ctx context.Context, id uuid.UUID) (*persistence.StateTransitionRecord, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+transitionColumns+transitionFromClause+` WHERE t.id = $1`, id)
	rec, err := scanTransitionRow(row)
	if err != nil {
		return nil, TranslateDBError("get transition", err)
	}
	return rec, nil
}

// ListByMachine lists one machine-version's transitions, ordered by
// (from_state, outcome) (WBS 6.20).
func (r *StateTransitionRepository) ListByMachine(ctx context.Context, machineID uuid.UUID, version int) ([]*persistence.StateTransitionRecord, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+transitionColumns+transitionFromClause+` WHERE t.machine_id = $1 AND t.version = $2 ORDER BY t.from_state_id, t.outcome`, machineID, version)
	if err != nil {
		return nil, TranslateDBError("list transitions", err)
	}
	defer rows.Close()
	var out []*persistence.StateTransitionRecord
	for rows.Next() {
		rec, err := scanTransitionRow(rows)
		if err != nil {
			return nil, TranslateDBError("list transitions", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list transitions", err)
	}
	return out, nil
}

// Update mutates the transition's editable configuration (outcome,
// priority, enabled) in place within the current machine version (WBS
// 6.37); identity fields are immutable. The outcome's unique index applies,
// so an outcome change that collides yields ErrConflict.
func (r *StateTransitionRepository) Update(ctx context.Context, rec *persistence.StateTransitionRecord) (*persistence.StateTransitionRecord, error) {
	res, err := r.exec.Exec(ctx,
		`UPDATE state_transitions SET outcome = $1, priority = $2, enabled = $3, updated_at = $4 WHERE id = $5`,
		rec.Outcome, rec.Priority, rec.Enabled, rec.UpdatedAt.UTC(), rec.ID)
	if err != nil {
		return nil, TranslateDBError("update transition", err)
	}
	if res.RowsAffected() == 0 {
		return nil, persistence.ErrNotFound
	}
	return r.Get(ctx, rec.ID)
}

// WithTx returns a copy bound to the given transaction (WBS 2.21).
func (r *StateTransitionRepository) WithTx(tx persistence.Transaction) persistence.StateTransitionRepository {
	return &StateTransitionRepository{exec: txExecutor{tx}}
}

// scanTransitionRow scans a state_transitions row into a fresh record.
func scanTransitionRow(row interface{ Scan(dest ...any) error }) (*persistence.StateTransitionRecord, error) {
	var (
		rec       persistence.StateTransitionRecord
		cField    sql.NullString
		cOperator sql.NullString
		cLiteral  []byte
		cContextF sql.NullString
	)
	if err := row.Scan(&rec.ID, &rec.MachineID, &rec.Version, &rec.FromStateID, &rec.ToStateID,
		&rec.Outcome, &rec.Priority, &rec.Enabled, &rec.CreatedAt, &rec.UpdatedAt,
		&cField, &cOperator, &cLiteral, &cContextF); err != nil {
		return nil, err
	}
	if cField.Valid || cOperator.Valid || cLiteral != nil || cContextF.Valid {
		cond := &persistence.TransitionCondition{
			Field: cField.String, Operator: cOperator.String,
		}
		if cContextF.Valid {
			cond.ContextField = cContextF.String
		}
		if cLiteral != nil {
			if err := json.Unmarshal(cLiteral, &cond.Literal); err != nil {
				return nil, err
			}
		}
		rec.Condition = cond
	}
	return &rec, nil
}
