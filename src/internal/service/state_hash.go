// P6-18 (WBS 6.21): service coordination for the deterministic machine
// configuration hash. The hash itself is the pure domain function
// state.MachineConfigurationHash; this service reads one machine version's
// persisted definition through the existing version-scoped repositories
// (no duplication of the P6-17 versioning logic) and feeds it to the hash.
package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/state"
)

// MachineConfigHashService computes the configuration hash of one machine
// version (WBS 6.21) from its persisted definition.
type MachineConfigHashService struct {
	machines     persistence.StateMachineRepository
	states       persistence.WorkflowStateRepository
	transitions  persistence.StateTransitionRepository
	instructions persistence.StateInstructionRepository
	assignments  persistence.StateRoleAssignmentRepository
	stateFiles   persistence.StateFileRequirementRepository
	roles        persistence.RoleRepository
	txPool       *pgxpool.Pool
}

// NewMachineConfigHashService wires the hash service to its repositories.
func NewMachineConfigHashService(
	machines persistence.StateMachineRepository,
	states persistence.WorkflowStateRepository,
	transitions persistence.StateTransitionRepository,
	instructions persistence.StateInstructionRepository,
	assignments persistence.StateRoleAssignmentRepository,
	stateFiles persistence.StateFileRequirementRepository,
	roles persistence.RoleRepository,
) *MachineConfigHashService {
	return &MachineConfigHashService{
		machines: machines, states: states, transitions: transitions,
		instructions: instructions, assignments: assignments,
		stateFiles: stateFiles, roles: roles,
	}
}

// WithTxPool wires the transaction pool so the read runs as one
// transaction (WBS 2.21); with a nil pool the reads run sequentially.
func (s *MachineConfigHashService) WithTxPool(pool *pgxpool.Pool) *MachineConfigHashService {
	s.txPool = pool
	return s
}

// Hash returns the deterministic configuration hash of the machine
// version's persisted definition (WBS 6.21). machineID addresses the
// machine family (WBS 6.20); the version's definition is assembled
// from the machine row, its states (with their current instruction
// version/hash, role assignments, and state-file requirements), and its
// transitions.
func (s *MachineConfigHashService) Hash(ctx context.Context, machineID string, version int) (string, error) {
	id, err := uuid.Parse(machineID)
	if err != nil {
		return "", ErrInvalidInput
	}
	if version < 1 {
		return "", ErrInvalidInput
	}
	if s.txPool == nil {
		return s.hashOnce(ctx, nil, id, version)
	}
	var out string
	if e := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var ee error
		out, ee = s.hashOnce(ctx, tx, id, version)
		return ee
	}); e != nil {
		return "", translate(e)
	}
	return out, nil
}

func (s *MachineConfigHashService) bind(tx persistence.Transaction) (
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

// hashOnce assembles the version's definition and hashes it.
func (s *MachineConfigHashService) hashOnce(ctx context.Context, tx persistence.Transaction, machineID uuid.UUID, version int) (string, error) {
	machines, states, transitions, instructions, assignments, stateFiles, roles := s.bind(tx)
	mrec, err := machines.GetVersion(ctx, machineID, version)
	if err != nil {
		return "", translate(err)
	}
	def := &state.MachineDefinition{
		Name: mrec.Name, Description: mrec.Description,
		Version: mrec.Version, Enabled: mrec.Enabled,
	}
	stRecs, err := states.ListByMachine(ctx, mrec.ID, version)
	if err != nil {
		return "", translate(err)
	}
	keyOf := map[uuid.UUID]string{}
	for _, st := range stRecs {
		keyOf[st.ID] = st.Key
		sd := state.StateConfigDef{
			Key: st.Key, Name: st.Name, Description: st.Description,
			PromptTemplate: st.PromptTemplate, Kind: st.Kind,
			Terminal: st.Terminal, Enabled: st.Enabled,
		}
		cur, err := instructions.GetCurrentVersion(ctx, st.ID)
		if err != nil {
			return "", translate(err)
		}
		if cur > 0 {
			v, err := instructions.GetVersion(ctx, st.ID, cur)
			if err != nil {
				return "", translate(err)
			}
			sd.InstructionVersion = cur
			sd.InstructionHash = v.ContentHash
		}
		asgns, err := assignments.ListByState(ctx, st.ID)
		if err != nil {
			return "", translate(err)
		}
		for _, a := range asgns {
			r, err := roles.Get(ctx, a.RoleID.String())
			if err != nil {
				return "", translate(err)
			}
			sd.Roles = append(sd.Roles, r.Key)
		}
		files, err := stateFiles.ListByState(ctx, st.ID)
		if err != nil {
			return "", translate(err)
		}
		for _, f := range files {
			sd.StateFiles = append(sd.StateFiles, f.Category)
		}
		def.States = append(def.States, sd)
	}
	trans, err := transitions.ListByMachine(ctx, mrec.ID, version)
	if err != nil {
		return "", translate(err)
	}
	for _, tr := range trans {
		def.Transitions = append(def.Transitions, state.TransitionConfigDef{
			From: keyOf[tr.FromStateID], To: keyOf[tr.ToStateID],
			Outcome: tr.Outcome, Priority: tr.Priority, Enabled: tr.Enabled,
		})
	}
	if mrec.InitialStateID != nil {
		def.InitialKey = keyOf[*mrec.InitialStateID]
	}
	return state.MachineConfigurationHash(def), nil
}
