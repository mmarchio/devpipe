package dbrepo

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

const planColumns = `id, phase_id, wbs_version, version, status, created_at, updated_at, kind, amends_plan_id, source_wbs_hash`

// PlanRepository implements execution-plan persistence on PostgreSQL (WBS
// 2.11 domain model; Phase 2 persists plans, generation comes later).
type PlanRepository struct {
	exec executor
}

// NewPlanRepository binds the repository to an existing pool.
func NewPlanRepository(pool *pgxpool.Pool) *PlanRepository {
	return &PlanRepository{exec: poolAdapter{pool}}
}

// NewPlanRepositoryForExecutor binds the repository to any executor that
// satisfies the driver-free surface (a pool adapter, a transaction, or a
// test wrapper) — the same operation set runs on all of them.
func NewPlanRepositoryForExecutor(exec persistence.Executor) *PlanRepository {
	return &PlanRepository{exec: exec}
}

// WithTx returns a copy of the repository that executes its operations on
// the given transaction (WBS 2.21).
func (r *PlanRepository) WithTx(tx persistence.Transaction) *PlanRepository {
	return &PlanRepository{exec: txExecutor{tx}}
}

type scanPlanRow struct {
	id         uuid.UUID
	phaseID    uuid.UUID
	wbsVersion int
	version    int
	status     string
	createdAt  time.Time
	updatedAt  time.Time
	kind       string
	amends     *uuid.UUID
	wbsHash    string
}

func decodePlan(v scanPlanRow) (*wbs.ExecutionPlan, error) {
	s, err := wbs.ParsePlanStatus(v.status)
	if err != nil {
		return nil, err
	}
	return &wbs.ExecutionPlan{
		ID:         v.id,
		PhaseID:    v.phaseID,
		WBSVersion: v.wbsVersion,
		Version:    v.version,
		Status:     s,
		CreatedAt:  v.createdAt,
		UpdatedAt:  v.updatedAt,

		Kind:          v.kind,
		AmendsPlanID:  v.amends,
		SourceWBSHash: v.wbsHash,
	}, nil
}

// NextVersion returns the next plan version for the phase (MAX(version)+1,
// 1 when the phase has no plans) — plan versions are allocated at creation
// (WBS 4.13: the plan's own version is independent of its source WBS
// version).
func (r *PlanRepository) NextVersion(ctx context.Context, phaseID uuid.UUID) (int, error) {
	var maxVersion int
	if err := r.exec.QueryRow(ctx,
		`SELECT coalesce(MAX(version), 0) FROM execution_plans WHERE phase_id = $1`, phaseID).Scan(&maxVersion); err != nil {
		return 0, TranslateDBError("next plan version", err)
	}
	return maxVersion + 1, nil
}

// ListByPhase returns a phase's execution plans in ascending version order
// (WBS 2.19 active-plan lookup).
func (r *PlanRepository) ListByPhase(ctx context.Context, phaseID uuid.UUID) ([]*wbs.ExecutionPlan, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+planColumns+` FROM execution_plans WHERE phase_id = $1 ORDER BY version ASC`, phaseID)
	if err != nil {
		return nil, TranslateDBError("list plans", err)
	}
	defer rows.Close()
	var out []*wbs.ExecutionPlan
	for rows.Next() {
		var v scanPlanRow
		if err := rows.Scan(&v.id, &v.phaseID, &v.wbsVersion, &v.version, &v.status, &v.createdAt, &v.updatedAt, &v.kind, &v.amends, &v.wbsHash); err != nil {
			return nil, TranslateDBError("list plans", err)
		}
		p, err := decodePlan(v)
		if err != nil {
			return nil, TranslateDBError("list plans", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list plans", err)
	}
	return out, nil
}

// Get returns the execution plan with the given id; unknown ids yield
// persistence.ErrNotFound.
// ListByPhaseSet returns the execution plans of the given phase set in ONE
// statement (WBS 4.24 N+1 avoidance): rows of every phase in the set, in
// ascending version order per phase. An empty set yields no rows.
func (r *PlanRepository) ListByPhaseSet(ctx context.Context, phaseIDs []uuid.UUID) ([]*wbs.ExecutionPlan, error) {
	if len(phaseIDs) == 0 {
		return nil, nil
	}
	rows, err := r.exec.Query(ctx,
		`SELECT `+planColumns+` FROM execution_plans WHERE phase_id = ANY($1) ORDER BY phase_id, version ASC`, phaseIDs)
	if err != nil {
		return nil, TranslateDBError("list plans", err)
	}
	defer rows.Close()
	var out []*wbs.ExecutionPlan
	for rows.Next() {
		var v scanPlanRow
		if err := rows.Scan(&v.id, &v.phaseID, &v.wbsVersion, &v.version, &v.status, &v.createdAt, &v.updatedAt, &v.kind, &v.amends, &v.wbsHash); err != nil {
			return nil, TranslateDBError("list plans", err)
		}
		p, err := decodePlan(v)
		if err != nil {
			return nil, TranslateDBError("list plans", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list plans", err)
	}
	return out, nil
}

func (r *PlanRepository) Get(ctx context.Context, id uuid.UUID) (*wbs.ExecutionPlan, error) {
	var v scanPlanRow
	err := r.exec.QueryRow(ctx, `SELECT `+planColumns+` FROM execution_plans WHERE id = $1`, id).
		Scan(&v.id, &v.phaseID, &v.wbsVersion, &v.version, &v.status, &v.createdAt, &v.updatedAt, &v.kind, &v.amends, &v.wbsHash)
	if err != nil {
		return nil, TranslateDBError("get plan", err)
	}
	return decodePlan(v)
}

// Create inserts an execution plan (WBS 2.20): the plan records the WBS
// version it was derived from and its own version; duplicate (phase,
// version) yields persistence.ErrConflict.
func (r *PlanRepository) Create(ctx context.Context, p *wbs.ExecutionPlan) error {
	if p == nil || p.ID == uuid.Nil || p.PhaseID == uuid.Nil {
		return persistence.ErrInvalidReference
	}
	if p.WBSVersion <= 0 || p.Version <= 0 {
		return persistence.ErrInvalidReference
	}
	row := r.exec.QueryRow(ctx,
		`INSERT INTO execution_plans (id, phase_id, wbs_version, version, status, created_at, updated_at, kind, amends_plan_id, source_wbs_hash)
		 VALUES ($1, $2, $3, $4, $5, $6, now(), $7, $8, $9) RETURNING `+planColumns,
		p.ID, p.PhaseID, p.WBSVersion, p.Version, p.Status.String(), p.CreatedAt.UTC(), planKind(p.Kind), p.AmendsPlanID, p.SourceWBSHash)
	var v scanPlanRow
	if err := row.Scan(&v.id, &v.phaseID, &v.wbsVersion, &v.version, &v.status, &v.createdAt, &v.updatedAt, &v.kind, &v.amends, &v.wbsHash); err != nil {
		return TranslateDBError("create plan", err)
	}
	got, err := decodePlan(v)
	if err != nil {
		return TranslateDBError("create plan", err)
	}
	*p = *got
	return nil
}

func planKind(k string) string {
	if k == "" {
		return wbs.PlanKindInitial
	}
	return k
}

// Update persists status changes and refreshes UpdatedAt (WBS 2.23).
func (r *PlanRepository) Update(ctx context.Context, p *wbs.ExecutionPlan) error {
	tag, err := r.exec.Exec(ctx,
		`UPDATE execution_plans SET status = $2, updated_at = now() WHERE id = $1`,
		p.ID, p.Status.String())
	if err != nil {
		return TranslateDBError("update plan", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrNotFound
	}
	return nil
}
