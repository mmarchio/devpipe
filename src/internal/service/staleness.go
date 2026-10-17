package service

import (
	"context"

	"overseer/internal/wbs"
)

// PlanStalenessState is the derived plan-vs-WBS relationship (WBS 4.14);
// the canonical type lives in the wbs domain package.
type PlanStalenessState = wbs.PlanStalenessState

const (
	PlanStaleCurrent   = wbs.PlanStaleCurrent
	PlanStaleStale     = wbs.PlanStaleStale
	PlanStaleUnplanned = wbs.PlanStaleUnplanned
)

// PlanStaleness reports whether the phase's active execution plan was
// generated from the current WBS (WBS 4.14, T4.14.1–T4.14.4). The state is
// always DERIVED from the plan's stored source wbs_version (WBS 4.13
// binding, never mutated) and the phase's current WBS version: no new
// persisted state, no migration — a WBS update therefore marks the
// previous plan stale on the same tick. A phase without a plan (or without
// a WBS) is Unplanned.
func (s *WBSDocumentService) PlanStaleness(ctx context.Context, projectID, phaseID string) (wbs.PlanStalenessState, error) {
	if s.plans == nil || s.versions == nil {
		return "", ErrInfrastructure
	}
	p, err := s.getScopedPhase(ctx, projectID, phaseID)
	if err != nil {
		return "", err
	}
	plans, err := s.plans.ListByPhase(ctx, p.ID)
	if err != nil {
		return "", translate(err)
	}
	active := wbs.ActivePlan(p.ID, plans)
	if active == nil {
		return wbs.PlanStaleUnplanned, nil
	}
	cur, err := s.versions.GetCurrentVersion(ctx, p.ID.String())
	if err != nil {
		return "", translate(err)
	}
	if cur == 0 {
		return wbs.PlanStaleUnplanned, nil
	}
	return wbs.PlanStaleness(active, cur), nil
}
