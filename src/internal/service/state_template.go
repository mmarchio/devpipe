// StateTemplateService seeds the default development-workflow state-machine
// template as data (WBS 6.3) and copies it into a project. It is a
// service-layer op only — WBS 6.3 declares no REST endpoint for it. The
// machine, its states, and its transitions are stored as rows; no
// orchestration code is added. Seeding is idempotent (keyed by a stable
// machine name inside a canonical system project) and copy-to-project
// materializes a fresh machine with new IDs and the same topology.
package service

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/state"
)

// Stable identity constants (P6-11). The template is keyed by (system
// project name, machine name) so re-seeding finds the existing rows and
// leaves them unchanged (T6.3.2).
const (
	defaultSystemProjectName = "Overseer State Template"
	stateTemplateLockKey     = "state-template:default"
)

// templateDef is one workflow template: its machine name (the identity
// seeding and copying are keyed by) and definition.
type templateDef struct {
	name, description string
	states            func() []stateSpec
	transitions       func() []transitionSpec
}

// statefileTemplate is the default workflow new projects receive.
// v2 added the output-format skill step (SKILL → ROUTE) every execution
// starts with; v3 adds the replan of a task the overseer's validation keeps
// failing the same way (VALIDATE → REPLAN → REPLAN_CHECK → IMPLEMENT).
// Projects created earlier keep the workflow they copied.
var statefileTemplate = templateDef{name: "Statefile Development Workflow v3",
	description: "Command-driven development workflow over <working directory>/statefiles",
	states:      defaultStateSpecs, transitions: defaultTransitionSpecs}

// composedTemplate is the earlier composed-prompt workflow (WBS 6.3),
// kept under its original name for the projects that run it.
var composedTemplate = templateDef{name: "Default Development Workflow",
	description: "Default autonomous development workflow (WBS 6.3)",
	states:      composedStateSpecs, transitions: composedTransitionSpecs}

// StateTemplateService coordinates the state-machine, workflow-state, and
// transition repositories (plus the project repository for the template's
// canonical home) to seed and copy the default workflow.
type StateTemplateService struct {
	machines    persistence.StateMachineRepository
	states      persistence.WorkflowStateRepository
	transitions persistence.StateTransitionRepository
	projects    persistence.ProjectRepository
	txPool      *pgxpool.Pool
}

// NewStateTemplateService wires the service to the four repositories.
func NewStateTemplateService(
	machines persistence.StateMachineRepository,
	states persistence.WorkflowStateRepository,
	transitions persistence.StateTransitionRepository,
	projects persistence.ProjectRepository,
) *StateTemplateService {
	return &StateTemplateService{
		machines:    machines,
		states:      states,
		transitions: transitions,
		projects:    projects,
	}
}

// WithTxPool wires the transaction pool used for the atomic seed/copy path
// (WBS 6.3). When nil, operations run sequentially (single-goroutine test
// scope).
func (s *StateTemplateService) WithTxPool(pool *pgxpool.Pool) *StateTemplateService {
	s.txPool = pool
	return s
}

// stateSpec is one state of the default template (WBS 6.3 + 6.7).
type stateSpec struct {
	key      string
	kind     state.StateKind
	template string
	terminal bool
}

// transitionSpec is one directed edge of the default template (WBS 6.3).
type transitionSpec struct {
	from    string
	outcome string
	to      string
}

// defaultStateSpecs enumerates the statefile workflow's states. Each
// action state sends one command (a CMD_ template rendered as is); the
// team works in <working directory>/statefiles and the *_CHECK control
// states read those files back to decide the transition.
func defaultStateSpecs() []stateSpec {
	return []stateSpec{
		{key: "PROJECT_INIT", kind: state.KindControl},
		// "create the <project>-output-format skill": Hermes saves the
		// project's required response formatting as a skill, then ROUTE
		// picks the work (plan / accept / validate).
		{key: "SKILL", kind: state.KindAction, template: "CMD_SKILL"},
		{key: "ROUTE", kind: state.KindControl},
		// "plan phase <N>": ARCHITECT → PLAN.md, PLANNER → BACKLOG.md.
		{key: "PLAN", kind: state.KindAction, template: "CMD_PLAN"},
		{key: "PLAN_CHECK", kind: state.KindControl},
		// PROJECT_MANAGER: archive the finished task, load the next one.
		{key: "ACCEPTANCE", kind: state.KindAction, template: "CMD_ACCEPTANCE"},
		{key: "ACCEPTANCE_CHECK", kind: state.KindControl},
		// ENGINEER: "implement task <K>" / "continue".
		{key: "IMPLEMENT", kind: state.KindAction, template: "CMD_IMPLEMENT"},
		{key: "CONTINUE", kind: state.KindAction, template: "CMD_CONTINUE"},
		{key: "IMPLEMENT_CHECK", kind: state.KindControl},
		// TESTER: "test task <K>"; all criteria met → COMPLETE.
		{key: "TEST", kind: state.KindAction, template: "CMD_TEST"},
		{key: "TEST_CHECK", kind: state.KindControl},
		// Overseer-run deterministic validation before acceptance.
		{key: "VALIDATE", kind: state.KindValidation, template: "VALIDATE_WORK_UNIT"},
		// ARCHITECT: "have the architect replan task <K>" after repeated
		// identical validation failures; the check adopts the new task.
		{key: "REPLAN", kind: state.KindAction, template: "CMD_REPLAN"},
		{key: "REPLAN_CHECK", kind: state.KindControl},
		{key: "PHASE_VALIDATE", kind: state.KindValidation, template: "PHASE_VALIDATE"},
		{key: "PHASE_DONE", kind: state.KindControl},
		{key: "MORE_PHASES", kind: state.KindDecision, template: "MORE_PHASES"},
		// P16-78 (WBS 16.10, 16.86–16.156): project finalization — the last
		// phase leads into final hardening, final validation and the final
		// checkpoint, never directly to PROJECT_DONE.
		{key: "FINAL_HARDENING", kind: state.KindControl},
		{key: "HARDENING_FIX", kind: state.KindAction, template: "HARDENING_FIX"},
		{key: "FINAL_VALIDATE", kind: state.KindValidation, template: "FINAL_VALIDATE"},
		{key: "FINAL_CHECKPOINT", kind: state.KindControl},
		{key: "PROJECT_DONE", kind: state.KindTerminal, terminal: true},
	}
}

// defaultTransitionSpecs enumerates the statefile workflow's edges.
// Outcomes are the explicit decision tokens for each branch.
func defaultTransitionSpecs() []transitionSpec {
	return []transitionSpec{
		// Every execution first has Hermes save the output-format skill.
		{from: "PROJECT_INIT", outcome: "PROCEED", to: "SKILL"},
		{from: "SKILL", outcome: "SUBMIT", to: "ROUTE"},
		{from: "ROUTE", outcome: "RETRY", to: "SKILL"},
		// Routing (P15-21): the control-state resolver tries
		// PLAN_REQUIRED / PLAN_READY / WORK_COMPLETE before PROCEED. A
		// ready plan resumes with the PROJECT_MANAGER (ACCEPTANCE).
		{from: "ROUTE", outcome: "PROCEED", to: "PLAN"},
		{from: "ROUTE", outcome: "PLAN_REQUIRED", to: "PLAN"},
		{from: "ROUTE", outcome: "PLAN_READY", to: "ACCEPTANCE"},
		{from: "ROUTE", outcome: "WORK_COMPLETE", to: "PHASE_VALIDATE"},
		{from: "PLAN", outcome: "SUBMIT", to: "PLAN_CHECK"},
		{from: "PLAN_CHECK", outcome: "ACCEPTED", to: "ACCEPTANCE"},
		{from: "PLAN_CHECK", outcome: "REJECTED", to: "PLAN"},
		{from: "ACCEPTANCE", outcome: "SUBMIT", to: "ACCEPTANCE_CHECK"},
		{from: "ACCEPTANCE_CHECK", outcome: "NEXT", to: "IMPLEMENT"},
		{from: "ACCEPTANCE_CHECK", outcome: "TESTED", to: "VALIDATE"},
		{from: "ACCEPTANCE_CHECK", outcome: "PHASE_COMPLETE", to: "PHASE_VALIDATE"},
		{from: "ACCEPTANCE_CHECK", outcome: "RETRY", to: "ACCEPTANCE"},
		{from: "IMPLEMENT", outcome: "SUBMIT", to: "IMPLEMENT_CHECK"},
		{from: "CONTINUE", outcome: "SUBMIT", to: "IMPLEMENT_CHECK"},
		{from: "IMPLEMENT_CHECK", outcome: "REPLIED", to: "TEST"},
		{from: "IMPLEMENT_CHECK", outcome: "INTERRUPTED", to: "CONTINUE"},
		{from: "TEST", outcome: "SUBMIT", to: "TEST_CHECK"},
		{from: "TEST_CHECK", outcome: "COMPLETE", to: "VALIDATE"},
		{from: "TEST_CHECK", outcome: "INCOMPLETE", to: "IMPLEMENT"},
		{from: "TEST_CHECK", outcome: "RETRY", to: "TEST"},
		{from: "VALIDATE", outcome: "PASS", to: "ACCEPTANCE"},
		{from: "VALIDATE", outcome: "FAIL", to: "IMPLEMENT"},
		{from: "VALIDATE", outcome: "REPLAN", to: "REPLAN"},
		{from: "REPLAN", outcome: "SUBMIT", to: "REPLAN_CHECK"},
		{from: "REPLAN_CHECK", outcome: "RETRY", to: "REPLAN"},
		{from: "REPLAN_CHECK", outcome: "REPLANNED", to: "IMPLEMENT"},
		{from: "PHASE_VALIDATE", outcome: "COMPLETE", to: "PHASE_DONE"},
		{from: "PHASE_VALIDATE", outcome: "FAIL", to: "PLAN"},
		{from: "PHASE_DONE", outcome: "EVALUATE", to: "MORE_PHASES"},
		{from: "MORE_PHASES", outcome: "YES", to: "PLAN"},
		{from: "MORE_PHASES", outcome: "NO", to: "FINAL_HARDENING"},
		{from: "FINAL_HARDENING", outcome: "FIX", to: "HARDENING_FIX"},
		{from: "HARDENING_FIX", outcome: "APPLY", to: "FINAL_HARDENING"},
		{from: "FINAL_HARDENING", outcome: "COMPLETE", to: "FINAL_VALIDATE"},
		{from: "FINAL_VALIDATE", outcome: "PASS", to: "FINAL_CHECKPOINT"},
		{from: "FINAL_VALIDATE", outcome: "FAIL", to: "FINAL_HARDENING"},
		{from: "FINAL_CHECKPOINT", outcome: "PROCEED", to: "PROJECT_DONE"},
	}
}

// composedStateSpecs is the composed-prompt workflow's state set (WBS 6.3
// diagram + P16-78 finalization).
func composedStateSpecs() []stateSpec {
	return []stateSpec{
		{key: "PROJECT_INIT", kind: state.KindControl},
		{key: "PHASE_PLAN", kind: state.KindAction, template: "PHASE_PLAN"},
		{key: "PLAN_VALIDATE", kind: state.KindValidation, template: "PLAN_VALIDATE"},
		{key: "WORK_UNIT_PREPARE", kind: state.KindControl},
		{key: "IMPLEMENT", kind: state.KindAction, template: "IMPLEMENT_WORK_UNIT"},
		{key: "RESPONSE_EVALUATE", kind: state.KindValidation, template: "RESPONSE_EVALUATE"},
		{key: "COMPLETE", kind: state.KindControl},
		{key: "INCOMPLETE", kind: state.KindControl},
		{key: "VALIDATE", kind: state.KindValidation, template: "VALIDATE_WORK_UNIT"},
		{key: "CONTINUE_DECISION", kind: state.KindDecision, template: "CONTINUE_DECISION"},
		{key: "SAME_SESSION", kind: state.KindControl},
		{key: "RESET_SESSION", kind: state.KindControl},
		{key: "CONTINUE", kind: state.KindAction, template: "CONTINUE_WORK_UNIT"},
		{key: "VALIDATION_RESULT", kind: state.KindControl},
		{key: "PASS", kind: state.KindControl},
		{key: "FAIL", kind: state.KindControl},
		{key: "UNIT_DONE", kind: state.KindControl},
		{key: "FIX", kind: state.KindAction, template: "FIX_WORK_UNIT"},
		{key: "MORE_WORK", kind: state.KindDecision, template: "MORE_WORK"},
		{key: "NEXT_UNIT", kind: state.KindControl},
		{key: "PHASE_VALIDATE", kind: state.KindValidation, template: "PHASE_VALIDATE"},
		{key: "PHASE_DONE", kind: state.KindControl},
		{key: "MORE_PHASES", kind: state.KindDecision, template: "MORE_PHASES"},
		{key: "FINAL_HARDENING", kind: state.KindControl},
		{key: "HARDENING_FIX", kind: state.KindAction, template: "HARDENING_FIX"},
		{key: "FINAL_VALIDATE", kind: state.KindValidation, template: "FINAL_VALIDATE"},
		{key: "FINAL_CHECKPOINT", kind: state.KindControl},
		{key: "PROJECT_DONE", kind: state.KindTerminal, terminal: true},
	}
}

// composedTransitionSpecs is the composed-prompt workflow's edge set.
func composedTransitionSpecs() []transitionSpec {
	return []transitionSpec{
		{from: "PROJECT_INIT", outcome: "PROCEED", to: "PHASE_PLAN"},
		{from: "PROJECT_INIT", outcome: "PLAN_REQUIRED", to: "PHASE_PLAN"},
		{from: "PROJECT_INIT", outcome: "PLAN_READY", to: "WORK_UNIT_PREPARE"},
		{from: "PROJECT_INIT", outcome: "WORK_COMPLETE", to: "PHASE_VALIDATE"},
		{from: "PHASE_PLAN", outcome: "SUBMIT", to: "PLAN_VALIDATE"},
		{from: "PLAN_VALIDATE", outcome: "ACCEPTED", to: "WORK_UNIT_PREPARE"},
		{from: "WORK_UNIT_PREPARE", outcome: "READY", to: "IMPLEMENT"},
		{from: "IMPLEMENT", outcome: "EVALUATE", to: "RESPONSE_EVALUATE"},
		{from: "RESPONSE_EVALUATE", outcome: "COMPLETE", to: "VALIDATE"},
		{from: "RESPONSE_EVALUATE", outcome: "INCOMPLETE", to: "CONTINUE_DECISION"},
		{from: "CONTINUE_DECISION", outcome: "SAME", to: "SAME_SESSION"},
		{from: "CONTINUE_DECISION", outcome: "RESET", to: "RESET_SESSION"},
		{from: "SAME_SESSION", outcome: "CONTINUE", to: "CONTINUE"},
		{from: "RESET_SESSION", outcome: "CONTINUE", to: "CONTINUE"},
		{from: "CONTINUE", outcome: "PROCEED", to: "IMPLEMENT"},
		{from: "VALIDATE", outcome: "EVALUATE", to: "VALIDATION_RESULT"},
		{from: "VALIDATION_RESULT", outcome: "PASS", to: "PASS"},
		{from: "VALIDATION_RESULT", outcome: "FAIL", to: "FAIL"},
		{from: "PASS", outcome: "PROCEED", to: "UNIT_DONE"},
		{from: "FAIL", outcome: "PROCEED", to: "FIX"},
		{from: "FIX", outcome: "APPLY", to: "IMPLEMENT"},
		{from: "UNIT_DONE", outcome: "EVALUATE", to: "MORE_WORK"},
		{from: "MORE_WORK", outcome: "YES", to: "NEXT_UNIT"},
		{from: "MORE_WORK", outcome: "NO", to: "PHASE_VALIDATE"},
		{from: "NEXT_UNIT", outcome: "PROCEED", to: "WORK_UNIT_PREPARE"},
		{from: "PHASE_VALIDATE", outcome: "COMPLETE", to: "PHASE_DONE"},
		{from: "PHASE_DONE", outcome: "EVALUATE", to: "MORE_PHASES"},
		{from: "MORE_PHASES", outcome: "YES", to: "PHASE_PLAN"},
		{from: "PLAN_VALIDATE", outcome: "REJECTED", to: "PHASE_PLAN"},
		{from: "RESPONSE_EVALUATE", outcome: "FAILED", to: "FIX"},
		{from: "PHASE_VALIDATE", outcome: "FAIL", to: "PHASE_PLAN"},
		{from: "MORE_PHASES", outcome: "NO", to: "FINAL_HARDENING"},
		{from: "FINAL_HARDENING", outcome: "FIX", to: "HARDENING_FIX"},
		{from: "HARDENING_FIX", outcome: "APPLY", to: "FINAL_HARDENING"},
		{from: "FINAL_HARDENING", outcome: "COMPLETE", to: "FINAL_VALIDATE"},
		{from: "FINAL_VALIDATE", outcome: "PASS", to: "FINAL_CHECKPOINT"},
		{from: "FINAL_VALIDATE", outcome: "FAIL", to: "FINAL_HARDENING"},
		{from: "FINAL_CHECKPOINT", outcome: "PROCEED", to: "PROJECT_DONE"},
	}
}

// SeedDefaultTemplate seeds the default development-workflow machine, its
// states, and its transitions as data (WBS 6.3, T6.3.1). It is idempotent:
// a second call finds the existing machine and returns it unchanged
// (T6.3.2). It returns the machine ID.
func (s *StateTemplateService) SeedDefaultTemplate(ctx context.Context) (string, error) {
	return s.seed(ctx, statefileTemplate)
}

// SeedComposedTemplate seeds the composed-prompt workflow template.
func (s *StateTemplateService) SeedComposedTemplate(ctx context.Context) (string, error) {
	return s.seed(ctx, composedTemplate)
}

func (s *StateTemplateService) seed(ctx context.Context, def templateDef) (string, error) {
	if s.txPool == nil {
		return s.seedOnce(ctx, nil, def)
	}
	var id string
	err := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		if _, e := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", stateTemplateLockKey); e != nil {
			return translate(e)
		}
		var e error
		id, e = s.seedOnce(ctx, tx, def)
		return e
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// CopyToProject materializes the default template into the given project
// (WBS 6.3, T6.3.6) as a new machine with fresh IDs and the same topology.
// If the template is not yet seeded it is seeded first. It is idempotent: a
// second copy into the same project returns the existing copy.
func (s *StateTemplateService) CopyToProject(ctx context.Context, targetProjectID string) (string, error) {
	return s.copyTo(ctx, targetProjectID, statefileTemplate)
}

// CopyComposedToProject materializes the composed-prompt workflow into the
// project (seeding it first).
func (s *StateTemplateService) CopyComposedToProject(ctx context.Context, targetProjectID string) (string, error) {
	return s.copyTo(ctx, targetProjectID, composedTemplate)
}

func (s *StateTemplateService) copyTo(ctx context.Context, targetProjectID string, def templateDef) (string, error) {
	tid, err := uuid.Parse(targetProjectID)
	if err != nil {
		return "", ErrInvalidInput
	}
	if _, err := s.seed(ctx, def); err != nil {
		return "", err
	}
	if s.txPool == nil {
		return s.copyOnce(ctx, nil, tid, def)
	}
	var out string
	err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var e error
		out, e = s.copyOnce(ctx, tx, tid, def)
		return e
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// bind returns repository copies bound to tx (or the base pool-bound repos
// when tx is nil), so the same code runs inside and outside a transaction.
func (s *StateTemplateService) bind(tx persistence.Transaction) (
	persistence.StateMachineRepository,
	persistence.WorkflowStateRepository,
	persistence.StateTransitionRepository,
	persistence.ProjectRepository,
) {
	if tx == nil {
		return s.machines, s.states, s.transitions, s.projects
	}
	return s.machines.WithTx(tx), s.states.WithTx(tx), s.transitions.WithTx(tx), s.projects.WithTx(tx)
}

// seedOnce performs one idempotent seed attempt against the given
// transaction (nil for the sequential path).
func (s *StateTemplateService) seedOnce(ctx context.Context, tx persistence.Transaction, def templateDef) (string, error) {
	machines, states, transitions, projects := s.bind(tx)
	proj, err := s.findOrCreateSystemProject(ctx, projects)
	if err != nil {
		return "", err
	}
	if _, id, found := findMachineByName(ctx, machines, proj.ID, def.name); found {
		return id, nil
	}
	return buildMachine(ctx, machines, states, transitions, proj.ID, def)
}

// copyOnce performs one copy attempt into targetID against the given
// transaction (nil for the sequential path).
func (s *StateTemplateService) copyOnce(ctx context.Context, tx persistence.Transaction, targetID uuid.UUID, def templateDef) (string, error) {
	machines, states, transitions, projects := s.bind(tx)
	srcProj, err := s.findSystemProject(ctx, projects)
	if err != nil {
		return "", err
	}
	src, _, ok := findMachineByName(ctx, machines, srcProj.ID, def.name)
	if !ok {
		return "", ErrNotFound
	}
	if _, id, found := findMachineByName(ctx, machines, targetID, def.name); found {
		return id, nil
	}
	srcStates, err := states.ListByMachine(ctx, src.ID, src.Version)
	if err != nil {
		return "", translate(err)
	}
	srcTransitions, err := transitions.ListByMachine(ctx, src.ID, src.Version)
	if err != nil {
		return "", translate(err)
	}
	now := time.Now().UTC()
	newMachineID := uuid.New()
	remap := make(map[uuid.UUID]uuid.UUID, len(srcStates))
	var initialID uuid.UUID
	for _, ss := range srcStates {
		nid := uuid.New()
		remap[ss.ID] = nid
		if ss.Key == "PROJECT_INIT" {
			initialID = nid
		}
	}
	mrec := &persistence.MachineRecord{
		ID: newMachineID, ProjectID: targetID, Name: def.name,
		Description: src.Description, Version: 1, Enabled: true,
		InitialStateID: nil, CreatedAt: now, UpdatedAt: now,
	}
	if err := machines.Create(ctx, mrec); err != nil {
		return "", translate(err)
	}
	for _, ss := range srcStates {
		srec := &persistence.WorkflowStateRecord{
			ID: remap[ss.ID], MachineID: newMachineID, Version: 1, Key: ss.Key, Name: ss.Name,
			Description: ss.Description, PromptTemplate: ss.PromptTemplate,
			Kind: ss.Kind, Terminal: ss.Terminal, Enabled: true,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := states.Create(ctx, srec); err != nil {
			return "", translate(err)
		}
	}
	if err := machines.SetInitialState(ctx, newMachineID, initialID); err != nil {
		return "", translate(err)
	}
	for _, tt := range srcTransitions {
		trec := &persistence.StateTransitionRecord{
			ID: uuid.New(), MachineID: newMachineID, Version: 1,
			FromStateID: remap[tt.FromStateID], ToStateID: remap[tt.ToStateID],
			Outcome: tt.Outcome, Priority: tt.Priority, Enabled: true,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := transitions.Create(ctx, trec); err != nil {
			return "", translate(err)
		}
	}
	return newMachineID.String(), nil
}

// findSystemProject returns the canonical system project, or ErrNotFound.
func (s *StateTemplateService) findSystemProject(ctx context.Context, projs persistence.ProjectRepository) (*project.Project, error) {
	list, err := projs.List(ctx, persistence.ProjectListFilter{})
	if err != nil {
		return nil, translate(err)
	}
	for _, p := range list {
		if p.Name == defaultSystemProjectName {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

// findOrCreateSystemProject returns the canonical system project, creating it
// when absent.
func (s *StateTemplateService) findOrCreateSystemProject(ctx context.Context, projs persistence.ProjectRepository) (*project.Project, error) {
	if p, err := s.findSystemProject(ctx, projs); err == nil {
		return p, nil
	}
	p, err := project.New(defaultSystemProjectName, "System project hosting the default state-machine template")
	if err != nil {
		return nil, err
	}
	if err := projs.Create(ctx, p); err != nil {
		return nil, translate(err)
	}
	return p, nil
}

// findMachineByName lists a project's machines and returns the one whose
// name matches (idempotency key for the template).
func findMachineByName(ctx context.Context, m persistence.StateMachineRepository, projectID uuid.UUID, name string) (*persistence.MachineRecord, string, bool) {
	list, err := m.ListByProject(ctx, projectID)
	if err != nil {
		return nil, "", false
	}
	for _, rec := range list {
		if rec.Name == name {
			return rec, rec.ID.String(), true
		}
	}
	return nil, "", false
}

// buildMachine inserts a fresh default machine, its states, and its
// transitions, keyed to the stable state set + transition set (WBS 6.3).
func buildMachine(ctx context.Context,
	machines persistence.StateMachineRepository,
	states persistence.WorkflowStateRepository,
	transitions persistence.StateTransitionRepository,
	projectID uuid.UUID,
	def templateDef,
) (string, error) {
	specs := def.states()
	tSpecs := def.transitions()
	now := time.Now().UTC()
	machineID := uuid.New()
	ids := make(map[string]uuid.UUID, len(specs))
	var initialID uuid.UUID
	for _, sp := range specs {
		sid := uuid.New()
		ids[sp.key] = sid
		if sp.key == "PROJECT_INIT" {
			initialID = sid
		}
	}
	mrec := &persistence.MachineRecord{
		ID: machineID, ProjectID: projectID, Name: def.name,
		Description: def.description,
		Version:     1, Enabled: true,
		InitialStateID: nil, CreatedAt: now, UpdatedAt: now,
	}
	if err := machines.Create(ctx, mrec); err != nil {
		return "", translate(err)
	}
	for _, sp := range specs {
		srec := &persistence.WorkflowStateRecord{
			ID: ids[sp.key], MachineID: machineID, Version: 1, Key: sp.key, Name: sp.key,
			Description: "", PromptTemplate: sp.template,
			Kind: string(sp.kind), Terminal: sp.terminal, Enabled: true,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := states.Create(ctx, srec); err != nil {
			return "", translate(err)
		}
	}
	if err := machines.SetInitialState(ctx, machineID, initialID); err != nil {
		return "", translate(err)
	}
	for _, ts := range tSpecs {
		trec := &persistence.StateTransitionRecord{
			ID: uuid.New(), MachineID: machineID, Version: 1,
			FromStateID: ids[ts.from], ToStateID: ids[ts.to],
			Outcome: ts.outcome, Priority: 0, Enabled: true,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := transitions.Create(ctx, trec); err != nil {
			return "", translate(err)
		}
	}
	return machineID.String(), nil
}
