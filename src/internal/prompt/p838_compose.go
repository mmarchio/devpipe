package prompt

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/role"
	"overseer/internal/state"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// P8-38 — WBS 8.25 composition request + core prompt composer.
//
// The composer resolves a pinned context snapshot into canonical components
// (via the P8-15..P8-22 primitives), orders them canonically, deduplicates,
// renders the pinned template, and produces the persisted CanonicalPrompt
// (WBS 8.26) with exact content + deterministic hash + token estimate +
// provenance (P8-31) + cache key (P8-34). It consumes ONLY snapshot-captured
// / pinned values — never "latest" — so repeated composition over identical
// pinned inputs is byte-identical (WBS 8.32, T8.32.1/T8.32.2; the DB-row-order
// half is P8-39).

// ComposePromptRequest is the explicit composition request object (WBS 8.25).
// It replaces loosely related arguments: the caller pins the context snapshot,
// the template identity+version, and an optional continuation status (whose
// work-unit association is validated against the snapshot's bound unit, T8.25.4).
type ComposePromptRequest struct {
	ContextSnapshotID uuid.UUID
	TemplateID        uuid.UUID
	TemplateVersion   int

	// ContinuationStatusID identifies a captured worker status; when present
	// it is validated against ContinuationWorkUnitID (the unit the status is
	// applied to, T8.25.4). A nil status (no continuation) is accepted.
	ContinuationStatusID   *uuid.UUID
	ContinuationWorkUnitID *uuid.UUID

	// CapturedStatus optionally carries the captured status values
	// (summary/completed/remaining/blocking/next action) for the
	// ContinuationStatusID (Phase 9 P9-62: a continuation prompt preserves
	// the captured status). Its WorkUnitID must equal
	// ContinuationWorkUnitID; nil keeps the identity-only Phase 8 behaviour.
	CapturedStatus *ContinuationStatusInput
}

// ErrMissingSnapshot reports a request with no context snapshot (WBS 8.25,
// T8.25.2) — a composition always pins a specific snapshot.
var ErrMissingSnapshot = errors.New("prompt: composition request carries no context snapshot")

// ErrInvalidTemplate reports a request with no usable pinned template (WBS
// 8.25, T8.25.3): a missing template id or a non-positive template version.
var ErrInvalidTemplate = errors.New("prompt: composition request carries no valid pinned template")

// IsMissingSnapshot reports whether err is ErrMissingSnapshot.
func IsMissingSnapshot(err error) bool { return errors.Is(err, ErrMissingSnapshot) }

// IsInvalidTemplate reports whether err is ErrInvalidTemplate.
func IsInvalidTemplate(err error) bool { return errors.Is(err, ErrInvalidTemplate) }

// NewComposePromptRequest validates a composition request (WBS 8.25): the
// context snapshot id is required (T8.25.2) and the template id+version must
// be present and positive (T8.25.3). The request is a byte-stable
// pass-through of the pinned values.
func NewComposePromptRequest(req ComposePromptRequest) (ComposePromptRequest, error) {
	if req.ContextSnapshotID == uuid.Nil {
		return ComposePromptRequest{}, ErrMissingSnapshot
	}
	if req.TemplateID == uuid.Nil || req.TemplateVersion <= 0 {
		return ComposePromptRequest{}, ErrInvalidTemplate
	}
	return req, nil
}

// ValidateContinuationForUnit validates a captured continuation status against
// the work unit it would be applied to (WBS 8.25, T8.25.4): a nil status is
// accepted (no continuation); a non-nil status must be associated with the
// given unit (the P8-08 ValidateAssociation contract — cross-unit is rejected).
func ValidateContinuationForUnit(status *ContinuationStatus, workUnitID uuid.UUID) error {
	if status == nil {
		return nil
	}
	return status.ValidateAssociation(workUnitID)
}

// CanonicalCompositionInput carries every CAPTURED/pinned value the core
// composer consumes (WBS 8.25/8.32). Nothing here reads "latest": the role
// snapshot, state files, work unit, and continuation are the snapshot's pinned
// captures, and the template/protocol are the pinned content at the pinned
// version. Repeated composition over the same input is byte-identical.
type CanonicalCompositionInput struct {
	// Associations / provenance (from the pinned snapshot).
	ProjectID uuid.UUID
	PhaseID   *uuid.UUID
	UnitID    *uuid.UUID

	ContextSnapshotID uuid.UUID

	TemplateID      uuid.UUID
	TemplateVersion int

	RoleID      uuid.UUID
	RoleVersion int

	StateMachineID      uuid.UUID
	StateMachineVersion int
	StateKey            string

	// Captured role context (P8-16 system prompt, P8-17 role instructions).
	Role role.RoleSnapshot

	// Captured state files (P8-19) + the role's required type keys.
	StateFiles []statefile.StateFileSnapshot
	Required   []string

	// Captured current work unit (P8-14); nil = no unit in context.
	Unit *WorkUnitContext

	// Captured continuation status (P8-08); nil = no continuation.
	Continuation *ContinuationStatus

	// Captured response protocol (P8-22/P8-23): identity/version + content
	// at the pinned version + allowed statuses.
	ProtocolID      uuid.UUID
	ProtocolVersion int
	ProtocolContent string
	AllowedStatuses []string

	// Captured state instruction (P8-12): the state's current instruction
	// content + its version + hash (resolved by the service; empty when the
	// state carries no instruction).
	StateInstructionContent string
	StateInstructionVersion int
	StateInstructionHash    string

	// Pinned template SOURCE (rendered over the composed components).
	TemplateSource string

	// Snapshot content hash (stands in for the pinned state files, WBS 8.27/8.34).
	SnapshotHash string

	// Planning composition (WBS 8.16, consumed only when StateKey is
	// PHASE_PLAN — the P8-43 canonical planning prompt): the phase's current
	// WBS version + the EXACT document content, and the project's configured
	// planning policy + granularity (project configuration, WBS 8.16).
	PhaseWBSVersion     int
	PhaseWBSContent     string
	PlanningPolicy      string
	PlanningGranularity string
}

// componentVariable maps each accepted component type to the deterministic
// template variable it renders under (the WBS 8.5 namespace.field set): one
// variable per type, so the render is a pure function of the component set.
var componentVariable = map[PromptComponentType]string{
	ComponentSystem:              "role.system_prompt",
	ComponentRoleInstruction:     "role.instructions",
	ComponentStateInstruction:    "state.instruction",
	ComponentProjectContext:      "project.context",
	ComponentPhaseContext:        "phase.context",
	ComponentTaskContext:         "work_unit.task",
	ComponentTestContext:         "runtime.tests",
	ComponentContinuationContext: "continuation.status",
	ComponentResponseProtocol:    "response_protocol.schema",
}

// composeCanonicalComponents assembles the prompt components from the captured
// input using the per-state primitives (P8-15..P8-22), in canonical order,
// with byte-identical components deduplicated. It is a pure function of the
// captured values (no "latest" input).
func composeCanonicalComponents(in CanonicalCompositionInput) (Components, error) {
	var out Components

	// P8-16: the exact captured system prompt (required — a cleared version
	// is a composition failure, the P8-10 contract).
	sys, err := ResolveSystemPrompt(in.Role)
	if err != nil {
		return nil, err
	}
	out = append(out, sys)

	// P8-17: the role's assigned instructions, in captured assignment order.
	ins, err := ComposeRoleInstructions(in.Role)
	if err != nil {
		return nil, err
	}
	out = append(out, ins...)

	// P8-19: the pinned state files (deterministic (section,type,id) order).
	files, err := ComposeStateFiles(in.StateFiles, in.Required)
	if err != nil {
		return nil, err
	}
	out = append(out, files...)

	// P8-14: the captured current work unit (optional — no unit, no component).
	if in.Unit != nil {
		unitComps, err := ComposeWorkUnit(*in.Unit)
		if err != nil {
			return nil, err
		}
		out = append(out, unitComps...)
	}

	// P8-08: the captured continuation status (optional — no continuation, no
	// component; the work-unit association is validated by the caller).
	if in.Continuation != nil {
		cc, err := newContinuationComponent(*in.Continuation)
		if err != nil {
			return nil, err
		}
		out = append(out, cc)
	}

	// P8-12: the state's instruction (resolved by the caller at the state's
	// current version — captured, never "latest"). A state without an
	// instruction composes to zero STATE_INSTRUCTION components.
	if in.StateInstructionContent != "" {
		stateSnap := state.StateMachineSnapshot{
			MachineID: in.StateMachineID, Version: in.StateMachineVersion,
			States: []state.StateSnapshot{{
				Key:                in.StateKey,
				InstructionVersion: in.StateInstructionVersion,
				InstructionHash:    in.StateInstructionHash,
			}},
		}
		stComps, err := ComposeStateInstruction(stateSnap, in.StateKey, in.StateInstructionContent)
		if err != nil {
			return nil, err
		}
		out = append(out, stComps...)
	}

	// P8-22: the captured response protocol (required — a cleared version/content
	// is a composition failure, the P8-10/P8-22 contract).
	proto, err := ComposeResponseProtocol(in.ProtocolID, in.ProtocolVersion, in.ProtocolContent)
	if err != nil {
		return nil, err
	}
	out = append(out, proto...)

	// P8-24 (WBS 8.16): at the PHASE_PLAN state the planning configuration
	// (the exact current WBS, the configured planning policy, and the project
	// granularity) and the planning response schema are composed (the
	// execution-plan response requirements, T8.44).
	if in.StateKey == "PHASE_PLAN" {
		// The EXACT current WBS document content (WBS 8.44 "exact WBS v3") is
		// carried as a PHASE_CONTEXT component (the phase context the planner
		// plans over).
		if in.PhaseWBSContent != "" {
			wbsComp, err := NewPromptComponent(
				ComponentPhaseContext,
				SourceStateFile,
				0,
				in.PhaseWBSContent,
				uuid.Nil,
				in.PhaseWBSVersion,
				"",
			)
			if err != nil {
				return nil, err
			}
			// The phase WBS is the planning task itself: its requirement
			// identifiers must survive compression verbatim (Phase 9, WBS
			// 9.49) — the policy is set where the component is composed.
			if err := wbsComp.SetPolicy(PolicyPreserveExact); err != nil {
				return nil, err
			}
			out = append(out, wbsComp)
		}
		planComps, err := ComposePlanningPrompt(PlanningPromptInput{
			WBSVersion:      in.PhaseWBSVersion,
			PlanningPolicy:  in.PlanningPolicy,
			Granularity:     in.PlanningGranularity,
			WorkUnitKinds:   planningWorkUnitKinds(),
			ProtocolID:      in.ProtocolID,
			ProtocolVersion: in.ProtocolVersion,
		})
		if err != nil {
			return nil, err
		}
		// The planning configuration (WBS version, planning policy /
		// constraints, granularity) is preserved exactly (WBS 9.49).
		for i := range planComps {
			if planComps[i].Type == ComponentPhaseContext {
				if err := planComps[i].SetPolicy(PolicyPreserveExact); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, planComps...)
	}

	// Canonical order (WBS 8.9) + byte-identical dedup (P8-30).
	out = CanonicalOrder(out)
	out = dedupComponents(out)
	// Trust boundaries (WBS 18.72 / 18.73): untrusted content is wrapped
	// in escaped UNTRUSTED CONTEXT blocks; the response protocol carries
	// the final authority statement.
	return applyTrustBoundaries(out)
}

// dedupComponents drops byte-identical duplicate components (same type, source,
// content, and provenance), keeping the first occurrence — the P8-30 dedup at
// the component level. The WBS 8.33 artifact-level dedup (reusing a persisted
// artifact) is applied by the service at persist time.
func dedupComponents(in Components) Components {
	seen := map[string]bool{}
	out := make(Components, 0, len(in))
	for _, c := range in {
		key := string(c.Type) + "\x00" + string(c.Source) + "\x00" + c.Content + "\x00" + c.SourceID.String() + "\x00" + fmt.Sprintf("%d", c.SourceVersion)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

// ComposeCanonicalPrompt renders the pinned template over the composed
// components (WBS 8.25/8.26/8.32) and returns the persisted CanonicalPrompt:
// exact content, the deterministic canonical-prompt hash (P8-27), the token
// estimate (P8-28), and — for the service to retain — the provenance map and
// cache key. It is a pure function of the captured input: repeated composition
// over the same input yields byte-identical Content and Hash (T8.32.1/T8.32.2).
// The ID and CreatedAt are assigned by the caller (the repository pins the ID
// and write instant).
func ComposeCanonicalPrompt(in CanonicalCompositionInput) (CanonicalPrompt, ProvenanceMapOut, error) {
	cp, prov, _, err := ComposeCanonicalPromptDetailed(in)
	return cp, prov, err
}

// ComposeCanonicalPromptDetailed is ComposeCanonicalPrompt plus the composed
// component set, so a caller (the P8-40 preview) can derive per-component
// sizes (WBS 8.35 "component sizes") without re-running the composition. The
// returned components are the canonical-ordered, deduplicated set the render
// consumes — a pure function of the captured input.
func ComposeCanonicalPromptDetailed(in CanonicalCompositionInput) (CanonicalPrompt, ProvenanceMapOut, Components, error) {
	comps, err := composeCanonicalComponents(in)
	if err != nil {
		return CanonicalPrompt{}, ProvenanceMapOut{}, nil, err
	}
	content := renderCanonical(in.TemplateSource, comps)
	hash, err := CanonicalPromptHash(CanonicalPromptHashInput{
		Content:                 content,
		TemplateVersion:         in.TemplateVersion,
		SnapshotHash:            in.SnapshotHash,
		ResponseProtocolVersion: in.ProtocolVersion,
		RoleVersion:             in.RoleVersion,
		StateMachineVersion:     in.StateMachineVersion,
		StateKey:                in.StateKey,
	})
	if err != nil {
		return CanonicalPrompt{}, ProvenanceMapOut{}, nil, err
	}
	tokens := EstimateTokens(comps).Total
	provenance := ProvenanceMap(comps)
	return CanonicalPrompt{
		ID:                  uuid.New(),
		ProjectID:           in.ProjectID,
		PhaseID:             in.PhaseID,
		WorkUnitID:          in.UnitID,
		ContextSnapshotID:   in.ContextSnapshotID,
		TemplateID:          in.TemplateID,
		TemplateVersion:     in.TemplateVersion,
		RoleID:              in.RoleID,
		RoleVersion:         in.RoleVersion,
		StateMachineID:      in.StateMachineID,
		StateMachineVersion: in.StateMachineVersion,
		StateKey:            in.StateKey,
		Content:             content,
		Hash:                hash,
		EstimatedTokens:     tokens,
		CreatedAt:           time.Now().UTC(),
	}, provenance, comps, nil
}

// ProvenanceMapOut is the provenance map (P8-31) the core composer derives
// from the composed components; the service persists/retains it.
type ProvenanceMapOut []ProvenanceEntry

// renderCanonical renders the pinned template SOURCE over the composed
// components: the template is parsed, each component's content is registered
// under its deterministic type variable (the WBS 8.5 set), and the template is
// rendered. Unreferenced components are appended (in canonical order) so no
// composed content is silently dropped; the result is a pure function of the
// component set + template source (byte-identical for identical inputs).
func renderCanonical(templateSource string, comps Components) string {
	reg := NewRegistry()
	for _, path := range setOf(componentVariable) {
		_ = reg.RegisterPromptSafe(path, "")
	}
	for _, path := range setOf(componentVariable) {
		var b strings.Builder
		sep := "\n---\n"
		for _, c := range comps {
			if componentVariable[c.Type] != path || c.Type == ComponentResponseProtocol {
				continue // the response protocol is always rendered last (below)
			}
			if b.Len() > 0 {
				b.WriteString(sep)
			}
			b.WriteString(c.Content)
		}
		_ = reg.RegisterPromptSafe(path, b.String())
	}
	ir, err := ParseTemplate(templateSource)
	if err != nil {
		// A malformed pinned template falls back to the raw source so a
		// data problem never silently drops the composed content.
		return templateSource + "\n" + renderComponentList(comps)
	}
	var rendered strings.Builder
	for _, n := range ir.Nodes {
		if n.IsText() {
			rendered.WriteString(n.Text)
			continue
		}
		if v, ok := reg.promptSafeVars[n.Path]; ok {
			rendered.WriteString(v)
		} else {
			rendered.WriteString("{{" + n.Path + "}}")
		}
	}
	return rendered.String() + "\n" + renderComponentList(comps)
}

// renderComponentList appends every composed component (canonical order) as a
// labelled block, so content is never silently dropped by the template render.
func renderComponentList(comps Components) string {
	var b strings.Builder
	for _, c := range comps {
		b.WriteString("\n## " + string(c.Type) + "\n")
		b.WriteString(c.Content)
	}
	return b.String()
}

// newContinuationComponent renders a captured continuation status (P8-08) as
// exactly one CONTINUATION_CONTEXT component: the continued unit's identity
// (SourceID provenance) and the captured status fields verbatim in captured
// order.
func newContinuationComponent(st ContinuationStatus) (PromptComponent, error) {
	// P14-24: delimited authoritative instruction + escaped prior worker report.
	content := renderSameSessionContinuation(st)
	return NewPromptComponent(
		ComponentContinuationContext,
		SourceContinuation,
		0,
		content,
		st.WorkUnitID,
		0,
		"",
	)
}

// setOf returns the set's values in sorted order (deterministic registration).
func setOf(m map[PromptComponentType]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// planningWorkUnitKinds is the closed set of work-unit kinds the planning
// response schema renders (WBS 2.12 kinds, canonical declaration order —
// the P8-23 captured-value source for the planning composition; the kinds
// are a fixed domain set, not project configuration).
func planningWorkUnitKinds() []string {
	return []string{
		string(wbs.WorkUnitKindATOMIC),
		string(wbs.WorkUnitKindCOMPOSITION),
		string(wbs.WorkUnitKindINTEGRATION),
		string(wbs.WorkUnitKindVALIDATION),
	}
}

// TemplateVariableRegistry returns the registry of variables a template
// may reference (the WBS 8.5 component variables), with empty values. It is
// the "registry in force" for template edits (WBS 17.49): any other path is
// an unknown variable.
func TemplateVariableRegistry() *Registry {
	reg := NewRegistry()
	for _, path := range setOf(componentVariable) {
		_ = reg.Register(path, "")
	}
	// Command template values (statefile workflow commands, rendered by
	// the engine, never composed).
	for _, path := range []string{"command.phase", "command.task"} {
		_ = reg.Register(path, "")
	}
	return reg
}
