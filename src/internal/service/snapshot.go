package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/project"
)

// ProjectConfiguration is the read model later prompt/execution systems
// consume without querying individual settings repeatedly (WBS 3.26). It
// bundles the project, its settings, the current phase, and the current
// phase's WBS document. It is not yet the later full state snapshot used
// for prompt construction.
type ProjectConfiguration struct {
	Project      project.Project
	Settings     project.Settings
	CurrentPhase *phase.Phase
	CurrentWBS   *persistence.WBSDocumentRecord
}

// SnapshotService assembles the project configuration snapshot (WBS 3.26).
type SnapshotService struct {
	projects  persistence.ProjectRepository
	settings  persistence.SettingsRepository
	phases    persistence.PhaseRepository
	documents persistence.WBSDocumentRepository
	txPool    *pgxpool.Pool
}

// NewSnapshotService wires the snapshot service to its repositories and,
// when provided, the transaction pool used for transactionally consistent
// reads (T3.26.5).
func NewSnapshotService(
	projects persistence.ProjectRepository,
	settings persistence.SettingsRepository,
	phases persistence.PhaseRepository,
	documents *dbrepo.DocumentRepository,
	txPool *pgxpool.Pool,
) *SnapshotService {
	return &SnapshotService{
		projects:  projects,
		settings:  settings,
		phases:    phases,
		documents: wbsDocumentAdapter{repo: documents},
		txPool:    txPool,
	}
}

// Snapshot loads the project configuration read model (WBS 3.26). The
// project and its settings are required (ErrNotFound when the project is
// missing); the current phase is the lowest-order incomplete phase, falling
// back to the last phase when every phase is DONE, and nil when the project
// has no phases; the current WBS is the current phase's WBS document and
// nil when it has none. When a transaction pool is available the whole
// snapshot is read in one transaction (T3.26.5); without one the reads run
// sequentially. The snapshot is read-only: it never mutates.
func (s *SnapshotService) Snapshot(ctx context.Context, projectID string) (*ProjectConfiguration, error) {
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	do := func(
		projRepo persistence.ProjectRepository,
		setRepo persistence.SettingsRepository,
		phaseRepo persistence.PhaseRepository,
		docRepo persistence.WBSDocumentRepository,
	) (*ProjectConfiguration, error) {
		p, err := projRepo.Get(ctx, parsed.String())
		if err != nil {
			return nil, translate(err)
		}
		st, err := setRepo.Get(ctx, parsed.String())
		if err != nil {
			return nil, translate(err)
		}
		cfg := &ProjectConfiguration{Project: *p, Settings: *st}
		current, err := phaseRepo.GetNextIncomplete(ctx, parsed)
		if err != nil {
			return nil, translate(err)
		}
		if current == nil {
			all, err := phaseRepo.ListByProject(ctx, parsed)
			if err != nil {
				return nil, translate(err)
			}
			if len(all) > 0 {
				current = all[len(all)-1]
			}
		}
		cfg.CurrentPhase = current
		if current != nil {
			if doc, err := docRepo.GetByPhase(ctx, current.ID.String()); err == nil {
				cfg.CurrentWBS = doc
			} else if !errors.Is(err, persistence.ErrNotFound) {
				return nil, translate(err)
			}
		}
		return cfg, nil
	}
	if s.txPool == nil {
		return do(s.projects, s.settings, s.phases, s.documents)
	}
	var cfg *ProjectConfiguration
	err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var err error
		cfg, err = do(
			s.projects.WithTx(tx),
			s.settings.WithTx(tx),
			s.phases.WithTx(tx),
			s.documents.WithTx(tx),
		)
		return err
	})
	if err != nil {
		return nil, err
	}
	return cfg, nil
}
