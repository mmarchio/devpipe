package service

import (
	"context"
	"errors"
	"fmt"
	"overseer/internal/bounds"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/phase"
)

// CreatePhaseWithWBS creates a phase and its initial WBS document (version
// 1) in ONE service transaction (WBS 4.28, T4.28.1–T4.28.4): the UI submits
// both together, one commit covers both, and failure of either rolls back
// both. The phase receives the next server-assigned order — the append's
// advisory lock and conflict retry run INSIDE the outer transaction (the
// repository joins it, never nests a commit) — and the WBS stores the
// document row plus the version-1 history row before the commit.
//
// Validation (empty name, empty content, malformed project id) happens
// before the transaction, and the project-existence pre-read mirrors
// CreatePhase. When no transaction pool is wired the two repository calls
// run sequentially (the established service degradation pattern).
func (s *PhaseService) CreatePhaseWithWBS(ctx context.Context, projectID, name, description, wbsContent string) (*phase.Phase, *persistence.WBSDocumentRecord, error) {
	if err := bounds.Check(bounds.WBS, wbsContent); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if strings.TrimSpace(name) == "" || len(wbsContent) == 0 {
		return nil, nil, ErrInvalidInput
	}
	parsed, err := uuid.Parse(projectID)
	if err != nil {
		return nil, nil, ErrInvalidInput
	}
	if _, err := s.projects.Get(ctx, parsed.String()); err != nil {
		return nil, nil, translate(err)
	}
	if s.txPool == nil {
		p, err := s.phases.Append(ctx, parsed, name, description)
		if err != nil {
			return nil, nil, translate(err)
		}
		rec, err := s.createWBSV1(ctx, p.ID.String(), wbsContent)
		if err != nil {
			return nil, nil, translate(err)
		}
		return p, rec, nil
	}
	var created *phase.Phase
	err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		created, err = s.txPhases().WithTx(tx).Append(ctx, parsed, name, description)
		if err != nil {
			return translate(err)
		}
		_, err = s.createWBSV1Tx(ctx, tx, created.ID.String(), wbsContent)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	rec, err := s.docs.GetByPhase(ctx, created.ID.String())
	if err != nil {
		return nil, nil, translate(err)
	}
	return created, rec, nil
}

// createWBSV1Tx stores the initial WBS (document row + version-1 history
// row) on the given transaction — the in-transaction half of
// CreatePhaseWithWBS.
func (s *PhaseService) createWBSV1Tx(ctx context.Context, tx persistence.Transaction, phaseID, content string) (rec *persistence.WBSDocumentRecord, err error) {
	docs := s.txDocs().WithTx(tx)
	if _, err := docs.GetByPhase(ctx, phaseID); err == nil {
		return nil, ErrConflict
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, translate(err)
	}
	if rec, err = docs.Create(ctx, phaseID, content); err != nil {
		return nil, translate(err)
	}
	if vers := s.txVersions(); vers != nil {
		if _, err = vers.WithTx(tx).AppendVersion(ctx, phaseID, content); err != nil {
			return nil, translate(err)
		}
	}
	return rec, nil
}

// createWBSV1 stores the initial WBS outside a service transaction (the
// nil-pool sequential fallback of CreatePhaseWithWBS).
func (s *PhaseService) createWBSV1(ctx context.Context, phaseID, content string) (*persistence.WBSDocumentRecord, error) {
	docs := s.txDocs()
	if _, err := docs.GetByPhase(ctx, phaseID); err == nil {
		return nil, ErrConflict
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, translate(err)
	}
	rec, err := docs.Create(ctx, phaseID, content)
	if err != nil {
		return nil, translate(err)
	}
	if vers := s.txVersions(); vers != nil {
		if _, err = vers.AppendVersion(ctx, phaseID, content); err != nil {
			return nil, translate(err)
		}
	}
	return rec, nil
}
