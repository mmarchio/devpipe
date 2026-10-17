// P7-13 (WBS 7.18/7.19): scope resolution + missing required vs optional
// state-file handling.
//
// The StateFileScopeResolutionService is the read-model op that, for every
// required type of a project, resolves the file at the type's DECLARED
// scope from the runtime context:
//
//   - PROJECT-scoped types -> (type, PROJECT, no associations)
//   - PHASE-scoped types   -> (type, PHASE, the project's current phase)
//   - WORK_UNIT-scoped     -> (type, WORK_UNIT, the current work unit)
//   - EXECUTION-scoped     -> (type, EXECUTION, no associations)
//
// The scope mapping is per TYPE record (state_file_types.scope), not per
// category string — the type is looked up through the repository and its
// stored scope drives the association (task NOTES).
//
// Requiredness (WBS 7.19) is expressed by the persisted type data
// (state_file_types.required, the P7-01 flag): a missing REQUIRED type
// fails the op with an error identifying the type's key
// (service.ErrMissingStateFile, T7.19.1/T7.19.2); a missing OPTIONAL type
// is omitted from the result without failing (T7.19.3). The op is a pure
// read — it never creates files or writes content, so a failure leaves the
// persisted state byte-identical (T7.19.4, asserted by direct SQL in the
// integration tests).
//
// Current phase / work unit reuse the P7-10 selection rule (stated there,
// pinned by its tests): the current phase's active (highest non-SUPERSEDED
// version) plan, first non-DONE unit in sequence order. A project lacking
// the context a required type's scope needs (no current phase / no current
// work unit) is an error (T7.18.4, service.ErrNotFound).
//
// No cross-project leakage (T7.18.5): the file lookup is
// (type, scope, association)-addressed, the type is project-scoped (the
// project's own type record, with the global default as fallback), and the
// phase/work-unit associations are project-scoped FKs — a file of another
// project can never be addressed.
//
// The Tx variant (ResolveStateFileScopesTx, P7-18) runs the same pipeline
// with every repository read joined to the caller's transaction — the
// snapshot-creation op needs it so the requirement + scope resolution sees
// the same consistent view as the rest of the snapshot read.
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// StateFileScopeResolutionService resolves, per required type, the scoped
// state file from the runtime context (WBS 7.18/7.19).
type StateFileScopeResolutionService struct {
	reqs   *StateFileRequirementResolutionService
	types  persistence.StateFileTypeRepository
	files  persistence.StateFileRepository
	phases persistence.PhaseRepository
	plans  activeTaskPlanLister
	units  activeTaskUnitLister
}

// NewStateFileScopeResolutionService wires the scope-resolution op: the
// P7-12 requirement-resolution op (which types the project requires), the
// type and file repositories (scope mapping + file lookup), and the
// phase/plan/unit repositories the runtime context reads.
func NewStateFileScopeResolutionService(
	reqs *StateFileRequirementResolutionService,
	types persistence.StateFileTypeRepository,
	files persistence.StateFileRepository,
	phases persistence.PhaseRepository,
	plans activeTaskPlanLister,
	units activeTaskUnitLister,
) *StateFileScopeResolutionService {
	return &StateFileScopeResolutionService{
		reqs: reqs, types: types, files: files,
		phases: phases, plans: plans, units: units,
	}
}

// ResolvedStateFile is one resolved (type, file) pair.
type ResolvedStateFile struct {
	Type *persistence.StateFileTypeRecord
	File *persistence.StateFileRecord
}

// scopeResolutionRepos is the op's repository set, each bound to the
// attempt's transaction when tx is non-nil (the P7-18 Tx variant).
type scopeResolutionRepos struct {
	// phaseAt / unitAt, when set, pin the scope position explicitly
	// (Phase 11 projections) instead of deriving the current phase / work
	// unit.
	phaseAt *uuid.UUID
	unitAt  *uuid.UUID

	types  persistence.StateFileTypeRepository
	files  persistence.StateFileRepository
	phases persistence.PhaseRepository
	plans  activeTaskPlanLister
	units  activeTaskUnitLister
}

func (s *StateFileScopeResolutionService) bind(tx persistence.Transaction) scopeResolutionRepos {
	if tx == nil {
		return scopeResolutionRepos{types: s.types, files: s.files,
			phases: s.phases, plans: s.plans, units: s.units}
	}
	return scopeResolutionRepos{
		types: s.types.WithTx(tx), files: s.files.WithTx(tx),
		phases: s.phases.WithTx(tx),
		plans:  &txPlanLister{p: s.plans, tx: tx},
		units:  &txUnitLister{u: s.units, tx: tx},
	}
}

// ResolveStateFileScopes returns, for every required type the project
// requires (the P7-12 resolved set intersected with enabled required types),
// the file at the type's declared scope (WBS 7.18, T7.18.1-T7.18.5). A
// missing required context (no current phase for a PHASE scope, no current
// work unit for a WORK_UNIT scope) is an error (T7.18.4); a missing
// required FILE fails with ErrMissingStateFile identifying the type
// (T7.19.1/T7.19.2); a missing optional file is omitted (T7.19.3). The op
// is read-only (T7.19.4).
func (s *StateFileScopeResolutionService) ResolveStateFileScopes(ctx context.Context, projectID string) ([]ResolvedStateFile, error) {
	return s.resolve(ctx, nil, projectID)
}

// ResolveStateFileScopesTx is ResolveStateFileScopes with every repository
// read joined to the given transaction (P7-18: the snapshot-creation op
// needs the resolution inside its one consistent view). tx must be
// non-nil.
func (s *StateFileScopeResolutionService) ResolveStateFileScopesTx(ctx context.Context, tx persistence.Transaction, projectID string) ([]ResolvedStateFile, error) {
	return s.resolve(ctx, tx, projectID)
}

// ResolveStateFileScopesAt resolves the required files at an EXPLICIT scope
// position (Phase 11 projections, P11-21): phase / work-unit scoped types
// resolve against the given phase / unit instead of the current ones. A
// missing REQUIRED file is reported in missing (not an error) so a
// projection can decide what it can prepare; nothing is written.
func (s *StateFileScopeResolutionService) ResolveStateFileScopesAt(ctx context.Context, projectID string, phaseID, unitID *uuid.UUID) ([]ResolvedStateFile, []*persistence.StateFileTypeRecord, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, nil, ErrInvalidInput
	}
	categories, err := s.reqs.ResolveStateFileRequirements(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	r := s.bind(nil)
	r.phaseAt, r.unitAt = phaseID, unitID
	byKey, err := r.typeByRequirementKey(ctx, pid)
	if err != nil {
		return nil, nil, translate(err)
	}
	var out []ResolvedStateFile
	var missing []*persistence.StateFileTypeRecord
	for _, category := range categories {
		rec, ok := byKey[category]
		if !ok || !rec.Enabled {
			continue
		}
		file, err := r.lookupScopedFile(ctx, pid, rec)
		if err != nil {
			if !errors.Is(err, persistence.ErrNotFound) {
				return nil, nil, translate(err)
			}
			if rec.Required {
				missing = append(missing, rec)
			}
			continue
		}
		out = append(out, ResolvedStateFile{Type: rec, File: file})
	}
	return out, missing, nil
}

func (s *StateFileScopeResolutionService) resolve(ctx context.Context, tx persistence.Transaction, projectID string) ([]ResolvedStateFile, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	var categories []string
	if tx == nil {
		categories, err = s.reqs.ResolveStateFileRequirements(ctx, projectID)
	} else {
		categories, err = s.reqs.ResolveStateFileRequirementsTx(ctx, tx, projectID)
	}
	if err != nil {
		return nil, err
	}
	r := s.bind(tx)
	byKey, err := r.typeByRequirementKey(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}
	var out []ResolvedStateFile
	for _, category := range categories {
		rec, ok := byKey[category]
		if !ok || !rec.Enabled {
			// No enabled type carries this requirement: nothing to
			// resolve.
			continue
		}
		var file *persistence.StateFileRecord
		if rec.Required {
			file, err = r.resolveScopedFile(ctx, pid, rec)
			if err != nil {
				return nil, translate(err)
			}
		} else {
			// Optional: attempt the lookup, omit on absence (T7.19.3).
			file, err = r.lookupScopedFile(ctx, pid, rec)
			if err != nil {
				if !errors.Is(err, persistence.ErrNotFound) {
					return nil, translate(err)
				}
				continue
			}
		}
		out = append(out, ResolvedStateFile{Type: rec, File: file})
	}
	return out, nil
}

// scopeResolutionContext runs the op's context + lookup helpers against
// one bound repository set.
type scopeResolutionContext struct {
	r scopeResolutionRepos
}

func (r scopeResolutionRepos) ctx() *scopeResolutionContext { return &scopeResolutionContext{r: r} }

// resolveScopedFile resolves a REQUIRED type's file. The runtime context is
// resolved FIRST, so a missing required context (no current phase / work
// unit) is surfaced as persistence.ErrNotFound (T7.18.4) — distinct from a
// present context with no file, which fails with an error identifying the
// type's key (T7.19.1/T7.19.2).
func (r scopeResolutionRepos) resolveScopedFile(ctx context.Context, projectID uuid.UUID, rec *persistence.StateFileTypeRecord) (*persistence.StateFileRecord, error) {
	c := r.ctx()
	if rec.Scope == string(statefile.ScopePhase) && r.phaseAt == nil {
		if _, err := c.currentPhase(ctx, projectID); err != nil {
			return nil, err
		}
	}
	if rec.Scope == string(statefile.ScopeWorkUnit) && r.unitAt == nil {
		if _, err := c.currentWorkUnit(ctx, projectID); err != nil {
			return nil, err
		}
	}
	file, err := r.lookupScopedFile(ctx, projectID, rec)
	if err == nil {
		return file, nil
	}
	if !errors.Is(err, persistence.ErrNotFound) {
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s (%s)", ErrMissingStateFile, rec.Key, rec.Scope)
}

// lookupScopedFile looks up the file at the type's declared scope:
// PROJECT/EXECUTION scopes address the bare (type, scope) pair; PHASE and
// WORK_UNIT scopes resolve the runtime association first (T7.18.4 — a
// missing required context is persistence.ErrNotFound, surfaced to callers
// as service.ErrNotFound).
func (r scopeResolutionRepos) lookupScopedFile(ctx context.Context, projectID uuid.UUID, rec *persistence.StateFileTypeRecord) (*persistence.StateFileRecord, error) {
	c := r.ctx()
	var phaseID, unitID *uuid.UUID
	switch rec.Scope {
	case string(statefile.ScopePhase):
		if r.phaseAt != nil {
			phaseID = r.phaseAt
			break
		}
		ph, err := c.currentPhase(ctx, projectID)
		if err != nil {
			return nil, err
		}
		phaseID = &ph.ID
	case string(statefile.ScopeWorkUnit):
		if r.unitAt != nil {
			unitID = r.unitAt
			break
		}
		unit, err := c.currentWorkUnit(ctx, projectID)
		if err != nil {
			return nil, err
		}
		unitID = &unit.ID
	}
	return r.files.GetByTypeAndScope(ctx, projectID, rec.ID, rec.Scope, phaseID, unitID)
}

// typeByRequirementKey maps each enabled type (the global defaults first,
// then the project's own types, which shadow a same-key global type) to its
// key, so the P7-12 resolved requirement set maps to its type record.
func (r scopeResolutionRepos) typeByRequirementKey(ctx context.Context, projectID uuid.UUID) (map[string]*persistence.StateFileTypeRecord, error) {
	byKey := map[string]*persistence.StateFileTypeRecord{}
	global, err := r.types.ListByScope(ctx, nil)
	if err != nil {
		return nil, err
	}
	for _, rec := range global {
		byKey[rec.Key] = rec
	}
	projectTypes, err := r.types.ListByScope(ctx, &projectID)
	if err != nil {
		return nil, err
	}
	for _, rec := range projectTypes {
		byKey[rec.Key] = rec
	}
	return byKey, nil
}

// currentPhase returns the project's current phase (WBS 2.19);
// persistence.ErrNotFound when the project has none (T7.18.4).
// TypeByRequirementKey returns the enabled type records keyed by their key
// (global defaults first, then the project's own, which shadow a same-key
// global), joined to the given transaction when non-nil. The P7-23
// context-preview op uses it to re-resolve the missing required files after
// the strict scope op reports the first missing type.
func (s *StateFileScopeResolutionService) TypeByRequirementKey(ctx context.Context, tx persistence.Transaction, projectID uuid.UUID) (map[string]*persistence.StateFileTypeRecord, error) {
	r := s.bind(tx)
	return r.typeByRequirementKey(ctx, projectID)
}

// LookupScopedFile looks up the file at the type's declared scope, joined to
// the given transaction when non-nil. The P7-23 context-preview op uses it
// per required type so a missing file reports as a nil-file entry (the
// preview's MissingFiles set) instead of failing the whole op.
func (s *StateFileScopeResolutionService) LookupScopedFile(ctx context.Context, tx persistence.Transaction, projectID uuid.UUID, rec *persistence.StateFileTypeRecord) (*persistence.StateFileRecord, error) {
	r := s.bind(tx)
	return r.lookupScopedFile(ctx, projectID, rec)
}

func (c *scopeResolutionContext) currentPhase(ctx context.Context, projectID uuid.UUID) (*phase.Phase, error) {
	phases, err := c.r.phases.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	cur := wbs.CurrentPhase(phases)
	if cur == nil {
		return nil, persistence.ErrNotFound
	}
	return cur, nil
}

// currentWorkUnit returns the project's current work unit under the P7-10
// selection rule; persistence.ErrNotFound when absent (T7.18.4).
func (c *scopeResolutionContext) currentWorkUnit(ctx context.Context, projectID uuid.UUID) (*wbs.WorkUnit, error) {
	phases, err := c.r.phases.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	cur := wbs.CurrentPhase(phases)
	if cur == nil {
		return nil, persistence.ErrNotFound
	}
	plans, err := c.r.plans.ListByPhase(ctx, cur.ID)
	if err != nil {
		return nil, err
	}
	active := wbs.ActivePlan(cur.ID, plans)
	if active == nil {
		return nil, persistence.ErrNotFound
	}
	units, err := c.r.units.ListByPlan(ctx, active.ID)
	if err != nil {
		return nil, err
	}
	unit := wbs.CurrentWorkUnit(units)
	if unit == nil {
		return nil, persistence.ErrNotFound
	}
	return unit, nil
}

// txPlanLister / txUnitLister join the P7-10 plan/unit listers to the
// attempt's transaction (the dbrepo repositories expose WithTx; an
// unwired/other lister keeps the unbound read).
type txPlanLister struct {
	p  activeTaskPlanLister
	tx persistence.Transaction
}

func (l *txPlanLister) ListByPhase(ctx context.Context, phaseID uuid.UUID) ([]*wbs.ExecutionPlan, error) {
	if l.tx != nil {
		if repo, ok := l.p.(*dbrepo.PlanRepository); ok {
			return repo.WithTx(l.tx).ListByPhase(ctx, phaseID)
		}
	}
	return l.p.ListByPhase(ctx, phaseID)
}

type txUnitLister struct {
	u  activeTaskUnitLister
	tx persistence.Transaction
}

func (l *txUnitLister) ListByPlan(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error) {
	if l.tx != nil {
		if repo, ok := l.u.(*dbrepo.WorkUnitRepository); ok {
			return repo.WithTx(l.tx).ListByPlan(ctx, planID)
		}
	}
	return l.u.ListByPlan(ctx, planID)
}
