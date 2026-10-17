// P7-12 (WBS 7.17): state-file requirement resolution.
//
// The StateFileRequirementResolutionService is the read-model op that wraps
// the Phase 6 pure primitive (state.ResolveStateFileRequirements,
// union-dedup with first-seen deterministic ordering) end-to-end over
// persisted role + state requirement data.
package service

import (
	"context"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/state"
)

// StateFileRequirementResolutionService resolves a project's required
// state-file categories from its persisted role and state requirements
// (WBS 7.17). It is a pure read model: it reads the repositories and feeds
// the two stored lists to the Phase 6 pure union-dedup primitive — no
// persistence of the union (the union is not a stored source of truth).
//
// Source model and ordering (stated here, pinned by the integration tests):
//   - ROLE sources: the project-scoped roles in repository (key) order, each
//     role's state-file requirements in stored position order.
//   - STATE sources: the project's CURRENT machine version's states (the
//     P6-26 resolveCurrent rule: highest version, tie-broken by smallest
//     family id) in repository (key) order, each state's required
//     categories in stored ordinal order.
//   - The primitive then combines roles first, states second, each category
//     once, in first-seen order.
//
// Disabled-source semantics (T7.17.6, the stated chosen reading): a
// DISABLED SOURCE CONTRIBUTES NOTHING — a disabled role contributes none of
// its requirements, a disabled state contributes none of its required
// categories, and a disabled current machine contributes no state
// requirements at all. The requirement rows stay persisted; exclusion is at
// resolution time.
type StateFileRequirementResolutionService struct {
	roles      persistence.RoleRepository
	roleFiles  persistence.RoleStateFileRepository
	machines   persistence.StateMachineRepository
	states     persistence.WorkflowStateRepository
	stateFiles persistence.StateFileRequirementRepository
}

// NewStateFileRequirementResolutionService wires the resolution op to its
// repositories.
func NewStateFileRequirementResolutionService(
	roles persistence.RoleRepository,
	roleFiles persistence.RoleStateFileRepository,
	machines persistence.StateMachineRepository,
	states persistence.WorkflowStateRepository,
	stateFiles persistence.StateFileRequirementRepository,
) *StateFileRequirementResolutionService {
	return &StateFileRequirementResolutionService{
		roles: roles, roleFiles: roleFiles, machines: machines,
		states: states, stateFiles: stateFiles,
	}
}

// ResolveStateFileRequirements returns the project's resolved state-file
// requirement categories: the deduplicated, deterministic union of its
// project-scoped roles' requirements and its current machine's enabled
// states' requirements (WBS 7.17, T7.17.1-T7.17.6). A project with no
// requirement rows resolves to an empty (non-nil) slice.
func (s *StateFileRequirementResolutionService) ResolveStateFileRequirements(ctx context.Context, projectID string) ([]string, error) {
	return s.resolve(ctx, nil, projectID)
}

// ResolveStateFileRequirementsTx is ResolveStateFileRequirements with every
// repository read joined to the given transaction (P7-18: the
// snapshot-creation op needs the resolution inside its one consistent
// view). tx must be non-nil.
func (s *StateFileRequirementResolutionService) ResolveStateFileRequirementsTx(ctx context.Context, tx persistence.Transaction, projectID string) ([]string, error) {
	return s.resolve(ctx, tx, projectID)
}

func (s *StateFileRequirementResolutionService) resolve(ctx context.Context, tx persistence.Transaction, projectID string) ([]string, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	var roleReq, stateReq []string
	if tx == nil {
		roleReq, err = s.roleRequirements(ctx, pid)
		if err != nil {
			return nil, translate(err)
		}
		stateReq, err = s.stateRequirements(ctx, pid)
		if err != nil {
			return nil, translate(err)
		}
	} else {
		roleReq, err = s.roleRequirementsTx(ctx, tx, pid)
		if err != nil {
			return nil, translate(err)
		}
		stateReq, err = s.stateRequirementsTx(ctx, tx, pid)
		if err != nil {
			return nil, translate(err)
		}
	}
	return state.ResolveStateFileRequirements(roleReq, stateReq), nil
}

// roleRequirements lists the project-scoped roles' state-file
// requirements in (role key, position) order, skipping disabled roles
// (T7.17.6: a disabled source contributes nothing).
func (s *StateFileRequirementResolutionService) roleRequirements(ctx context.Context, projectID uuid.UUID) ([]string, error) {
	roles, err := s.roles.ListByScope(ctx, &projectID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range roles {
		if !r.Enabled {
			continue
		}
		files, err := s.roleFiles.ListByRole(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			out = append(out, f.StateFileKey)
		}
	}
	return out, nil
}

// stateRequirements lists the project's CURRENT machine version's enabled
// states' required categories in (state key, ordinal) order, skipping
// disabled states; a disabled current machine contributes nothing
// (T7.17.6).
func (s *StateFileRequirementResolutionService) stateRequirements(ctx context.Context, projectID uuid.UUID) ([]string, error) {
	machine, err := s.currentMachine(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if machine == nil || !machine.Enabled {
		return nil, nil
	}
	states, err := s.states.ListByMachine(ctx, machine.ID, machine.Version)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, st := range states {
		if !st.Enabled {
			continue
		}
		reqs, err := s.stateFiles.ListByState(ctx, st.ID)
		if err != nil {
			return nil, err
		}
		for _, req := range reqs {
			out = append(out, req.Category)
		}
	}
	return out, nil
}

// currentMachine returns the project's current machine version: the
// highest-version row of the project's machines, tie-broken by smallest
// family id (the P6-26 resolveCurrent rule); ErrNotFound when the project
// has no machine.
func (s *StateFileRequirementResolutionService) currentMachine(ctx context.Context, projectID uuid.UUID) (*persistence.MachineRecord, error) {
	list, err := s.machines.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return pickCurrentMachine(list), nil
}

// pickCurrentMachine is the shared selection rule (P6-26): highest version,
// tie-broken by smallest family id; nil (and the caller's ErrNotFound)
// when the project has no machine.
func pickCurrentMachine(list []*persistence.MachineRecord) *persistence.MachineRecord {
	var best *persistence.MachineRecord
	for _, m := range list {
		if best == nil ||
			m.Version > best.Version ||
			(m.Version == best.Version && m.FamilyID.String() < best.FamilyID.String()) {
			best = m
		}
	}
	return best
}

// roleRequirementsTx is roleRequirements with the reads joined to tx
// (P7-18).
func (s *StateFileRequirementResolutionService) roleRequirementsTx(ctx context.Context, tx persistence.Transaction, projectID uuid.UUID) ([]string, error) {
	roles, err := s.roles.WithTx(tx).ListByScope(ctx, &projectID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range roles {
		if !r.Enabled {
			continue
		}
		files, err := s.roleFiles.WithTx(tx).ListByRole(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			out = append(out, f.StateFileKey)
		}
	}
	return out, nil
}

// stateRequirementsTx is stateRequirements with the reads joined to tx
// (P7-18).
func (s *StateFileRequirementResolutionService) stateRequirementsTx(ctx context.Context, tx persistence.Transaction, projectID uuid.UUID) ([]string, error) {
	list, err := s.machines.WithTx(tx).ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	machine := pickCurrentMachine(list)
	if machine == nil {
		return nil, nil
	}
	if !machine.Enabled {
		return nil, nil
	}
	states, err := s.states.WithTx(tx).ListByMachine(ctx, machine.ID, machine.Version)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, st := range states {
		if !st.Enabled {
			continue
		}
		reqs, err := s.stateFiles.WithTx(tx).ListByState(ctx, st.ID)
		if err != nil {
			return nil, err
		}
		for _, req := range reqs {
			out = append(out, req.Category)
		}
	}
	return out, nil
}
