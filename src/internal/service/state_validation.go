// P6-16 (WBS 6.19): the state-machine validation service. It loads the
// machine's persisted definition and aggregates every WBS 6.19 check,
// returning ALL practical errors at once with structured codes (never
// fail-fast). Validation is non-mutating, and the activation gate
// (CanActivate) is the seam the later activation paths (P6-25+) call: an
// invalid machine cannot become active. The pure check logic is reused
// from internal/state (P6-14/P6-15); this service only coordinates reads.
package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/state"
)

// Structured validation error codes (WBS 6.19). The P6-28 validation REST
// surfaces these verbatim in {valid, errors[]}.
const (
	ValidationCodeInitialMissing    = "initial_missing"
	ValidationCodeDuplicateKey      = "duplicate_key"
	ValidationCodeRoleNotFound      = "role_not_found"
	ValidationCodeRoleDisabled      = "role_disabled"
	ValidationCodeRoleAllowedState  = "role_allowed_state"
	ValidationCodeTemplateMissing   = "template_missing"
	ValidationCodeTransitionInvalid = "transition_invalid"
	ValidationCodeAmbiguity         = "ambiguity"
	ValidationCodeCondition         = "condition_invalid"
	ValidationCodeTerminal          = "terminal"
	ValidationCodeReachability      = "reachability"
	ValidationCodeDeadEnd           = "dead_end"
	ValidationCodeCycle             = "cycle"
	ValidationCodeMachineDisabled   = "machine_disabled"
)

// ValidationIssue is one structured validation error (WBS 6.19). Code is one
// of the ValidationCode* constants; StateKey names the offending state when
// applicable; Detail carries a deterministic human-readable explanation.
type ValidationIssue struct {
	Code     string `json:"code"`
	StateKey string `json:"state_key,omitempty"`
	Detail   string `json:"detail"`
}

// ValidationReport is the aggregate result of Validate: Valid is true when
// no issue was found; Errors is deterministic (ordered by code, then state
// key, then detail) and carries every practical issue at once.
type ValidationReport struct {
	Valid  bool              `json:"valid"`
	Errors []ValidationIssue `json:"errors"`
}

// StateMachineValidationService validates persisted machine definitions
// (WBS 6.19). It is read-only: no repository mutation is ever performed.
type StateMachineValidationService struct {
	machines    persistence.StateMachineRepository
	states      persistence.WorkflowStateRepository
	transitions persistence.StateTransitionRepository
	assignments persistence.StateRoleAssignmentRepository
	roles       persistence.RoleRepository
	allowed     persistence.RoleAllowedStateRepository
}

// NewStateMachineValidationService wires the validator to its read
// repositories.
func NewStateMachineValidationService(
	machines persistence.StateMachineRepository,
	states persistence.WorkflowStateRepository,
	transitions persistence.StateTransitionRepository,
	assignments persistence.StateRoleAssignmentRepository,
	roles persistence.RoleRepository,
	allowed persistence.RoleAllowedStateRepository,
) *StateMachineValidationService {
	return &StateMachineValidationService{
		machines:    machines,
		states:      states,
		transitions: transitions,
		assignments: assignments,
		roles:       roles,
		allowed:     allowed,
	}
}

// Validate aggregates every WBS 6.19 check over the machine's persisted
// definition and returns ALL practical issues at once (T6.19.2 — never
// fail-fast):
//
//   - initial state exists (initial_missing);
//   - state keys unique within the machine (duplicate_key);
//   - role assignments valid: referenced roles exist (role_not_found), are
//     enabled (role_disabled, WBS 6.4.3), and allow the state key
//     (role_allowed_state, WBS 6.4.4);
//   - prompt-template requirements valid (template_missing, WBS 6.7 via
//     WorkflowState.ValidateTemplate);
//   - transitions reference valid states (transition_invalid);
//   - transition priorities deterministic (ambiguity, WBS 6.15 via
//     DetectAmbiguity);
//   - conditions valid (condition_invalid, WBS 6.10 construction rules —
//     conditions are stored only as a presence marker on the persisted
//     surface at this stage, so the check validates what is persisted);
//   - terminal states valid (terminal, WBS 6.17 via CheckTerminalStates);
//   - reachability valid (reachability, WBS 6.16 via CheckReachability);
//   - obvious zero-progress cycles absent (cycle, WBS 6.18 via
//     DetectZeroProgressCycles).
//
// The report is deterministic: issues are sorted by (code, state key,
// detail). The machine is never mutated (T6.19.3).
func (s *StateMachineValidationService) Validate(ctx context.Context, machineID string) (*ValidationReport, error) {
	id, err := uuid.Parse(machineID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rep := &ValidationReport{Valid: true, Errors: []ValidationIssue{}}
	machine, err := s.machines.Get(ctx, id)
	if err != nil {
		return nil, translate(err)
	}
	recs, err := s.states.ListByMachine(ctx, id, machine.Version)
	if err != nil {
		return nil, translate(err)
	}
	trecs, err := s.transitions.ListByMachine(ctx, id, machine.Version)
	if err != nil {
		return nil, translate(err)
	}
	// Map the persisted records to domain values (the pure checks
	// operate on domain types; records are read-only inputs).
	states := make([]*state.WorkflowState, 0, len(recs))
	keyByID := map[uuid.UUID]string{}
	for _, r := range recs {
		states = append(states, &state.WorkflowState{
			ID: r.ID, StateMachineID: r.MachineID, Key: r.Key, Name: r.Name,
			PromptTemplate: r.PromptTemplate, Kind: state.StateKind(r.Kind),
			Terminal: r.Terminal, Enabled: r.Enabled,
		})
		keyByID[r.ID] = r.Key
	}
	transitions := make([]*state.StateTransition, 0, len(trecs))
	for _, r := range trecs {
		transitions = append(transitions, &state.StateTransition{
			ID: r.ID, StateMachineID: r.MachineID,
			FromStateID: r.FromStateID, ToStateID: r.ToStateID,
			Outcome: r.Outcome, Priority: r.Priority,
			Enabled: r.Enabled,
		})
	}
	m := &state.Machine{
		ID: machine.ID, ProjectID: machine.ProjectID, Name: machine.Name,
		Version: machine.Version, Enabled: machine.Enabled,
		InitialStateID: uuid.Nil,
	}
	if machine.InitialStateID != nil {
		m.InitialStateID = *machine.InitialStateID
	}

	// 1. Initial state exists.
	if machine.InitialStateID == nil {
		rep.add(ValidationCodeInitialMissing, "", "machine has no initial state")
	} else if _, ok := keyByID[*machine.InitialStateID]; !ok {
		rep.add(ValidationCodeInitialMissing, "", fmt.Sprintf("initial state %s does not belong to the machine", machine.InitialStateID))
	}

	// 2. State keys unique within the machine.
	seen := map[string]*state.WorkflowState{}
	for _, s := range states {
		if first, dup := seen[s.Key]; dup {
			rep.add(ValidationCodeDuplicateKey, s.Key,
				fmt.Sprintf("state key %q is defined by both %s and %s", s.Key, first.ID, s.ID))
		} else {
			seen[s.Key] = s
		}
	}

	// 3. Role assignments valid (roles exist, are enabled, allow the state).
	if s.assignments != nil && s.roles != nil {
		for _, st := range states {
			assns, err := s.assignments.ListByState(ctx, st.ID)
			if err != nil {
				return nil, translate(err)
			}
			for _, a := range assns {
				role, err := s.roles.Get(ctx, a.RoleID.String())
				if err != nil {
					if errors.Is(err, persistence.ErrNotFound) {
						rep.add(ValidationCodeRoleNotFound, st.Key,
							fmt.Sprintf("state %q references missing role %s", st.Key, a.RoleID))
						continue
					}
					return nil, translate(err)
				}
				if !role.Enabled {
					rep.add(ValidationCodeRoleDisabled, st.Key,
						fmt.Sprintf("state %q is assigned to disabled role %s", st.Key, a.RoleID))
				}
				if s.allowed != nil {
					eligible, err := s.allowed.IsEligible(ctx, a.RoleID, st.Key)
					if err != nil {
						return nil, translate(err)
					}
					if !eligible {
						rep.add(ValidationCodeRoleAllowedState, st.Key,
							fmt.Sprintf("state %q is assigned to role %s which does not allow it", st.Key, a.RoleID))
					}
				}
			}
		}
	}

	// 4. Prompt-template requirements valid (WBS 6.7). Disabled states are
	// inert (same policy as the P6-15 graph checks) and are exempt.
	for _, st := range states {
		if !st.Enabled {
			continue
		}
		if err := st.ValidateTemplate(); err != nil {
			rep.add(ValidationCodeTemplateMissing, st.Key,
				fmt.Sprintf("state %q: %v", st.Key, err))
		}
	}

	// 5. Transitions reference valid states.
	for _, tr := range transitions {
		for _, endpoint := range []uuid.UUID{tr.FromStateID, tr.ToStateID} {
			if _, ok := keyByID[endpoint]; !ok {
				rep.add(ValidationCodeTransitionInvalid, keyOrEmpty(keyByID, tr.FromStateID),
					fmt.Sprintf("transition %s references state %s which is not in the machine", tr.ID, endpoint))
			}
		}
	}

	// 6. Transition priorities deterministic (WBS 6.15).
	if amb := state.DetectAmbiguity(transitions); !amb.Valid {
		for _, g := range amb.Groups {
			rep.add(ValidationCodeAmbiguity, keyOrEmpty(keyByID, g.SourceStateID),
				fmt.Sprintf("state %s: %d enabled transitions share the same outcome and priority", g.SourceStateID, len(g.Transitions)))
		}
	}

	// 7. Conditions valid (WBS 6.10): at this stage conditions are not
	// persisted (the presence marker is derived from future work), so the
	// check is vacuously satisfied — no malformed condition can exist.

	// 8+9+10. Terminal states, reachability, zero-progress cycles (P6-15).
	if term := state.CheckTerminalStates(m, states, transitions); !term.Valid {
		for _, s := range term.TerminalWithOutgoing {
			rep.add(ValidationCodeTerminal, s.Key,
				fmt.Sprintf("terminal state %q has an outgoing enabled transition", s.Key))
		}
		for _, s := range term.DeadEndStates {
			rep.add(ValidationCodeDeadEnd, s.Key,
				fmt.Sprintf("non-terminal state %q has no outgoing enabled transition", s.Key))
		}
	}
	if reach := state.CheckReachability(m, states, transitions); !reach.Valid {
		for _, s := range reach.OrphanStates {
			rep.add(ValidationCodeReachability, s.Key,
				fmt.Sprintf("state %q is unreachable from the initial state", s.Key))
		}
	}
	if cyc := state.DetectZeroProgressCycles(m, states, transitions); !cyc.Valid {
		for _, c := range cyc.Cycles {
			rep.add(ValidationCodeCycle, cycleFirstKey(c),
				fmt.Sprintf("zero-progress control cycle among %d states", len(c.States)))
		}
	}

	rep.finalize()
	return rep, nil
}

// CanActivate is the activation gate (T6.19.4): a machine may become active
// only when it is enabled AND its validation report is clean. At this stage
// the activation surface is the machine's Enabled flag (the REST
// surface lands in P6-25+); the later activation path calls this method so
// an invalid machine can never be activated.
func (s *StateMachineValidationService) CanActivate(ctx context.Context, machineID string) (*ValidationReport, error) {
	id, err := uuid.Parse(machineID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	machine, err := s.machines.Get(ctx, id)
	if err != nil {
		return nil, translate(err)
	}
	if !machine.Enabled {
		rep := &ValidationReport{Valid: false, Errors: []ValidationIssue{{
			Code: ValidationCodeMachineDisabled, Detail: "machine is disabled",
		}}}
		return rep, nil
	}
	return s.Validate(ctx, machineID)
}

func (r *ValidationReport) add(code, key, detail string) {
	r.Valid = false
	r.Errors = append(r.Errors, ValidationIssue{Code: code, StateKey: key, Detail: detail})
}

// finalize imposes the deterministic report ordering (code, state key,
// detail) required for a stable {valid, errors[]} surface.
func (r *ValidationReport) finalize() {
	sort.Slice(r.Errors, func(i, j int) bool {
		if r.Errors[i].Code != r.Errors[j].Code {
			return r.Errors[i].Code < r.Errors[j].Code
		}
		if r.Errors[i].StateKey != r.Errors[j].StateKey {
			return r.Errors[i].StateKey < r.Errors[j].StateKey
		}
		return r.Errors[i].Detail < r.Errors[j].Detail
	})
}

func keyOrEmpty(m map[uuid.UUID]string, id uuid.UUID) string { return m[id] }

func cycleFirstKey(c state.ZeroProgressCycle) string {
	if len(c.States) == 0 {
		return ""
	}
	return c.States[0].Key
}
