package service

import (
	"context"
	"errors"
	"fmt"
	"overseer/internal/bounds"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/wbs"
)

// WBSDocumentService manages phase WBS documents through the application
// layer (WBS 3.18, upgraded to versioned WBS in WBS 4.1/4.9): the first
// WBS is created as version 1, edits append new versions (never
// overwriting history), and reads return the current version or a
// specific one. Executed phases (the P3-17 protection set:
// EXECUTING/VALIDATING/DONE) are protected against WBS rewrites. Updates
// may declare the version they were made against (WBS 4.12); a stale
// declaration is ErrConflict.
type WBSDocumentService struct {
	phases    persistence.PhaseRepository
	projects  persistence.ProjectRepository
	documents *wbsDocumentAdapter
	// versions is the append-only WBS version history (WBS 4.10); when
	// nil the service degrades to the legacy single-document behavior.
	versions persistence.WBSVersionRepository
	// txPool enables transactional create/append (document row and
	// version history move together); nil degrades to sequential calls.
	txPool *pgxpool.Pool
	// plans is the execution-plan repository (WBS 4.13 binding); when
	// nil (WithPlans never called) the plan ops report an infrastructure
	// failure.
	plans *dbrepo.PlanRepository
}

// NewWBSDocumentService wires the service to the phase, project, WBS
// document, and WBS version repositories plus the optional transaction
// pool (WBS 3.18 + 4.1/4.10). Nil version repository/pool degrade to
// the legacy single-document path.
func NewWBSDocumentService(
	phases persistence.PhaseRepository,
	projects persistence.ProjectRepository,
	documents *dbrepo.DocumentRepository,
	versions persistence.WBSVersionRepository,
	txPool *pgxpool.Pool,
) *WBSDocumentService {
	return &WBSDocumentService{
		phases: phases, projects: projects,
		documents: &wbsDocumentAdapter{repo: documents},
		versions:  versions, txPool: txPool,
	}
}

// CreateWBS stores the phase's first WBS document as version 1 (WBS 4.1,
// 4.9): the current-document row and the version-1 history row are created
// together. Empty content is ErrInvalidInput; an unknown phase is
// ErrNotFound; a phase that already has a WBS is ErrConflict.
func (s *WBSDocumentService) CreateWBS(ctx context.Context, projectID, phaseID, content string) (*persistence.WBSDocumentRecord, error) {
	if err := bounds.Check(bounds.WBS, content); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if len(content) == 0 {
		return nil, ErrInvalidInput
	}
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	if _, err := s.documents.GetByPhase(ctx, p.ID.String()); err == nil {
		return nil, ErrConflict
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, translate(err)
	}
	return s.appendVersion(ctx, p.ID.String(), content, 0)
}

// GetWBS returns the phase's current WBS version (WBS 4.1, 4.9): content,
// version number, and identity of the highest stored version. A phase
// without a WBS is ErrNotFound; with no version history wired, the
// legacy document row is returned.
func (s *WBSDocumentService) GetWBS(ctx context.Context, projectID, phaseID string) (*persistence.WBSDocumentRecord, error) {
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	if s.versions == nil {
		return s.documents.GetByPhase(ctx, p.ID.String())
	}
	cur, err := s.versions.GetCurrentVersion(ctx, p.ID.String())
	if err != nil {
		return nil, translate(err)
	}
	if cur == 0 {
		return nil, ErrNotFound
	}
	rec, err := s.versions.GetVersion(ctx, p.ID.String(), cur)
	if err != nil {
		return nil, translate(err)
	}
	return toDocumentRecord(rec), nil
}

// UpdateWBS edits the phase WBS by appending a new version (WBS 4.1) with
// optimistic concurrency (WBS 4.12): the update declares the version it
// was made against (0 = undeclared, legacy behavior). A declaration that
// does not match the phase's current version is ErrConflict before any
// mutation, so stale content is never persisted. Empty content is
// ErrInvalidInput, an unknown phase is ErrNotFound, and executed phases
// are rejected with ErrConflict (the P3-17 protection set).
func (s *WBSDocumentService) UpdateWBS(ctx context.Context, projectID, phaseID, content string, expectedVersion int) (*persistence.WBSDocumentRecord, error) {
	if err := bounds.Check(bounds.WBS, content); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if len(content) == 0 {
		return nil, ErrInvalidInput
	}
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	if p.Status == phase.StatusEXECUTING || p.Status == phase.StatusVALIDATING || p.Status == phase.StatusDONE {
		return nil, ErrConflict
	}
	if s.versions == nil {
		// Legacy single-document path (WBS 3.18).
		doc, err := s.documents.GetByPhase(ctx, p.ID.String())
		if err == nil {
			if expectedVersion > 0 && expectedVersion != doc.Version {
				return nil, ErrConflict
			}
			if err := s.documents.UpdateContent(ctx, p.ID.String(), content); err != nil {
				return nil, translate(err)
			}
			return s.documents.GetByPhase(ctx, p.ID.String())
		}
		if !errors.Is(err, persistence.ErrNotFound) {
			return nil, translate(err)
		}
		if expectedVersion > 0 {
			// A declared version for a phase with no WBS is stale.
			return nil, ErrConflict
		}
		return s.documents.Create(ctx, p.ID.String(), content)
	}
	cur, err := s.versions.GetCurrentVersion(ctx, p.ID.String())
	if err != nil {
		return nil, translate(err)
	}
	if expectedVersion > 0 && expectedVersion != cur {
		return nil, ErrConflict
	}
	return s.appendVersion(ctx, p.ID.String(), content, expectedVersion)
}

// GetWBSVersion returns one stored version of the phase WBS (WBS 4.1);
// unknown phases and versions are ErrNotFound, a version below 1 is
// ErrInvalidInput.
func (s *WBSDocumentService) GetWBSVersion(ctx context.Context, projectID, phaseID string, version int) (*persistence.WBSDocumentRecord, error) {
	if version < 1 {
		return nil, ErrInvalidInput
	}
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	if s.versions == nil {
		if version != 1 {
			return nil, ErrNotFound
		}
		return s.documents.GetByPhase(ctx, p.ID.String())
	}
	rec, err := s.versions.GetVersion(ctx, p.ID.String(), version)
	if err != nil {
		return nil, translate(err)
	}
	return toDocumentRecord(rec), nil
}

// ListWBSVersions returns the phase's stored WBS versions in ascending
// order (WBS 4.1); a phase without a WBS yields an empty list.
func (s *WBSDocumentService) ListWBSVersions(ctx context.Context, projectID, phaseID string) ([]*persistence.WBSDocumentRecord, error) {
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	if s.versions == nil {
		doc, err := s.documents.GetByPhase(ctx, p.ID.String())
		if err != nil {
			return nil, translate(err)
		}
		return []*persistence.WBSDocumentRecord{doc}, nil
	}
	recs, err := s.versions.ListVersions(ctx, p.ID.String())
	if err != nil {
		return nil, translate(err)
	}
	out := make([]*persistence.WBSDocumentRecord, 0, len(recs))
	for _, r := range recs {
		out = append(out, toDocumentRecord(r))
	}
	return out, nil
}

// ListWBSVersionHistory returns the phase's stored WBS versions with their
// deterministic content hashes and timestamps in ascending version order
// (WBS 4.16 history surface). A phase without a WBS yields an empty list;
// without version history wired, the legacy single document is returned as
// a one-entry history (no stored hash).
func (s *WBSDocumentService) ListWBSVersionHistory(ctx context.Context, projectID, phaseID string) ([]*persistence.WBSVersionRecord, error) {
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	if s.versions == nil {
		doc, err := s.documents.GetByPhase(ctx, p.ID.String())
		if err != nil {
			return nil, translate(err)
		}
		return []*persistence.WBSVersionRecord{{
			PhaseID: doc.PhaseID, Content: doc.Content, Version: doc.Version,
		}}, nil
	}
	recs, err := s.versions.ListVersions(ctx, p.ID.String())
	if err != nil {
		return nil, translate(err)
	}
	return recs, nil
}

// GetWBSVersionHistory returns one stored WBS version with its content
// hash and timestamp (WBS 4.16). Unknown versions are ErrNotFound, a
// version below 1 is ErrInvalidInput.
func (s *WBSDocumentService) GetWBSVersionHistory(ctx context.Context, projectID, phaseID string, version int) (*persistence.WBSVersionRecord, error) {
	if version < 1 {
		return nil, ErrInvalidInput
	}
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	if s.versions == nil {
		if version != 1 {
			return nil, ErrNotFound
		}
		doc, err := s.documents.GetByPhase(ctx, p.ID.String())
		if err != nil {
			return nil, translate(err)
		}
		return &persistence.WBSVersionRecord{
			PhaseID: doc.PhaseID, Content: doc.Content, Version: doc.Version,
		}, nil
	}
	rec, err := s.versions.GetVersion(ctx, p.ID.String(), version)
	if err != nil {
		return nil, translate(err)
	}
	return rec, nil
}

// DiffWBS returns the deterministic line-based diff between two stored WBS
// versions of the phase (WBS 4.17 endpoint half, composing the P4-23
// primitive): from/to below 1 are ErrInvalidInput, unknown phases and
// versions are ErrNotFound. A phase without a WBS is ErrNotFound; without
// version history wired, only version 1 exists (the legacy document).
func (s *WBSDocumentService) DiffWBS(ctx context.Context, projectID, phaseID string, from, to int) (wbs.Diff, error) {
	if from < 1 || to < 1 {
		return wbs.Diff{}, ErrInvalidInput
	}
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return wbs.Diff{}, err
	}
	if s.versions == nil {
		if from != 1 || to != 1 {
			return wbs.Diff{}, ErrNotFound
		}
		doc, err := s.documents.GetByPhase(ctx, p.ID.String())
		if err != nil {
			return wbs.Diff{}, translate(err)
		}
		return wbs.DiffContents(&doc.Content, &doc.Content)
	}
	fromRec, err := s.versions.GetVersion(ctx, p.ID.String(), from)
	if err != nil {
		return wbs.Diff{}, translate(err)
	}
	toRec, err := s.versions.GetVersion(ctx, p.ID.String(), to)
	if err != nil {
		return wbs.Diff{}, translate(err)
	}
	return wbs.DiffContents(&fromRec.Content, &toRec.Content)
}

// appendVersion appends the content as the next WBS version and keeps the
// current-document row in sync (content + version pointer) in one unit:
// transactionally when a pool is wired, sequentially otherwise. When the
// update declared an expected version (WBS 4.12), the check is re-run
// under the phase-scoped advisory lock inside the transaction so a
// concurrent update that wins the lock first forces this one into
// ErrConflict before any mutation lands.
func (s *WBSDocumentService) appendVersion(ctx context.Context, phaseID, content string, expectedVersion int) (*persistence.WBSDocumentRecord, error) {
	if s.txPool != nil {
		err := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
			docs := s.documents.WithTx(tx)
			vers := s.versions.WithTx(tx)
			if expectedVersion > 0 {
				if err := s.checkExpectedVersion(ctx, tx, vers, phaseID, expectedVersion); err != nil {
					return err
				}
			}
			if _, err := docs.GetByPhase(ctx, phaseID); err != nil {
				if !errors.Is(err, persistence.ErrNotFound) {
					return translate(err)
				}
				if _, err := docs.Create(ctx, phaseID, content); err != nil {
					return translate(err)
				}
			} else if err := docs.UpdateContent(ctx, phaseID, content); err != nil {
				return translate(err)
			}
			if _, err := vers.AppendVersion(ctx, phaseID, content); err != nil {
				return translate(err)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		if _, err := s.documents.GetByPhase(ctx, phaseID); err != nil {
			if !errors.Is(err, persistence.ErrNotFound) {
				return nil, translate(err)
			}
			if _, err := s.documents.Create(ctx, phaseID, content); err != nil {
				return nil, translate(err)
			}
		} else if err := s.documents.UpdateContent(ctx, phaseID, content); err != nil {
			return nil, translate(err)
		}
		if _, err := s.versions.AppendVersion(ctx, phaseID, content); err != nil {
			return nil, translate(err)
		}
	}
	return s.documents.GetByPhase(ctx, phaseID)
}

// checkExpectedVersion re-validates the declared version under the
// phase-scoped advisory lock that serializes appends (WBS 4.12, same
// recipe as phase order allocation). A mismatch is ErrConflict; the
// caller's transaction rolls back, so nothing is mutated.
func (s *WBSDocumentService) checkExpectedVersion(ctx context.Context, tx persistence.Transaction, vers persistence.WBSVersionRepository, phaseID string, expectedVersion int) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "wbs-versions:"+phaseID); err != nil {
		return translate(err)
	}
	cur, err := vers.GetCurrentVersion(ctx, phaseID)
	if err != nil {
		return translate(err)
	}
	if expectedVersion != cur {
		return ErrConflict
	}
	return nil
}

// toDocumentRecord projects a stored WBS version onto the document record
// shape the service and web layers already use.
func toDocumentRecord(r *persistence.WBSVersionRecord) *persistence.WBSDocumentRecord {
	return &persistence.WBSDocumentRecord{
		ID: r.ID, PhaseID: r.PhaseID, Content: r.Content, Version: r.Version,
	}
}

// WithPlans wires the execution-plan repository for the plan binding
// operations (WBS 4.13).
func (s *WBSDocumentService) WithPlans(plans *dbrepo.PlanRepository) *WBSDocumentService {
	s.plans = plans
	return s
}

// CreatePlan generates an execution plan bound to the phase WBS version
// it was generated from (WBS 4.13, T4.13.1/T4.13.4): the source version
// must exist in the phase's WBS history — a non-existent version is
// ErrNotFound before any plan row is written. Plans are versioned per
// phase (next plan version, unique (phase, version)).
func (s *WBSDocumentService) CreatePlan(ctx context.Context, projectID, phaseID string, sourceWBSVersion int) (*wbs.ExecutionPlan, error) {
	if s.plans == nil {
		return nil, ErrInfrastructure
	}
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	if _, err := s.versions.GetVersion(ctx, p.ID.String(), sourceWBSVersion); err != nil {
		return nil, translate(err)
	}
	plan, err := wbs.NewExecutionPlan(p.ID, sourceWBSVersion)
	if err != nil {
		return nil, translate(err)
	}
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var err error
		if s.txPool != nil {
			err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
				// Serialize plan-version allocation per phase so concurrent
				// plans receive distinct versions (same recipe as WBS 4.12
				// version checks); the UNIQUE (phase_id, version) constraint
				// is the backstop the retry catches.
				if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "wbs-versions:"+p.ID.String()); err != nil {
					return translate(err)
				}
				plans := s.plans.WithTx(tx)
				next, err := plans.NextVersion(ctx, p.ID)
				if err != nil {
					return translate(err)
				}
				plan.Version = next
				return plans.Create(ctx, plan)
			})
		} else {
			next, err := s.plans.NextVersion(ctx, p.ID)
			if err != nil {
				return nil, translate(err)
			}
			plan.Version = next
			err = s.plans.Create(ctx, plan)
		}
		if err == nil {
			return plan, nil
		}
		if !errors.Is(err, ErrConflict) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// GetPlanSourceWBS returns the exact WBS content a plan was generated
// from (WBS 4.13, T4.13.3): the plan's stored source version is resolved
// from the append-only history, so historical content stays retrievable
// after newer versions exist. Unknown plans are ErrNotFound; a plan
// whose source version row is missing (invalid binding) is ErrNotFound.
func (s *WBSDocumentService) GetPlanSourceWBS(ctx context.Context, projectID, phaseID, planID string) (*persistence.WBSDocumentRecord, error) {
	if s.plans == nil {
		return nil, ErrInfrastructure
	}
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return nil, err
	}
	parsed, err := uuid.Parse(planID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	plan, err := s.plans.Get(ctx, parsed)
	if err != nil {
		return nil, translate(err)
	}
	if plan.PhaseID != p.ID {
		return nil, ErrNotFound
	}
	rec, err := s.versions.GetVersion(ctx, p.ID.String(), plan.WBSVersion)
	if err != nil {
		return nil, translate(err)
	}
	return toDocumentRecord(rec), nil
}

// getScopedPhase loads the phase and verifies both the project scope and
// the phase scope (same contract as PhaseService.GetPhase).
func (s *WBSDocumentService) getScopedPhase(ctx context.Context, projectID, phaseID string) (*phase.Phase, error) {
	parsedProject, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if _, err := s.projects.Get(ctx, parsedProject.String()); err != nil {
		return nil, translate(err)
	}
	parsedPhase, err := uuid.Parse(phaseID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	p, err := s.phases.Get(ctx, parsedPhase)
	if err != nil {
		return nil, translate(err)
	}
	if p.ProjectID != parsedProject {
		return nil, ErrNotFound
	}
	return p, nil
}
