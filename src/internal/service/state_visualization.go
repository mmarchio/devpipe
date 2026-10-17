// P6-29 (WBS 6.39): the StateMachineVisualization read model — a
// service read over the CURRENT machine (no REST surface in WBS 6.39).
// It extends the P6-19 snapshot assembly path with the Phase 5 version
// repositories (system prompts, state instructions), the state-file
// requirements, and the P6-27 transition conditions; no parallel
// definition assembly.
package service

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/state"
)

// Visualization returns the deterministic visualization read model of
// the project's CURRENT machine (WBS 6.39, T6.39.1-T6.39.5): every
// state as a node and every transition as an edge, in deterministic
// order. The current machine is resolved exactly like the P6-25
// current-machine family (the project's family whose MAX-version row is
// highest; a project with no machine reports ErrNotFound). All reads
// run in ONE transaction when a pool is wired (nil-pool sequential
// fallback, WBS 2.21).
func (s *StateMachineSnapshotService) Visualization(ctx context.Context, projectID string) (*state.StateMachineVisualization, error) {
	if s.prompts == nil || s.conditions == nil {
		return nil, ErrInfrastructure
	}
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	if s.txPool == nil {
		return s.assembleVisualization(ctx, nil, pid)
	}
	var out *state.StateMachineVisualization
	if e := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var ee error
		out, ee = s.assembleVisualization(ctx, tx, pid)
		return ee
	}); e != nil {
		return nil, translate(e)
	}
	return out, nil
}

func (s *StateMachineSnapshotService) bindViz(tx persistence.Transaction) (
	persistence.StateMachineRepository,
	persistence.WorkflowStateRepository,
	persistence.StateTransitionRepository,
	persistence.StateInstructionRepository,
	persistence.StateRoleAssignmentRepository,
	persistence.StateFileRequirementRepository,
	persistence.RoleRepository,
	persistence.SystemPromptRepository,
) {
	machines, states, transitions, instructions, assignments, stateFiles, roles := s.bind(tx)
	prompts := s.prompts
	if tx != nil {
		prompts = s.prompts.WithTx(tx)
	}
	// The edge CONDITION comes from the transition list itself (the
	// repository LEFT JOINs transition_conditions); the conditions
	// repository is wired for the nil-guard only.
	return machines, states, transitions, instructions, assignments, stateFiles, roles, prompts
}

// assembleVisualization builds the node/edge model from the current
// machine's persisted rows.
func (s *StateMachineSnapshotService) assembleVisualization(ctx context.Context, tx persistence.Transaction, projectID uuid.UUID) (*state.StateMachineVisualization, error) {
	machines, states, transitions, instructions, assignments, stateFiles, roles, prompts := s.bindViz(tx)
	// Current machine: the project's family whose MAX-version row is
	// highest (the P6-25 resolution); tie on version -> smallest family
	// id, so the resolution is deterministic.
	list, err := machines.ListByProject(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	var best *persistence.MachineRecord
	for _, m := range list {
		if best == nil ||
			m.Version > best.Version ||
			(m.Version == best.Version && m.FamilyID.String() < best.FamilyID.String()) {
			best = m
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	viz := &state.StateMachineVisualization{
		MachineID: best.ID, Version: best.Version,
		Nodes: []state.VizNode{}, Edges: []state.VizEdge{},
	}
	stRecs, err := states.ListByMachine(ctx, best.ID, best.Version)
	if err != nil {
		return nil, translate(err)
	}
	// T6.39.1: every state of the current machine is a node (no
	// enabled-only filter — the editor renders the full definition).
	for _, st := range stRecs {
		node := state.VizNode{
			Key: st.Key, Terminal: st.Terminal,
			PromptTemplate: st.PromptTemplate, Roles: []state.VizRole{},
			StateFiles: []string{},
		}
		// T6.39.3: the node's roles resolve from the persisted role
		// assignments (ordinal order) plus the role_system_prompts
		// history (current version per role; 0 = none).
		asgns, err := assignments.ListByState(ctx, st.ID)
		if err != nil {
			return nil, translate(err)
		}
		for _, a := range asgns {
			r, err := roles.Get(ctx, a.RoleID.String())
			if err != nil {
				return nil, translate(err)
			}
			pv, err := prompts.GetCurrentVersion(ctx, a.RoleID.String())
			if err != nil {
				return nil, translate(err)
			}
			node.Roles = append(node.Roles, state.VizRole{Key: r.Key, SystemPromptVersion: pv})
		}
		// T6.39.4: the state-instruction version resolves from the
		// instruction_versions append-only history; nil when absent.
		cur, err := instructions.GetCurrentVersion(ctx, st.ID)
		if err != nil {
			return nil, translate(err)
		}
		if cur > 0 {
			node.InstructionVersion = &cur
		}
		files, err := stateFiles.ListByState(ctx, st.ID)
		if err != nil {
			return nil, translate(err)
		}
		for _, f := range files {
			node.StateFiles = append(node.StateFiles, f.Category)
		}
		viz.Nodes = append(viz.Nodes, node)
	}
	// T6.39.2: every transition of the current machine is an edge,
	// addressed by state key; the CONDITION is the P6-27 structured
	// condition (nil when the transition has none).
	keyOf := map[uuid.UUID]string{}
	for _, st := range stRecs {
		keyOf[st.ID] = st.Key
	}
	trans, err := transitions.ListByMachine(ctx, best.ID, best.Version)
	if err != nil {
		return nil, translate(err)
	}
	for _, tr := range trans {
		edge := state.VizEdge{
			From: keyOf[tr.FromStateID], Outcome: tr.Outcome,
			Priority: tr.Priority, Destination: keyOf[tr.ToStateID],
		}
		if tr.Condition != nil {
			edge.Condition = &state.VizEdgeCondition{
				Field: tr.Condition.Field, Operator: tr.Condition.Operator,
				Literal: tr.Condition.Literal, ContextField: tr.Condition.ContextField,
			}
		}
		viz.Edges = append(viz.Edges, edge)
	}
	// T6.39.5: deterministic ordering — nodes by state key ascending;
	// edges by (outcome, priority, from key, destination key).
	sort.Slice(viz.Nodes, func(i, j int) bool { return viz.Nodes[i].Key < viz.Nodes[j].Key })
	sort.Slice(viz.Edges, func(i, j int) bool {
		a, b := viz.Edges[i], viz.Edges[j]
		if a.Outcome != b.Outcome {
			return a.Outcome < b.Outcome
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.Destination < b.Destination
	})
	return viz, nil
}
