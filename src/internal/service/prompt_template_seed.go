// PromptTemplateService seeds the default prompt templates (WBS 8.2: the 9
// canonical keys, plus the two WBS 6.7 state-reference aliases) as
// editable data at GLOBAL scope (project_id NULL), idempotently, and exposes
// the global-scope reads a later composition task consumes. It is a
// service-layer op only — WBS 8.2 declares no REST endpoint for it. The
// templates are rows, not Go constants controlling behavior: each seeded
// template is independently editable through the P8-10 Update surface, and
// seeding is the only writer of the initial v1 content version.
package service

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
)

// promptTemplateSeedLockKey serializes the seed op (mirrors the P6-11
// state-template advisory-lock recipe).
const promptTemplateSeedLockKey = "prompt-template:defaults"

// DefaultTemplateKeys is the WBS 8.2 canonical default template key set —
// the 9 keys seeded at global scope by SeedDefaults.
var DefaultTemplateKeys = []string{
	"PLAN_PHASE",
	"VALIDATE_PLAN",
	"PREPARE_WORK_UNIT",
	"IMPLEMENT_WORK_UNIT",
	"CONTINUE_WORK_UNIT",
	"VALIDATE_WORK_UNIT",
	"FIX_WORK_UNIT",
	"VALIDATE_PHASE",
	"REVIEW_RESULT",
}

// StateTemplateRef pairs one default-machine state key with the
// prompt-template key it references. This is the state -> key mapping for
// T8.2.3: the required-state set from the WBS 8.2 task notes (the seven
// states above). The seeded global set (DefaultTemplateKeys) is a superset
// of it: PREPARE_WORK_UNIT and REVIEW_RESULT are WBS 8.2 defaults without a
// current default-state reference. The default machine's decision/evaluation
// states (RESPONSE_EVALUATE, CONTINUE_DECISION, MORE_WORK, MORE_PHASES)
// reference further keys outside the WBS 8.2 default set.
type StateTemplateRef struct {
	StateKey    string
	TemplateKey string
}

// DefaultStateTemplateRefs is the T8.2.3 required-state reference table:
// the default-machine states that reference a WBS 8.2-scope default key
// (the seven-state set from the task notes).
var DefaultStateTemplateRefs = []StateTemplateRef{
	{StateKey: "PHASE_PLAN", TemplateKey: "PLAN_PHASE"},
	{StateKey: "PLAN_VALIDATE", TemplateKey: "PLAN_VALIDATE"},
	{StateKey: "IMPLEMENT", TemplateKey: "IMPLEMENT_WORK_UNIT"},
	{StateKey: "VALIDATE", TemplateKey: "VALIDATE_WORK_UNIT"},
	{StateKey: "CONTINUE", TemplateKey: "CONTINUE_WORK_UNIT"},
	{StateKey: "FIX", TemplateKey: "FIX_WORK_UNIT"},
	{StateKey: "PHASE_VALIDATE", TemplateKey: "PHASE_VALIDATE"},
}

// defaultTemplateSpecs pairs each WBS 8.2 default key with a purpose from
// the 7 known values and a stable non-empty initial content (T8.2.1). The
// content is the seeded v1 body; it is editable data, not behavior.
type defaultTemplateSpec struct {
	key         string
	purpose     prompt.PromptPurpose
	name        string
	description string
	content     string
	// supersedes lists earlier default contents: a template still holding
	// one of them (never edited) gets content as its next version.
	supersedes []string
}

func defaultTemplateSpecs() []defaultTemplateSpec {
	return []defaultTemplateSpec{
		{key: "PLAN_PHASE", purpose: prompt.PurposePhasePlanning, name: "Plan phase",
			description: "Default prompt for planning a phase's work (WBS 8.2).",
			content:     "Plan the work for the current phase. Decompose it into work units with objectives, and state the plan structure."},
		{key: "VALIDATE_PLAN", purpose: prompt.PurposeValidation, name: "Validate plan",
			description: "Default prompt for validating a phase plan (WBS 8.2).",
			content:     "Validate the phase plan. Check completeness and coherence, and report acceptance or the required changes."},
		{key: "PREPARE_WORK_UNIT", purpose: prompt.PurposeImplementation, name: "Prepare work unit",
			description: "Default prompt for preparing a work unit for execution (WBS 8.2).",
			content:     "Prepare the work unit for execution. Gather the context needed and state the execution approach."},
		{key: "IMPLEMENT_WORK_UNIT", purpose: prompt.PurposeImplementation, name: "Implement work unit",
			description: "Default prompt for implementing a work unit (WBS 8.2).",
			content:     "Implement the assigned work unit. Follow its objective and report the result for evaluation."},
		{key: "CONTINUE_WORK_UNIT", purpose: prompt.PurposeContinuation, name: "Continue work unit",
			description: "Default prompt for continuing an in-progress work unit (WBS 8.2).",
			content:     "Continue the in-progress work unit from its current position. Resume where the previous session stopped."},
		{key: "VALIDATE_WORK_UNIT", purpose: prompt.PurposeValidation, name: "Validate work unit",
			description: "Default prompt for validating a completed work unit (WBS 8.2).",
			content:     "Validate the work unit's result against its objective. Report pass or fail with findings."},
		{key: "FIX_WORK_UNIT", purpose: prompt.PurposeImplementation, name: "Fix work unit",
			description: "Default prompt for fixing a failed work unit (WBS 8.2).",
			content:     "Fix the work unit based on the validation findings, then report the corrected result. The failing commands, exit codes and output are recorded in TEST_STATE; the work unit definition in ACTIVE_TASK stays authoritative."},
		{key: "VALIDATE_PHASE", purpose: prompt.PurposeValidation, name: "Validate phase",
			description: "Default prompt for validating a completed phase (WBS 8.2).",
			content:     "Validate the completed phase. Check that all work units are done and report the phase result."},
		{key: "REVIEW_RESULT", purpose: prompt.PurposeReview, name: "Review result",
			description: "Default prompt for reviewing a result (WBS 8.2).",
			content:     "Review the presented result. Summarize findings and recommend the next step."},
		// State-reference aliases: the default machine's PLAN_VALIDATE and
		// PHASE_VALIDATE states reference these keys (WBS 6.7), outside the
		// 9-key WBS 8.2 canonical set. Seeded too so every state template
		// reference resolves (T8.2.3).
		{key: "PLAN_VALIDATE", purpose: prompt.PurposeValidation, name: "Validate plan (state reference)",
			description: "Default prompt referenced by the PLAN_VALIDATE state (WBS 6.7 alias of VALIDATE_PLAN).",
			content:     "Validate the phase plan. Check completeness and coherence, and report acceptance or the required changes."},
		{key: "PHASE_VALIDATE", purpose: prompt.PurposeValidation, name: "Validate phase (state reference)",
			description: "Default prompt referenced by the PHASE_VALIDATE state (WBS 6.7 alias of VALIDATE_PHASE).",
			content:     "Validate the completed phase. Check that all work units are done and report the phase result."},
		// P15-22 (WBS 15.34): state-reference seeds for the remaining
		// default-machine template keys, so every reachable state of the
		// default workflow resolves during bootstrap readiness.
		// Statefile workflow commands: each CMD_ template is sent to the
		// worker as is ({{command.phase}} = the phase number,
		// {{command.task}} = the active task's key); the team's own role
		// instructions say how to act on it.
		{key: "CMD_SKILL", purpose: prompt.PurposeCustom, name: "Command: create output-format skill",
			description: "Sent by the SKILL state at the start of every execution: Hermes saves the project's required response formatting as the <project>-output-format skill (the skill content is appended by the overseer).",
			content:     "create the {{command.skill}} skill"},
		{key: "CMD_PLAN", purpose: prompt.PurposeCustom, name: "Command: plan phase",
			description: "Sent by the PLAN state: the ARCHITECT plans the phase WBS into statefiles/PLAN.md, the PLANNER breaks it into statefiles/BACKLOG.md.",
			content:     "plan phase {{command.phase}}"},
		{key: "CMD_IMPLEMENT", purpose: prompt.PurposeCustom, name: "Command: implement task",
			description: "Sent by the IMPLEMENT state: the ENGINEER implements the task in statefiles/ACTIVE_TASK.md.",
			content:     "implement task {{command.task}}"},
		{key: "CMD_CONTINUE", purpose: prompt.PurposeCustom, name: "Command: continue",
			description: "Sent by the CONTINUE state: the ENGINEER continues the active task after an interrupted run.",
			content:     "continue"},
		{key: "CMD_TEST", purpose: prompt.PurposeCustom, name: "Command: test task",
			description: "Sent by the TEST state: the TESTER tests the active task, checks met acceptance criteria and sets COMPLETE when all are met.",
			content:     "test task {{command.task}}"},
		{key: "CMD_ACCEPTANCE", purpose: prompt.PurposeCustom, name: "Command: acceptance",
			description: "Sent by the ACCEPTANCE state: the PROJECT_MANAGER marks the completed task DONE, archives it to statefiles/TASK_ARCHIVE.md and loads the next task into statefiles/ACTIVE_TASK.md.",
			content:     "have the project manager perform acceptance tasks"},
		{key: "PHASE_PLAN", purpose: prompt.PurposePhasePlanning, name: "Plan phase (state reference)",
			description: "Default prompt referenced by the PHASE_PLAN state (WBS 6.7 alias of PLAN_PHASE).",
			content: "Plan the current phase. Decompose its WBS into an ordered set of atomic work units with acceptance criteria.\n\n" +
				"Planning policy (Atomic): every unit is independently testable (declare validation commands or required tests), " +
				"declares its dependencies explicitly (composition, integration and validation units depend on the units they build on), " +
				"declares acceptance criteria with a verification type (COMMAND, TEST or MANUAL), keeps composition and integration " +
				"separate from primitives, and targets one worker invocation (single_invocation: true).\n\n" +
				"Return the plan in the response's `plan` object: {\"units\": [{\"key\", \"name\", \"kind\" (ATOMIC | COMPOSITION | " +
				"INTEGRATION | VALIDATION), \"objective\", \"instructions\", \"acceptance_checks\": [{\"text\", \"verification\", " +
				"\"command\": {\"program\", \"args\"}}], \"required_tests\", \"validation\": [{\"program\", \"args\"}], \"depends_on\", " +
				"\"paths\", \"single_invocation\"}]}. The overseer validates the plan deterministically before activating it.",
			// Interim defaults (task-file planning) are restored to the
			// composed workflow's JSON plan contract.
			supersedes: []string{"Break the phase WBS written by the ARCHITECT into an ordered set of atomic, independently testable tasks with " +
				"acceptance criteria, and write them as a markdown task file at the path given below, in the required task file format.\n\n" +
				"Planning policy (Atomic): every task is independently testable (declare validation commands or required tests), " +
				"declares its dependencies explicitly (composition, integration and validation tasks depend on the tasks they build on), " +
				"declares acceptance criteria with a verification type ([COMMAND], [TEST] or [MANUAL]), keeps composition and integration " +
				"separate from primitives, and targets one worker invocation (Single invocation: yes). Do not implement anything. " +
				"The overseer parses and validates the task file deterministically before activating it.",
				"Break the phase WBS into an ordered set of atomic, independently testable tasks with acceptance criteria, " +
					"and write them to statefiles/BACKLOG.md in the required task file format.\n\n" +
					"Planning policy (Atomic): every task is independently testable (declare validation commands or required tests), " +
					"declares its dependencies explicitly (composition, integration and validation tasks depend on the tasks they build on), " +
					"declares acceptance criteria with a verification type ([COMMAND], [TEST] or [MANUAL]), keeps composition and integration " +
					"separate from primitives, and targets one worker invocation (Single invocation: yes). Do not implement anything. " +
					"The overseer parses and validates the backlog deterministically before activating it."}},
		{key: "RESPONSE_EVALUATE", purpose: prompt.PurposeValidation, name: "Evaluate response (state reference)",
			description: "Default prompt referenced by the RESPONSE_EVALUATE state.",
			content:     "Evaluate the worker response against the response protocol and report whether the work unit is complete."},
		{key: "CONTINUE_DECISION", purpose: prompt.PurposeControl, name: "Continuation decision (state reference)",
			description: "Default prompt referenced by the CONTINUE_DECISION state.",
			content:     "Decide whether the incomplete work unit continues in the same session or a fresh session."},
		{key: "MORE_WORK", purpose: prompt.PurposeControl, name: "More work decision (state reference)",
			description: "Default prompt referenced by the MORE_WORK state.",
			content:     "Decide whether the current phase has further executable work units."},
		{key: "MORE_PHASES", purpose: prompt.PurposeControl, name: "More phases decision (state reference)",
			description: "Default prompt referenced by the MORE_PHASES state.",
			content:     "Decide whether the project has further phases to execute."},
		// P16-79 (WBS 16.49, 16.131, 16.151): final-hardening fix and final
		// validation state references.
		{key: "HARDENING_FIX", purpose: prompt.PurposeImplementation, name: "Fix hardening consolidation (state reference)",
			description: "Default prompt referenced by the HARDENING_FIX state.",
			content: "A deterministic validation failed during final hardening or final project validation. Fix the repository so the " +
				"failing commands pass, using the failure evidence in TEST_STATE (command, exit code, output). Preserve behaviour; do not " +
				"remove tests to make them pass."},
		{key: "FINAL_VALIDATE", purpose: prompt.PurposeValidation, name: "Final project validation (state reference)",
			description: "Default prompt referenced by the FINAL_VALIDATE state (validation runs under overseer control).",
			content:     "Final project validation runs the configured deterministic checks against the hardened repository."},
	}

}

// PromptTemplateService coordinates the prompt-template and
// prompt-template-version repositories to seed the WBS 8.2 defaults.
type PromptTemplateService struct {
	templates persistence.PromptTemplateRepository
	versions  persistence.PromptTemplateVersionRepository
	txPool    *pgxpool.Pool
}

// NewPromptTemplateService wires the service to the repositories.
func NewPromptTemplateService(
	templates persistence.PromptTemplateRepository,
	versions persistence.PromptTemplateVersionRepository,
) *PromptTemplateService {
	return &PromptTemplateService{templates: templates, versions: versions}
}

// WithTxPool wires the transaction pool used for the atomic seed path
// (WBS 8.2 idempotency). When nil, operations run sequentially (single-
// goroutine test scope).
func (s *PromptTemplateService) WithTxPool(pool *pgxpool.Pool) *PromptTemplateService {
	s.txPool = pool
	return s
}

// SeedDefaults seeds the 9 WBS 8.2 default templates at global scope (T8.2.1).
// It is idempotent: a second call finds the existing global templates by
// scoped key and leaves them and their version rows byte-unchanged (T8.2.2).
// The op is serialized by an advisory lock over a stable key.
func (s *PromptTemplateService) SeedDefaults(ctx context.Context) error {
	if s.txPool == nil {
		return s.seedOnce(ctx, nil)
	}
	return dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, promptTemplateSeedLockKey); err != nil {
			return translate(err)
		}
		return s.seedOnce(ctx, tx)
	})
}

// seedOnce performs one idempotent seed attempt against the given
// transaction (nil for the sequential path).
func (s *PromptTemplateService) seedOnce(ctx context.Context, tx persistence.Transaction) error {
	var templates persistence.PromptTemplateRepository
	var versions persistence.PromptTemplateVersionRepository
	if tx == nil {
		templates, versions = s.templates, s.versions
	} else {
		templates, versions = s.templates.WithTx(tx), s.versions.WithTx(tx)
	}
	for _, spec := range defaultTemplateSpecs() {
		if rec, err := templates.GetByKey(ctx, nil, spec.key); err == nil {
			// Already seeded in this scope: leave the row and its version
			// rows byte-unchanged (T8.2.2 idempotency short-circuit) unless
			// it still holds a superseded default.
			if len(spec.supersedes) == 0 {
				continue
			}
			cur, err := versions.GetCurrentVersion(ctx, rec.ID)
			if err != nil || cur == nil || !slices.Contains(spec.supersedes, cur.Content) {
				continue
			}
			if _, err := versions.AppendVersion(ctx, rec.ID, spec.content); err != nil {
				return translate(err)
			}
			continue
		}
		now := time.Now().UTC()
		rec := &persistence.PromptTemplateRecord{
			ID: uuid.New(), ProjectID: nil, Key: prompt.NormalizeKey(spec.key),
			Name: spec.name, Description: spec.description,
			Purpose: string(spec.purpose), Enabled: true, Version: 1,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := templates.Create(ctx, rec); err != nil {
			return translate(err)
		}
		if _, err := versions.AppendVersion(ctx, rec.ID, spec.content); err != nil {
			return translate(err)
		}
	}
	return nil
}

// ListGlobal returns the seeded global-scope templates in key order.
func (s *PromptTemplateService) ListGlobal(ctx context.Context) ([]*persistence.PromptTemplateRecord, error) {
	list, err := s.templates.ListGlobal(ctx)
	if err != nil {
		return nil, translate(err)
	}
	return list, nil
}

// GetByKeyGlobal resolves a key at global scope (T8.2.3 lookup surface); an
// unknown key yields ErrNotFound.
func (s *PromptTemplateService) GetByKeyGlobal(ctx context.Context, key string) (*persistence.PromptTemplateRecord, error) {
	rec, err := s.templates.GetByKey(ctx, nil, key)
	if err != nil {
		return nil, translate(err)
	}
	return rec, nil
}

// CurrentVersion returns the template's current content version (nil, nil
// when the template carries no versions).
func (s *PromptTemplateService) CurrentVersion(ctx context.Context, templateID uuid.UUID) (*persistence.PromptTemplateVersionRecord, error) {
	v, err := s.versions.GetCurrentVersion(ctx, templateID)
	if err != nil {
		return nil, translate(err)
	}
	return v, nil
}
