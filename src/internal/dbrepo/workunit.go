package dbrepo

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

const workUnitColumns = `id, execution_plan_id, project_id, sequence, name, description, kind, status,
	objective, implementation_instructions, acceptance_criteria, required_tests, constraints,
	created_at, updated_at, started_at, completed_at,
	unit_key, validation_commands, acceptance_checks, paths, carried_from_unit_id, claimed_execution_id, done_validation_run_id`

// WorkUnitRepository implements work-unit persistence on PostgreSQL (WBS 2.16).
// Transition enforcement belongs to a later state-machine phase; the marks
// here only persist status and timestamps.
// workUnitColumnsAliased is workUnitColumns prefixed for joined queries.
const workUnitColumnsAliased = `wu.id, wu.execution_plan_id, wu.project_id, wu.sequence, wu.name, wu.description, wu.kind, wu.status,
	wu.objective, wu.implementation_instructions, wu.acceptance_criteria, wu.required_tests, wu.constraints,
	wu.created_at, wu.updated_at, wu.started_at, wu.completed_at,
	wu.unit_key, wu.validation_commands, wu.acceptance_checks, wu.paths, wu.carried_from_unit_id, wu.claimed_execution_id,
	wu.done_validation_run_id`

type WorkUnitRepository struct {
	exec executor
}

// NewWorkUnitRepository binds the repository to an existing pool.
func NewWorkUnitRepository(pool *pgxpool.Pool) *WorkUnitRepository {
	return &WorkUnitRepository{exec: poolAdapter{pool}}
}

// NewWorkUnitRepositoryForExecutor binds the repository to any executor
// that satisfies the driver-free surface (a pool adapter, a transaction,
// or a test wrapper) — the same operation set runs on all of them.
func NewWorkUnitRepositoryForExecutor(exec persistence.Executor) *WorkUnitRepository {
	return &WorkUnitRepository{exec: exec}
}

// WithTx returns a copy of the repository that executes its operations on
// the given transaction (WBS 2.21).
func (r *WorkUnitRepository) WithTx(tx persistence.Transaction) *WorkUnitRepository {
	return &WorkUnitRepository{exec: txExecutor{tx}}
}

// scanWorkUnitScan is the scan target for one work_units row: all columns
// land in fresh local variables, so a nil WorkUnit.Specification pointer can
// never be dereferenced mid-scan.
type scanWorkUnitScan struct {
	id          uuid.UUID
	planID      uuid.UUID
	projectID   uuid.UUID
	sequence    int
	name        string
	desc        string
	kind        string
	status      string
	objective   string
	impl        string
	accJSON     []byte
	reqJSON     []byte
	conJSON     []byte
	createdAt   time.Time
	updatedAt   time.Time
	startedAt   *time.Time
	completedAt *time.Time
	// Phase 16 columns (P16-58).
	key         string
	valJSON     []byte
	checksJSON  []byte
	pathsJSON   []byte
	carriedFrom *uuid.UUID
	claimedBy   *uuid.UUID
	doneRun     *uuid.UUID
}

// scanWorkUnit scans a row (pgx.Row or pgx.Rows) and decodes it into a
// fully populated WorkUnit, including its specification (WBS 2.13).
func scanWorkUnit(row interface {
	Scan(...any) error
}) (*wbs.WorkUnit, error) {
	var v scanWorkUnitScan
	if err := row.Scan(&v.id, &v.planID, &v.projectID, &v.sequence, &v.name, &v.desc,
		&v.kind, &v.status, &v.objective, &v.impl,
		&v.accJSON, &v.reqJSON, &v.conJSON, &v.createdAt, &v.updatedAt, &v.startedAt, &v.completedAt,
		&v.key, &v.valJSON, &v.checksJSON, &v.pathsJSON, &v.carriedFrom, &v.claimedBy, &v.doneRun); err != nil {
		return nil, err
	}
	k, err := wbs.ParseWorkUnitKind(v.kind)
	if err != nil {
		return nil, err
	}
	s, err := wbs.ParseWorkUnitStatus(v.status)
	if err != nil {
		return nil, err
	}
	spec := &wbs.WorkUnitSpecification{
		Objective:                  v.objective,
		ImplementationInstructions: v.impl,
	}
	if len(v.accJSON) > 0 {
		if err := json.Unmarshal(v.accJSON, &spec.AcceptanceCriteria); err != nil {
			return nil, err
		}
	}
	if len(v.reqJSON) > 0 {
		if err := json.Unmarshal(v.reqJSON, &spec.RequiredTests); err != nil {
			return nil, err
		}
	}
	if len(v.conJSON) > 0 {
		if err := json.Unmarshal(v.conJSON, &spec.Constraints); err != nil {
			return nil, err
		}
	}
	spec.Key = v.key
	for _, x := range []struct {
		b   []byte
		dst any
	}{{v.valJSON, &spec.Validation}, {v.checksJSON, &spec.AcceptanceChecks}, {v.pathsJSON, &spec.Paths}} {
		if len(x.b) > 0 && string(x.b) != "[]" {
			if err := json.Unmarshal(x.b, x.dst); err != nil {
				return nil, err
			}
		}
	}
	return &wbs.WorkUnit{
		CarriedFromUnitID:   v.carriedFrom,
		ClaimedExecutionID:  v.claimedBy,
		DoneValidationRunID: v.doneRun,
		ID:                  v.id,
		ExecutionPlanID:     v.planID,
		ProjectID:           v.projectID,
		Sequence:            v.sequence,
		Name:                v.name,
		Description:         v.desc,
		Kind:                k,
		Status:              s,
		Specification:       spec,
		CreatedAt:           v.createdAt,
		UpdatedAt:           v.updatedAt,
		StartedAt:           v.startedAt,
		CompletedAt:         v.completedAt,
	}, nil
}

// phase16SpecJSON encodes the Phase 16 specification lists (nil → []).
func phase16SpecJSON(spec *wbs.WorkUnitSpecification) (val, checks, paths []byte, err error) {
	v := spec.Validation
	if v == nil {
		v = []wbs.ValidationSpec{}
	}
	c := spec.AcceptanceChecks
	if c == nil {
		c = []wbs.AcceptanceCheckSpec{}
	}
	if val, err = json.Marshal(v); err != nil {
		return
	}
	if checks, err = json.Marshal(c); err != nil {
		return
	}
	paths, err = specJSON(spec.Paths)
	return
}

// specJSON renders a string list for JSONB storage; nil lists encode as [].
func specJSON(list []string) ([]byte, error) {
	if list == nil {
		list = []string{}
	}
	return json.Marshal(list)
}

// Create inserts a work unit (T2.16.1). Duplicate (plan, sequence) yields
// persistence.ErrConflict.
func (r *WorkUnitRepository) Create(ctx context.Context, u *wbs.WorkUnit) error {
	spec := u.Specification
	if spec == nil {
		spec = &wbs.WorkUnitSpecification{}
	}
	acc, err := specJSON(spec.AcceptanceCriteria)
	if err != nil {
		return err
	}
	req, err := specJSON(spec.RequiredTests)
	if err != nil {
		return err
	}
	con, err := specJSON(spec.Constraints)
	if err != nil {
		return err
	}
	// ProjectID is backfilled from the plan's phase at persistence time
	// when the caller leaves it zero (P6-30: the runtime-state read
	// resolves work-unit associations project-scoped).
	projectID := u.ProjectID
	if projectID == uuid.Nil {
		var pid uuid.UUID
		if err := r.exec.QueryRow(ctx,
			`SELECT p.project_id FROM execution_plans ep JOIN phases p ON p.id = ep.phase_id WHERE ep.id = $1`, u.ExecutionPlanID).Scan(&pid); err != nil {
			return TranslateDBError("create work unit", err)
		}
		projectID = pid
	}
	val, checks, paths, err := phase16SpecJSON(spec)
	if err != nil {
		return err
	}
	_, err = r.exec.Exec(ctx,
		`INSERT INTO work_units (id, execution_plan_id, project_id, sequence, name, description, kind, status,
			objective, implementation_instructions, acceptance_criteria, required_tests, constraints, created_at, updated_at,
			unit_key, validation_commands, acceptance_checks, paths, carried_from_unit_id, completed_at, done_validation_run_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)`,
		u.ID, u.ExecutionPlanID, projectID, u.Sequence, u.Name, u.Description, u.Kind.String(), u.Status.String(),
		spec.Objective, spec.ImplementationInstructions, acc, req, con, u.CreatedAt.UTC(), u.CreatedAt.UTC(),
		spec.Key, val, checks, paths, u.CarriedFromUnitID, u.CompletedAt, u.DoneValidationRunID)
	if err != nil {
		return TranslateDBError("create work unit", err)
	}
	return nil
}

// Get returns the work unit with the given id (T2.16.1); unknown ids yield
// persistence.ErrNotFound.
func (r *WorkUnitRepository) Get(ctx context.Context, id uuid.UUID) (*wbs.WorkUnit, error) {
	row := r.exec.QueryRow(ctx, `SELECT `+workUnitColumns+` FROM work_units WHERE id = $1`, id)
	u, err := scanWorkUnit(row)
	if err != nil {
		return nil, TranslateDBError("get work unit", err)
	}
	return u, nil
}

// Update persists mutable field changes (WBS 2.16). ID, ExecutionPlanID, and
// Sequence are immutable by design.
func (r *WorkUnitRepository) Update(ctx context.Context, u *wbs.WorkUnit) error {
	spec := u.Specification
	if spec == nil {
		spec = &wbs.WorkUnitSpecification{}
	}
	acc, err := specJSON(spec.AcceptanceCriteria)
	if err != nil {
		return err
	}
	req, err := specJSON(spec.RequiredTests)
	if err != nil {
		return err
	}
	con, err := specJSON(spec.Constraints)
	if err != nil {
		return err
	}
	tag, err := r.exec.Exec(ctx,
		`UPDATE work_units SET name = $2, description = $3, kind = $4, status = $5,
			objective = $6, implementation_instructions = $7,
			acceptance_criteria = $8, required_tests = $9, constraints = $10,
		 updated_at = now()
		 WHERE id = $1`,
		u.ID, u.Name, u.Description, u.Kind.String(), u.Status.String(),
		spec.Objective, spec.ImplementationInstructions, acc, req, con)
	if err != nil {
		return TranslateDBError("update work unit", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// ListByPlan returns the plan's work units in ascending sequence order
// (T2.16.2).
func (r *WorkUnitRepository) ListByPlan(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+workUnitColumns+` FROM work_units WHERE execution_plan_id = $1 ORDER BY sequence ASC`, planID)
	if err != nil {
		return nil, TranslateDBError("list work units", err)
	}
	defer rows.Close()
	var out []*wbs.WorkUnit
	for rows.Next() {
		u, err := scanWorkUnit(rows)
		if err != nil {
			return nil, TranslateDBError("list work units", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list work units", err)
	}
	return out, nil
}

// ListByPlanSet returns the work units of the given plan set in ONE
// statement (WBS 4.24 N+1 avoidance): rows of every plan in the set, in
// ascending sequence order per plan. An empty set yields no rows.
func (r *WorkUnitRepository) ListByPlanSet(ctx context.Context, planIDs []uuid.UUID) ([]*wbs.WorkUnit, error) {
	if len(planIDs) == 0 {
		return nil, nil
	}
	rows, err := r.exec.Query(ctx,
		`SELECT `+workUnitColumns+` FROM work_units WHERE execution_plan_id = ANY($1) ORDER BY execution_plan_id, sequence ASC`, planIDs)
	if err != nil {
		return nil, TranslateDBError("list work units", err)
	}
	defer rows.Close()
	var out []*wbs.WorkUnit
	for rows.Next() {
		u, err := scanWorkUnit(rows)
		if err != nil {
			return nil, TranslateDBError("list work units", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list work units", err)
	}
	return out, nil
}

// ListEligible returns the plan's structurally eligible work units (WBS
// 2.17): status PENDING/READY and every dependency status DONE. Results are
// in ascending sequence order.
func (r *WorkUnitRepository) ListEligible(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+workUnitColumnsAliased+`
		 FROM work_units wu
		 WHERE wu.execution_plan_id = $1
		   AND wu.status IN ('PENDING','READY')
		   AND NOT EXISTS (
			     SELECT 1 FROM work_unit_dependencies d
			     JOIN work_units dep ON dep.id = d.depends_on_work_unit_id
			     WHERE d.work_unit_id = wu.id AND dep.status <> 'DONE')
		 ORDER BY wu.sequence ASC`, planID)
	if err != nil {
		return nil, TranslateDBError("list eligible work units", err)
	}
	defer rows.Close()
	var out []*wbs.WorkUnit
	for rows.Next() {
		u, err := scanWorkUnit(rows)
		if err != nil {
			return nil, TranslateDBError("list eligible work units", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list eligible work units", err)
	}
	return out, nil
}

// AddDependency records that unit dependsOn must complete before unit
// (WBS 2.14, T2.14.1–T2.14.5). Self-dependencies and cross-plan edges are
// rejected; duplicates yield persistence.ErrConflict.
func (r *WorkUnitRepository) AddDependency(ctx context.Context, unit, dependsOn uuid.UUID) error {
	if unit == dependsOn {
		return persistence.ErrConflict
	}
	u, err := r.Get(ctx, unit)
	if err != nil {
		return err
	}
	d, err := r.Get(ctx, dependsOn)
	if err != nil {
		return err
	}
	if u.ExecutionPlanID != d.ExecutionPlanID {
		return persistence.ErrInvalidReference
	}
	_, err = r.exec.Exec(ctx,
		`INSERT INTO work_unit_dependencies (work_unit_id, depends_on_work_unit_id) VALUES ($1, $2)`,
		unit, dependsOn)
	if err != nil {
		return TranslateDBError("add work unit dependency", err)
	}
	return nil
}

// GetDependencies returns the units that the given unit depends on
// (T2.14.6 / T2.16.6), in stable order by unit sequence.
func (r *WorkUnitRepository) GetDependencies(ctx context.Context, unitID uuid.UUID) ([]*wbs.WorkUnit, error) {
	return r.relatedUnits(ctx, unitID, "work_unit_id", "depends_on_work_unit_id")
}

// GetDependents returns the units that depend on the given unit
// (T2.16.7), in stable order by unit sequence.
func (r *WorkUnitRepository) GetDependents(ctx context.Context, unitID uuid.UUID) ([]*wbs.WorkUnit, error) {
	return r.relatedUnits(ctx, unitID, "depends_on_work_unit_id", "work_unit_id")
}

func (r *WorkUnitRepository) relatedUnits(ctx context.Context, unitID uuid.UUID, fromCol, toCol string) ([]*wbs.WorkUnit, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+workUnitColumnsAliased+`
		 FROM work_units wu
		 JOIN work_unit_dependencies d ON d.`+toCol+` = wu.id
		 WHERE d.`+fromCol+` = $1
		 ORDER BY wu.sequence ASC`, unitID)
	if err != nil {
		return nil, TranslateDBError("related work units", err)
	}
	defer rows.Close()
	var out []*wbs.WorkUnit
	for rows.Next() {
		u, err := scanWorkUnit(rows)
		if err != nil {
			return nil, TranslateDBError("related work units", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("related work units", err)
	}
	return out, nil
}

// MarkReady records the PENDING -> READY transition (T2.16.3).
func (r *WorkUnitRepository) MarkReady(ctx context.Context, id uuid.UUID) error {
	return r.markStatus(ctx, "mark work unit ready", id, "READY")
}

// MarkExecuting records the transition to EXECUTING and sets StartedAt once
// (T2.16.4).
func (r *WorkUnitRepository) MarkExecuting(ctx context.Context, id uuid.UUID) error {
	tag, err := r.exec.Exec(ctx,
		`UPDATE work_units SET status = 'EXECUTING', started_at = COALESCE(started_at, now()), updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return TranslateDBError("mark work unit executing", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

// MarkDone records the transition to DONE and sets CompletedAt once
// (T2.16.5).
func (r *WorkUnitRepository) MarkDone(ctx context.Context, id uuid.UUID) error {
	return r.markCompleted(ctx, "mark work unit done", id, "DONE")
}

// MarkFailed records the transition to FAILED and sets CompletedAt once
// (T2.16.5).
func (r *WorkUnitRepository) MarkFailed(ctx context.Context, id uuid.UUID) error {
	return r.markCompleted(ctx, "mark work unit failed", id, "FAILED")
}

func (r *WorkUnitRepository) markStatus(ctx context.Context, op string, id uuid.UUID, status string) error {
	tag, err := r.exec.Exec(ctx, `UPDATE work_units SET status = $2 WHERE id = $1`, id, status)
	if err != nil {
		return TranslateDBError(op, err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}

func (r *WorkUnitRepository) markCompleted(ctx context.Context, op string, id uuid.UUID, status string) error {
	tag, err := r.exec.Exec(ctx,
		`UPDATE work_units SET status = $2, completed_at = COALESCE(completed_at, now()), updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return TranslateDBError(op, err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}
