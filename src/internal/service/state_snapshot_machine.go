// P6-19 (WBS 6.22): the StateMachineSnapshot read model. P6-29 (WBS
// 6.39) extends the same service with the visualization read model
// over the current machine.
package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/state"
)

// StateMachineSnapshotService assembles the immutable snapshot of one
// machine version (WBS 6.22): its initial state, all enabled states
// (with role references and state-instruction versions), its
// transitions, and the P6-18 configuration hash of the same definition —
// in ONE transaction when a pool is wired (nil-pool sequential fallback,
// WBS 2.21). It is a pure read: no mutation.
type StateMachineSnapshotService struct {
	machines     persistence.StateMachineRepository
	states       persistence.WorkflowStateRepository
	transitions  persistence.StateTransitionRepository
	instructions persistence.StateInstructionRepository
	assignments  persistence.StateRoleAssignmentRepository
	stateFiles   persistence.StateFileRequirementRepository
	roles        persistence.RoleRepository
	// P6-29 (WBS 6.39) metadata sources for the visualization read
	// model: the role system-prompt version history and the P6-27
	// transition conditions. Both are REQUIRED — the visualization op
	// reports an infrastructure error when either is unwired (the
	// snapshot op never touches them, so prior construction sites are
	// unaffected).
	prompts    persistence.SystemPromptRepository
	conditions persistence.TransitionConditionRepository
	txPool     *pgxpool.Pool
}

// NewStateMachineSnapshotService wires the snapshot service to its
// repositories.
func NewStateMachineSnapshotService(
	machines persistence.StateMachineRepository,
	states persistence.WorkflowStateRepository,
	transitions persistence.StateTransitionRepository,
	instructions persistence.StateInstructionRepository,
	assignments persistence.StateRoleAssignmentRepository,
	stateFiles persistence.StateFileRequirementRepository,
	roles persistence.RoleRepository,
	prompts persistence.SystemPromptRepository,
	conditions persistence.TransitionConditionRepository,
) *StateMachineSnapshotService {
	return &StateMachineSnapshotService{
		machines: machines, states: states, transitions: transitions,
		instructions: instructions, assignments: assignments,
		stateFiles: stateFiles, roles: roles,
		prompts: prompts, conditions: conditions,
	}
}

// WithTxPool wires the transaction pool so the whole assembly runs as
// one transaction (WBS 2.21); with a nil pool the reads run sequentially.
func (s *StateMachineSnapshotService) WithTxPool(pool *pgxpool.Pool) *StateMachineSnapshotService {
	s.txPool = pool
	return s
}

// Snapshot returns the deterministic snapshot of the given machine
// family version (WBS 6.22, T6.22.1–T6.22.5).
func (s *StateMachineSnapshotService) Snapshot(ctx context.Context, machineID string, version int) (*state.StateMachineSnapshot, error) {
	id, err := uuid.Parse(machineID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if version < 1 {
		return nil, ErrInvalidInput
	}
	if s.txPool == nil {
		return s.assemble(ctx, nil, id, version)
	}
	var out *state.StateMachineSnapshot
	if e := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var ee error
		out, ee = s.assemble(ctx, tx, id, version)
		return ee
	}); e != nil {
		return nil, translate(e)
	}
	return out, nil
}

func (s *StateMachineSnapshotService) bind(tx persistence.Transaction) (
	persistence.StateMachineRepository,
	persistence.WorkflowStateRepository,
	persistence.StateTransitionRepository,
	persistence.StateInstructionRepository,
	persistence.StateRoleAssignmentRepository,
	persistence.StateFileRequirementRepository,
	persistence.RoleRepository,
) {
	if tx == nil {
		return s.machines, s.states, s.transitions, s.instructions,
			s.assignments, s.stateFiles, s.roles
	}
	return s.machines.WithTx(tx), s.states.WithTx(tx), s.transitions.WithTx(tx),
		s.instructions.WithTx(tx), s.assignments.WithTx(tx),
		s.stateFiles.WithTx(tx), s.roles.WithTx(tx)
}

// assemble builds the snapshot (and the definition the P6-18 hash runs
// over) from the version's persisted rows.
func (s *StateMachineSnapshotService) assemble(ctx context.Context, tx persistence.Transaction, machineID uuid.UUID, version int) (*state.StateMachineSnapshot, error) {
	machines, states, transitions, instructions, assignments, stateFiles, roles := s.bind(tx)
	mrec, err := machines.GetVersion(ctx, machineID, version)
	if err != nil {
		return nil, translate(err)
	}
	def := &state.MachineDefinition{
		Name: mrec.Name, Description: mrec.Description,
		Version: mrec.Version, Enabled: mrec.Enabled,
	}
	snap := &state.StateMachineSnapshot{
		MachineID: mrec.ID, Version: mrec.Version,
		States:      []state.StateSnapshot{},
		Transitions: []state.TransitionSnapshot{},
	}
	stRecs, err := states.ListByMachine(ctx, mrec.ID, version)
	if err != nil {
		return nil, translate(err)
	}
	keyOf := map[uuid.UUID]string{}
	for _, st := range stRecs {
		keyOf[st.ID] = st.Key
		sd := state.StateConfigDef{
			Key: st.Key, Name: st.Name, Description: st.Description,
			PromptTemplate: st.PromptTemplate, Kind: st.Kind,
			Terminal: st.Terminal, Enabled: st.Enabled,
		}
		ss := state.StateSnapshot{
			Key: st.Key, Name: st.Name, Description: st.Description,
			Kind: st.Kind, Terminal: st.Terminal, Enabled: st.Enabled,
			Roles:      []string{},
			StateFiles: []string{},
		}
		// State instruction at its current version (T6.22.4).
		cur, err := instructions.GetCurrentVersion(ctx, st.ID)
		if err != nil {
			return nil, translate(err)
		}
		if cur > 0 {
			v, err := instructions.GetVersion(ctx, st.ID, cur)
			if err != nil {
				return nil, translate(err)
			}
			sd.InstructionVersion = cur
			sd.InstructionHash = v.ContentHash
			ss.InstructionVersion = cur
			ss.InstructionHash = v.ContentHash
		}
		// Role references in the assignment's ordinal order (T6.22.3).
		asgns, err := assignments.ListByState(ctx, st.ID)
		if err != nil {
			return nil, translate(err)
		}
		for _, a := range asgns {
			r, err := roles.Get(ctx, a.RoleID.String())
			if err != nil {
				return nil, translate(err)
			}
			sd.Roles = append(sd.Roles, r.Key)
			ss.Roles = append(ss.Roles, r.Key)
		}
		// State-file requirements in ordinal order.
		files, err := stateFiles.ListByState(ctx, st.ID)
		if err != nil {
			return nil, translate(err)
		}
		for _, f := range files {
			sd.StateFiles = append(sd.StateFiles, f.Category)
			ss.StateFiles = append(ss.StateFiles, f.Category)
		}
		def.States = append(def.States, sd)
		// All ENABLED states appear in the snapshot (T6.22.1).
		if st.Enabled {
			snap.States = append(snap.States, ss)
		}
	}
	// Transitions, addressed by state key (T6.22.2).
	trans, err := transitions.ListByMachine(ctx, mrec.ID, version)
	if err != nil {
		return nil, translate(err)
	}
	for _, tr := range trans {
		def.Transitions = append(def.Transitions, state.TransitionConfigDef{
			From: keyOf[tr.FromStateID], To: keyOf[tr.ToStateID],
			Outcome: tr.Outcome, Priority: tr.Priority, Enabled: tr.Enabled,
		})
		snap.Transitions = append(snap.Transitions, state.TransitionSnapshot{
			From: keyOf[tr.FromStateID], To: keyOf[tr.ToStateID],
			Outcome: tr.Outcome, Priority: tr.Priority, Enabled: tr.Enabled,
		})
	}
	// Initial state (T6.22.1 scope: the snapshot's initial-state view).
	if mrec.InitialStateID != nil {
		def.InitialKey = keyOf[*mrec.InitialStateID]
		init, err := states.Get(ctx, *mrec.InitialStateID)
		if err != nil {
			return nil, translate(err)
		}
		snap.InitialState = state.StateSnapshot{
			Key: init.Key, Name: init.Name, Description: init.Description,
			Kind: init.Kind, Terminal: init.Terminal, Enabled: init.Enabled,
		}
	}
	// The P6-18 configuration hash of this version's definition
	// (reused, not re-invented).
	snap.Hash = state.MachineConfigurationHash(def)
	return snap, nil
}
