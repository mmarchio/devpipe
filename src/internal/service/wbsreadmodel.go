package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/wbs"
)

// WBSReadModelPhase is the current-phase metadata of the WBS read model
// (WBS 4.15).
type WBSReadModelPhase struct {
	Order  int
	Name   string
	Status phase.PhaseStatus
}

// WBSReadModelWBS is the current WBS section of the read model (WBS
// 4.15): the current version number, its deterministic content hash, and
// the markdown content retained exactly.
type WBSReadModelWBS struct {
	Version int
	Hash    string
	Content string
}

// WBSReadModelPlan is the active execution plan relationship of the read
// model (WBS 4.15): the plan version, the exact WBS version the plan was
// generated from, and the DERIVED staleness flag (plan.wbs_version <
// current WBS version, never persisted).
type WBSReadModelPlan struct {
	Version    int
	WBSVersion int
	Stale      bool
}

// WBSReadModel is the assembled read combining phase metadata, the current
// WBS, and the plan relationship (WBS 4.15). It is the read-side building
// block for the later current-wbs endpoint (P4-22). WBS is nil when the
// current phase has no WBS document; Plan is nil when the phase has no
// active plan (including every case with no WBS).
type WBSReadModel struct {
	Phase *WBSReadModelPhase
	WBS   *WBSReadModelWBS
	Plan  *WBSReadModelPlan
}

// WBSReadModelService assembles the WBS read model (WBS 4.15).
type WBSReadModelService struct {
	phases   persistence.PhaseRepository
	projects persistence.ProjectRepository
	versions persistence.WBSVersionRepository
	plans    *dbrepo.PlanRepository
	txPool   *pgxpool.Pool
}

// NewWBSReadModelService wires the read-model service to its repositories
// and, when provided, the transaction pool used for transactionally
// consistent assembly (same pattern as the P3-26 snapshot service).
func NewWBSReadModelService(
	phases persistence.PhaseRepository,
	projects persistence.ProjectRepository,
	versions persistence.WBSVersionRepository,
	plans *dbrepo.PlanRepository,
	txPool *pgxpool.Pool,
) *WBSReadModelService {
	return &WBSReadModelService{
		phases: phases, projects: projects,
		versions: versions, plans: plans, txPool: txPool,
	}
}

// WBSReadModel loads the WBS read model for a project (WBS 4.15). The
// project must exist (unknown/malformed id → ErrNotFound/ErrInvalidInput).
// The current phase is the P4-04 semantics (lowest-order not-DONE; nil
// when the project has none) and yields a nil Phase section. The current
// WBS is the phase's highest stored version (hash + content); a phase with
// no WBS yields a nil WBS section (only ErrNotFound is swallowed). The
// plan section is the active plan (WBS 2.19: highest version, not
// SUPERSEDED) with its stored source WBS version and the DERIVED stale
// flag; a phase without an active plan yields a nil Plan section. When a
// transaction pool is available the whole assembly runs in one
// transaction; without one the reads run sequentially. The read model is
// read-only: it never mutates phases, documents, plans, or work units.
func (s *WBSReadModelService) WBSReadModel(ctx context.Context, projectID string) (*WBSReadModel, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	do := func(
		projRepo persistence.ProjectRepository,
		phaseRepo persistence.PhaseRepository,
		versRepo persistence.WBSVersionRepository,
		planRepo *dbrepo.PlanRepository,
	) (*WBSReadModel, error) {
		if _, err := projRepo.Get(ctx, parsed.String()); err != nil {
			return nil, translate(err)
		}
		out := &WBSReadModel{}
		current, err := phaseRepo.GetNextIncomplete(ctx, parsed)
		if err != nil {
			return nil, translate(err)
		}
		if current == nil {
			return out, nil
		}
		out.Phase = &WBSReadModelPhase{
			Order: current.Order, Name: current.Name, Status: current.Status,
		}
		cur, err := versRepo.GetCurrentVersion(ctx, current.ID.String())
		if err != nil {
			return nil, translate(err)
		}
		if cur > 0 {
			rec, err := versRepo.GetVersion(ctx, current.ID.String(), cur)
			if err != nil {
				return nil, translate(err)
			}
			out.WBS = &WBSReadModelWBS{
				Version: rec.Version, Hash: rec.ContentHash, Content: rec.Content,
			}
		}
		if s.plans != nil {
			plans, err := planRepo.ListByPhase(ctx, current.ID)
			if err != nil {
				return nil, translate(err)
			}
			if active := wbs.ActivePlan(current.ID, plans); active != nil {
				out.Plan = &WBSReadModelPlan{
					Version: active.Version, WBSVersion: active.WBSVersion,
					Stale: active.WBSVersion < cur,
				}
			}
		}
		return out, nil
	}
	if s.txPool == nil {
		return do(s.projects, s.phases, s.versions, s.plans)
	}
	var out *WBSReadModel
	err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var err error
		out, err = do(
			s.projects.WithTx(tx),
			s.phases.WithTx(tx),
			s.versions.WithTx(tx),
			s.plans.WithTx(tx),
		)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
