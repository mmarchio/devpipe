package service

import (
	"context"

	"github.com/google/uuid"

	"overseer/internal/persistence"
)

// LineageSource reads unit lineage (dbrepo.LineageRepository).
type LineageSource interface {
	UnitLineage(ctx context.Context, projectID, unitID uuid.UUID) (*persistence.UnitLineage, error)
}

// LineageService serves work-unit lineage (WBS 18.205).
type LineageService struct{ src LineageSource }

// NewLineageService builds the service.
func NewLineageService(src LineageSource) *LineageService { return &LineageService{src: src} }

// UnitLineage returns the lineage of a project's work unit (ErrNotFound
// when the unit is not the project's).
func (s *LineageService) UnitLineage(ctx context.Context, projectID, unitID uuid.UUID) (*persistence.UnitLineage, error) {
	l, err := s.src.UnitLineage(ctx, projectID, unitID)
	if err != nil {
		return nil, translate(err)
	}
	return l, nil
}
