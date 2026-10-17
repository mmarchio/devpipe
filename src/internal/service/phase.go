package service

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/wbs"
)

// PhaseService exposes Phase 2 phase persistence through the application
// layer (WBS 3.16): server-assigned append ordering, ordered listing, and
// project-scoped phase access. It translates repository domain errors to
// the service-level set (WBS 3.1).
type PhaseService struct {
	phases   persistence.PhaseRepository
	projects persistence.ProjectRepository
	plans    planLister
	docs     persistence.WBSDocumentRepository
	// units lists a plan's work units for the completion-eligibility
	// check (WBS 4.22).
	units unitLister
	// versions is the WBS version-history repository (WBS 4.28); when
	// nil the initial WBS is stored as the document row only (the
	// versionless fallback).
	versions persistence.WBSVersionRepository
	txPool   *pgxpool.Pool
	// *Tx fields are WithTxSeams overrides (WBS 4.28 test seam): the
	// repository used for the WithTx-bound call when wired; nil keeps
	// the primary repository.
	phasesTx   persistence.PhaseRepository
	projectsTx persistence.ProjectRepository
	docsTx     persistence.WBSDocumentRepository
	versionsTx persistence.WBSVersionRepository
}

// planLister is the minimal contract PhaseService needs from the execution-
// plan repository (WBS 4.6 deletion eligibility).
type planLister interface {
	ListByPhase(ctx context.Context, phaseID uuid.UUID) ([]*wbs.ExecutionPlan, error)
}

// unitLister is the minimal contract PhaseService needs from the work-unit
// repository (WBS 4.22 completion eligibility).
type unitLister interface {
	ListByPlan(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error)
}

// NewPhaseService wires the service to the phase and project repositories,
// the optional plan/document repositories, and the optional transaction
// pool. The pool enables transactional operations (WBS 4.6 deletion removes
// associated WBS data atomically); nil pools/repos degrade to the
// repository foreign-key cascade.
func NewPhaseService(
	phases persistence.PhaseRepository,
	projects persistence.ProjectRepository,
	plans *dbrepo.PlanRepository,
	docs *dbrepo.DocumentRepository,
	units *dbrepo.WorkUnitRepository,
	txPool *pgxpool.Pool,
) *PhaseService {
	var planListerVal planLister
	if plans != nil {
		planListerVal = plans
	}
	var unitsVal unitLister
	if units != nil {
		unitsVal = units
	}
	return &PhaseService{
		phases:   phases,
		projects: projects,
		plans:    planListerVal,
		docs:     documentRepoOrNil(docs),
		units:    unitsVal,
		txPool:   txPool,
	}
}

func documentRepoOrNil(docs *dbrepo.DocumentRepository) persistence.WBSDocumentRepository {
	if docs == nil {
		return nil
	}
	return wbsDocumentAdapter{repo: docs}
}

// WithWBSVersions wires the WBS version-history repository for
// CreatePhaseWithWBS (WBS 4.28): the initial WBS v1 row lands in the
// version history inside the same transaction. Nil leaves the
// versionless fallback in place.
func (s *PhaseService) WithWBSVersions(versions persistence.WBSVersionRepository) *PhaseService {
	s.versions = versions
	return s
}

// WithTxSeams overrides the repositories used INSIDE service transactions
// (WBS 4.28 test seam): a nil argument keeps the existing repository, a
// non-nil one is used for the WithTx-bound call. The override is what lets
// a test force one step of the transaction to fail while the others run
// against the real database.
func (s *PhaseService) WithTxSeams(
	phases persistence.PhaseRepository,
	projects persistence.ProjectRepository,
	docs *dbrepo.DocumentRepository,
	versions persistence.WBSVersionRepository,
) *PhaseService {
	if phases != nil {
		s.phasesTx = phases
	}
	if projects != nil {
		s.projectsTx = projects
	}
	if docs != nil {
		s.docsTx = wbsDocumentAdapter{repo: docs}
	}
	if versions != nil {
		s.versionsTx = versions
	}
	return s
}

// txPhases / txProjects / txDocs / txVersions resolve the repository for
// transactional use: the WithTxSeams override when wired, the primary
// repository otherwise.
func (s *PhaseService) txPhases() persistence.PhaseRepository {
	if s.phasesTx != nil {
		return s.phasesTx
	}
	return s.phases
}

func (s *PhaseService) txProjects() persistence.ProjectRepository {
	if s.projectsTx != nil {
		return s.projectsTx
	}
	return s.projects
}

func (s *PhaseService) txDocs() persistence.WBSDocumentRepository {
	if s.docsTx != nil {
		return s.docsTx
	}
	return s.docs
}

func (s *PhaseService) txVersions() persistence.WBSVersionRepository {
	if s.versionsTx != nil {
		return s.versionsTx
	}
	return s.versions
}

// CreatePhase appends a new phase to the project with the next server-
// assigned order (WBS 3.16, T3.16.1–T3.16.2, T3.16.5). The project must
// exist (T3.16.7); the client never supplies an order.
func (s *PhaseService) CreatePhase(ctx context.Context, projectID, name, description string) (*phase.Phase, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if strings.TrimSpace(name) == "" {
		return nil, ErrInvalidInput
	}
	if _, err := s.projects.Get(ctx, parsed.String()); err != nil {
		return nil, translate(err)
	}
	p, err := s.phases.Append(ctx, parsed, name, description)
	if err != nil {
		return nil, translate(err)
	}
	return p, nil
}

// GetPhase returns the phase with the given id, scoped to the project
// (T3.16.7): unknown project or unknown/cross-project phase is ErrNotFound.
func (s *PhaseService) GetPhase(ctx context.Context, projectID, phaseID string) (*phase.Phase, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	p, err := s.projects.Get(ctx, parsed.String())
	if err != nil {
		return nil, translate(err)
	}
	ph, err := s.getPhase(ctx, p.ID, phaseID)
	if err != nil {
		return nil, translate(err)
	}
	return ph, nil
}

// ListPhases returns the project's phases in ascending order (T3.16.3).
func (s *PhaseService) ListPhases(ctx context.Context, projectID string) ([]*phase.Phase, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	p, err := s.projects.Get(ctx, parsed.String())
	if err != nil {
		return nil, translate(err)
	}
	out, err := s.phases.ListByProject(ctx, p.ID)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// UpdatePhase persists name/description changes to an existing phase,
// scoped to the project. Order is untouched. Once a phase is executing
// (EXECUTING/VALIDATING) or done (DONE), ordinary edits are rejected with
// ErrConflict so destructive structural changes stay out of the API (WBS
// 3.17, T3.17.3/T3.17.4).
func (s *PhaseService) UpdatePhase(ctx context.Context, projectID, phaseID string, name, description *string) (*phase.Phase, error) {
	p, err := s.GetPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	if p.Status == phase.StatusEXECUTING || p.Status == phase.StatusVALIDATING || p.Status == phase.StatusDONE {
		return nil, ErrConflict
	}
	if name != nil {
		if strings.TrimSpace(*name) == "" {
			return nil, ErrInvalidInput
		}
		p.Name = *name
	}
	if description != nil {
		p.Description = *description
	}
	if err := s.phases.Update(ctx, p); err != nil {
		return nil, translate(err)
	}
	return p, nil
}

// DeletePhase removes a project-scoped phase, subject to the deletion
// eligibility rules (WBS 4.6, T4.6.1–T4.6.6): only a PENDING phase with
// no execution plan is deletable — a phase that has started execution,
// completed execution, or accumulated execution history (plan/work units)
// is rejected with ErrConflict before any mutation. On success the phase
// and its unexecuted WBS document are removed together: transactionally
// when a pool is wired, via the foreign-key cascade otherwise, so no
// orphan rows are left. Remaining phases' order values are untouched.
func (s *PhaseService) DeletePhase(ctx context.Context, projectID, phaseID string) error {
	p, err := s.GetPhase(ctx, projectID, phaseID)
	if err != nil {
		return err
	}
	if p.Status != phase.StatusPENDING {
		return ErrConflict
	}
	if s.plans != nil {
		plans, err := s.plans.ListByPhase(ctx, p.ID)
		if err != nil {
			return translate(err)
		}
		if len(plans) > 0 {
			return ErrConflict
		}
	}
	if s.txPool == nil || s.docs == nil {
		if err := s.phases.Delete(ctx, p.ID); err != nil {
			return translate(err)
		}
		return nil
	}
	return dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		if err := s.docs.WithTx(tx).Delete(ctx, p.ID.String()); err != nil && !errors.Is(err, persistence.ErrNotFound) {
			return translate(err)
		}
		if err := s.phases.WithTx(tx).Delete(ctx, p.ID); err != nil {
			return translate(err)
		}
		return nil
	})
}

// getPhase loads a phase and verifies it belongs to the given project.
func (s *PhaseService) getPhase(ctx context.Context, projectID uuid.UUID, phaseID string) (*phase.Phase, error) {
	parsed, err := uuid.Parse(phaseID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	p, err := s.phases.Get(ctx, parsed)
	if err != nil {
		return nil, err
	}
	if p.ProjectID != projectID {
		return nil, ErrNotFound
	}
	return p, nil
}

// GetCurrentPhase returns the lowest ordered phase that is not DONE
// (WBS 4.7, T4.7.1–T4.7.5). Resolution is purely order-based: timestamps
// are never consulted, and gaps in the order sequence (deleted phases) do
// not affect the result. A project with no phases, or one whose phases
// are all DONE, yields (nil, nil).
func (s *PhaseService) GetCurrentPhase(ctx context.Context, projectID string) (*phase.Phase, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	p, err := s.projects.Get(ctx, parsed.String())
	if err != nil {
		return nil, translate(err)
	}
	phases, err := s.phases.ListByProject(ctx, p.ID)
	if err != nil {
		return nil, translate(err)
	}
	for _, ph := range phases { // ascending phase_order
		if ph.Status != phase.StatusDONE {
			return ph, nil
		}
	}
	return nil, nil
}

// CompletePhase marks an eligible phase DONE (WBS 4.22, T4.22.1–T4.22.5).
// The phase must be in VALIDATING; its active plan (highest-version, not
// SUPERSEDED) must exist and every work unit must be DONE — the P4-15
// eligibility query, wbs.PhaseCompletionEligible. On success the phase
// transitions to DONE with the completion timestamp recorded.
//
// Defined idempotency policy: completing an already-DONE phase is a
// no-op success — it returns nil and preserves the ORIGINAL completion
// timestamp (MarkCompleted uses COALESCE(completed_at, now())).
//
// Completing a phase never advances the next phase: sibling phases keep
// their statuses and timestamps; the next phase becomes the project's
// current phase only because it is now the lowest-order not-DONE phase.
func (s *PhaseService) CompletePhase(ctx context.Context, projectID, phaseID string) (*phase.Phase, error) {
	p, err := s.GetPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	switch p.Status {
	case phase.StatusDONE:
		// Idempotent no-op: the transition already happened.
		return p, nil
	case phase.StatusVALIDATING:
		// fall through to eligibility
	default:
		return nil, ErrInvalidInput
	}
	if s.plans == nil || s.units == nil {
		return nil, ErrInvalidInput
	}
	plans, err := s.plans.ListByPhase(ctx, p.ID)
	if err != nil {
		return nil, translate(err)
	}
	active := wbs.ActivePlan(p.ID, plans)
	if active == nil {
		return nil, ErrInvalidInput
	}
	units, err := s.units.ListByPlan(ctx, active.ID)
	if err != nil {
		return nil, translate(err)
	}
	if !wbs.PhaseCompletionEligible(active, units) {
		return nil, ErrInvalidInput
	}
	if err := s.phases.MarkCompleted(ctx, p.ID); err != nil {
		return nil, translate(err)
	}
	return s.GetPhase(ctx, projectID, phaseID)
}

// MarkPhaseFailed marks a phase failed (WBS 4.23, T4.23.1-T4.23.5).
//
// Allowed states (deliberate choice, stated in the tests): the in-flight
// set EXECUTING and VALIDATING - a failure records work that was in
// progress. PENDING/PLANNING/READY phases have no in-flight work and DONE
// phases are finished, so all are rejected with ErrInvalidInput before any
// mutation.
//
// Defined no-op policy: failing an already-FAILED phase is a no-op
// success - it returns nil and preserves the ORIGINAL failure reason and
// the original failed_at (the repository uses COALESCE, so the first
// failure's timestamp stays authoritative).
//
// The failure never destroys the WBS document or the execution plan: only
// the phases row is mutated (status + failure fields).
func (s *PhaseService) MarkPhaseFailed(ctx context.Context, projectID, phaseID, reason string) (*phase.Phase, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, ErrInvalidInput
	}
	p, err := s.GetPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	switch p.Status {
	case phase.StatusFAILED:
		// Idempotent no-op: the transition already happened.
		return p, nil
	case phase.StatusEXECUTING, phase.StatusVALIDATING:
		// fall through to the failure transition
	default:
		return nil, ErrInvalidInput
	}
	if err := s.phases.MarkFailed(ctx, p.ID, reason); err != nil {
		return nil, translate(err)
	}
	return s.GetPhase(ctx, projectID, phaseID)
}

// GetNextPhase returns the phase with the smallest order greater than the
// given phase's order, scoped to the same project (WBS 4.8,
// T4.8.1–T4.8.4). Gaps in the order sequence are skipped naturally, and
// the highest-ordered phase has no next phase: (nil, nil). A phase that
// belongs to another project is rejected with ErrNotFound.
func (s *PhaseService) GetNextPhase(ctx context.Context, projectID, phaseID string) (*phase.Phase, error) {
	base, err := s.GetPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	phases, err := s.phases.ListByProject(ctx, base.ProjectID)
	if err != nil {
		return nil, translate(err)
	}
	var next *phase.Phase
	for _, ph := range phases { // ascending phase_order
		if ph.Order > base.Order && next == nil {
			next = ph
		}
	}
	return next, nil
}
