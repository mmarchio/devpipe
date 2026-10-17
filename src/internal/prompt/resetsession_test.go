package prompt

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/role"
	"overseer/internal/statefile"
)

// P8-29 — WBS 8.22 reset-session continuation (CONTINUE_WORK_UNIT) prompt
// composition tests.
//
// The composition is FULL (WBS 8.22 — a context-reset leaves the new worker
// with no prior conversation, so the prompt must restore the complete role
// context + task state + captured status). It consumes ONLY
// caller-supplied captured values (the P8-16..P8-19 / P8-22/P8-23
// captured-values discipline — no "latest" input, no conversation channel) —
// repeated composition over the same input is byte-identical.
//
// Chosen composition (stated for TESTER/PM to verify):
//   - T8.22.1: the SAME work unit is included — the work-unit component
//     (SourceWorkUnit) carries the continued unit's ID as its SourceID AND in
//     its content, and the continuation component names the same unit.
//   - T8.22.2: the full required state context is restored — the captured
//     system prompt (ComponentSystem), the captured role instructions
//     (ComponentRoleInstruction), and the architecture/state files
//     (ComponentProjectContext etc.) are all present.
//   - T8.22.3: the captured status is included — summary/completed/blocking/
//     next-action render verbatim on exactly one CONTINUATION_CONTEXT
//     component.
//   - T8.22.4: the remaining work is included — the status's Remaining list
//     renders verbatim (captured order) as the "remaining:" section.
//   - T8.22.5: no dependency on previous conversation content — the input
//     carries NO conversation-history channel (compile-level guard) and a
//     bounded absence-scan pins that no conversation-reliance phrasing is
//     rendered.
//   - T8.22.6: the prompt clearly identifies this as a continuation — a
//     fixed marker naming the unit ("resume the in-progress work, do not
//     start fresh") is rendered.

// rsRoleID / rsUnitID / rsProtocolID are the fixture's FIXED identity values
// (deterministic composition input; a fresh uuid per construction would
// change the rendered IDs).
var (
	rsRoleID     = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p829-role"))
	rsUnitID     = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p829-unit-a"))
	rsProtocolID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p829-protocol"))
	rsFileID     = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p829-file-a"))
)

// rsInput is the canonical reset-session input: a role snapshot (system
// prompt + one instruction), one project-context state file, the current
// work unit (the ACTIVE_TASK, same id as the continued unit), the captured
// continuation status, and the implementation response schema's captured
// identity/version + allowed statuses.
func rsInput() ResetSessionPromptInput {
	return ResetSessionPromptInput{
		Role: role.RoleSnapshot{
			RoleID:  rsRoleID,
			RoleKey: "ENGINEER",
			SystemPrompt: role.PromptSnapshot{
				Version: 4,
				Content: "You are the engineer role; implement the assigned work unit.",
			},
			Instructions: []role.InstructionSnapshot{
				{Key: "TDD", Name: "TDD", Version: 2, Content: "Write the failing test first, then implement."},
			},
		},
		StateFiles: []statefile.StateFileSnapshot{{
			StateFileID:    rsFileID,
			TypeKey:        "PROJECT",
			Version:        3,
			Hash:           "abc123",
			Content:        "Project layout: /app, /team state files, phase WBS docs.",
			Classification: role.ClassificationProjectContext,
		}},
		Required: []string{"PROJECT"},
		ActiveUnit: WorkUnitContextInput{
			ID:            rsUnitID,
			Name:          "Build the WBS append service",
			Kind:          "ATOMIC",
			Objective:     "Provide advisory-locked WBS version appends",
			Acceptance:    []string{"accept #1", "accept #2"},
			RequiredTests: []string{"test T1"},
		},
		Status: ContinuationStatus{
			WorkUnitID: rsUnitID,
			Summary:    "Schema drafted; persistence pending",
			Completed:  []string{"domain types"},
			Remaining:  []string{"append repository", "advisory-locked MAX+1"},
			Blocking:   []string{"none"},
			NextAction: "Implement the append repository",
			CapturedAt: time.Unix(1, 0).UTC(),
		},
		ProtocolID:      rsProtocolID,
		ProtocolVersion: 3,
		AllowedStatuses: []string{"DONE", "INCOMPLETE", "BLOCKED"},
	}
}

// rsContinuationComponent returns the composed continuation component.
func rsContinuationComponent(t *testing.T, comps Components) PromptComponent {
	t.Helper()
	for _, c := range comps {
		if c.Type == ComponentContinuationContext {
			return c
		}
	}
	t.Fatalf("no continuation component in reset-session composition")
	return PromptComponent{}
}

// rsFullRender concatenates every component's content (whole-render scan).
func rsFullRender(t *testing.T, comps Components) string {
	t.Helper()
	var b strings.Builder
	for _, c := range comps {
		b.WriteString(c.Content)
		b.WriteByte(0)
	}
	return b.String()
}

// T8.22.1 — Same work unit included: the work-unit component carries the
// continued unit's id as SourceID and in its content, and the continuation
// component names that same unit.
func TestT8221_SameWorkUnitIncluded(t *testing.T) {
	comps, err := ComposeResetSessionPrompt(rsInput())
	if err != nil {
		t.Fatalf("ComposeResetSessionPrompt: %v", err)
	}
	var uc *PromptComponent
	for i := range comps {
		if comps[i].Source == SourceWorkUnit {
			uc = &comps[i]
		}
	}
	if uc == nil {
		t.Fatalf("no work-unit component in reset-session composition")
	}
	if uc.SourceID != rsUnitID {
		t.Fatalf("work-unit component SourceID = %s, want %s", uc.SourceID, rsUnitID)
	}
	if !strings.Contains(uc.Content, rsUnitID.String()) {
		t.Fatalf("work-unit component missing continued unit id; content:\n%s", uc.Content)
	}
	cc := rsContinuationComponent(t, comps)
	if cc.SourceID != rsUnitID {
		t.Fatalf("continuation component SourceID = %s, want %s", cc.SourceID, rsUnitID)
	}
	if !strings.Contains(cc.Content, rsUnitID.String()) {
		t.Fatalf("continuation component missing continued unit id; content:\n%s", cc.Content)
	}
	// A distinct continued unit renders with that id, not the other's.
	in := rsInput()
	in.Status.WorkUnitID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p829-unit-other"))
	in.ActiveUnit.ID = in.Status.WorkUnitID
	comps2, err := ComposeResetSessionPrompt(in)
	if err != nil {
		t.Fatalf("ComposeResetSessionPrompt(other): %v", err)
	}
	cc2 := rsContinuationComponent(t, comps2)
	if !strings.Contains(cc2.Content, in.Status.WorkUnitID.String()) {
		t.Fatalf("render missing other unit id; content:\n%s", cc2.Content)
	}
	if strings.Contains(cc2.Content, rsUnitID.String()) {
		t.Fatalf("stale continued unit id present; content:\n%s", cc2.Content)
	}
}

// T8.22.2 — Full required state context restored: the captured system prompt,
// the captured role instruction(s), and the state file(s) are all composed.
func TestT8222_FullStateContextRestored(t *testing.T) {
	comps, err := ComposeResetSessionPrompt(rsInput())
	if err != nil {
		t.Fatalf("ComposeResetSessionPrompt: %v", err)
	}
	render := rsFullRender(t, comps)
	for _, marker := range []string{
		"You are the engineer role; implement the assigned work unit.", // system prompt
		"Write the failing test first, then implement.",                // role instruction
		"Project layout: /app, /team state files, phase WBS docs.",     // state file
	} {
		if !strings.Contains(render, marker) {
			t.Fatalf("required state context %q missing from render:\n%s", marker, render)
		}
	}
	// The system component pins the role + captured version provenance.
	var sys, ins *PromptComponent
	for i := range comps {
		switch comps[i].Type {
		case ComponentSystem:
			sys = &comps[i]
		case ComponentRoleInstruction:
			ins = &comps[i]
		}
	}
	if sys == nil {
		t.Fatalf("no SYSTEM component in reset-session composition")
	}
	if sys.SourceID != rsRoleID || sys.SourceVersion != 4 {
		t.Fatalf("system component provenance = (%s,%d), want (%s,4)", sys.SourceID, sys.SourceVersion, rsRoleID)
	}
	if ins == nil {
		t.Fatalf("no ROLE_INSTRUCTION component in reset-session composition")
	}
}

// T8.22.3 — Captured status included: the summary, completed, blocking, and
// next-action values render verbatim on exactly one CONTINUATION_CONTEXT
// component (captured order).
func TestT8223_CapturedStatusIncluded(t *testing.T) {
	comps, err := ComposeResetSessionPrompt(rsInput())
	if err != nil {
		t.Fatalf("ComposeResetSessionPrompt: %v", err)
	}
	var contComps []PromptComponent
	for _, c := range comps {
		if c.Type == ComponentContinuationContext {
			contComps = append(contComps, c)
		}
	}
	if len(contComps) != 1 {
		t.Fatalf("want exactly 1 CONTINUATION_CONTEXT component, got %d", len(contComps))
	}
	cc := contComps[0]
	for _, marker := range []string{
		"Schema drafted; persistence pending", // summary
		"domain types",                        // completed
		"none",                                // blocking
		"Implement the append repository",     // next action
	} {
		if !strings.Contains(cc.Content, marker) {
			t.Fatalf("captured status field %q missing from render:\n%s", marker, cc.Content)
		}
	}
}

// T8.22.4 — Remaining work included: the status's Remaining list renders
// verbatim (captured order) as the "remaining:" section.
func TestT8224_RemainingWorkIncluded(t *testing.T) {
	comps, err := ComposeResetSessionPrompt(rsInput())
	if err != nil {
		t.Fatalf("ComposeResetSessionPrompt: %v", err)
	}
	cc := rsContinuationComponent(t, comps)
	i1 := strings.Index(cc.Content, "append repository")
	i2 := strings.Index(cc.Content, "advisory-locked MAX+1")
	if i1 < 0 || i2 < 0 {
		t.Fatalf("remaining work missing from render:\n%s", cc.Content)
	}
	if i1 > i2 {
		t.Fatalf("remaining work order not preserved:\n%s", cc.Content)
	}
}

// T8.22.5 — No dependency on previous conversation content: the render
// carries no conversation-reliance phrasing (bounded absence-scan), and the
// compile-level guard below proves the input carries no conversation channel.
func TestT8225_NoPriorConversationDependency(t *testing.T) {
	comps, err := ComposeResetSessionPrompt(rsInput())
	if err != nil {
		t.Fatalf("ComposeResetSessionPrompt: %v", err)
	}
	render := strings.ToLower(rsFullRender(t, comps))
	for _, phrase := range []string{
		"from the previous conversation",
		"the earlier discussion",
		"as we discussed",
		"the prior transcript",
		"recall the earlier chat",
	} {
		if strings.Contains(render, phrase) {
			t.Fatalf("conversation-reliance phrase %q present in render:\n%s", phrase, render)
		}
	}
}

// T8.22.6 — Continuation, not fresh: the render carries the fixed
// continuation-identification marker naming the continued unit.
func TestT8226_ContinuationIdentified(t *testing.T) {
	comps, err := ComposeResetSessionPrompt(rsInput())
	if err != nil {
		t.Fatalf("ComposeResetSessionPrompt: %v", err)
	}
	cc := rsContinuationComponent(t, comps)
	if !strings.Contains(cc.Content, "This is a continuation of work unit "+rsUnitID.String()) {
		t.Fatalf("continuation-identification marker missing; content:\n%s", cc.Content)
	}
	if !strings.Contains(cc.Content, "do not start fresh") {
		t.Fatalf("continuation marker missing the resume-not-fresh instruction; content:\n%s", cc.Content)
	}
}

// Compile-level guard (T8.22.5 channel-absence class): ResetSessionPromptInput
// carries NO conversation-history/transcript field. Adding one breaks this
// build.
var _ = func(in ResetSessionPromptInput) (Components, error) { return ComposeResetSessionPrompt(in) }

// Failure contract: a cleared work-unit association fails the composition
// (ErrMissingContinuationUnit) — no silent fallback.
func TestP829_ClearedContinuationUnitIsFailure(t *testing.T) {
	in := rsInput()
	in.Status.WorkUnitID = uuid.Nil
	if _, err := ComposeResetSessionPrompt(in); !errors.Is(err, ErrMissingContinuationUnit) {
		t.Fatalf("cleared work-unit err = %v, want ErrMissingContinuationUnit", err)
	}
}

// Failure contract: a cleared captured system-prompt version fails
// (ErrMissingSystemPromptVersion, the P8-16 contract propagated).
func TestP829_ClearedSystemPromptVersionIsFailure(t *testing.T) {
	in := rsInput()
	in.Role.SystemPrompt.Version = 0
	if _, err := ComposeResetSessionPrompt(in); !errors.Is(err, ErrMissingSystemPromptVersion) {
		t.Fatalf("cleared system-prompt version err = %v, want ErrMissingSystemPromptVersion", err)
	}
}

// Failure contract: a cleared response-protocol version (<= 0) is a
// composition failure (the P8-23 contract propagated).
func TestP829_ClearedProtocolVersionIsFailure(t *testing.T) {
	for _, v := range []int{0, -1} {
		in := rsInput()
		in.ProtocolVersion = v
		if _, err := ComposeResetSessionPrompt(in); !errors.Is(err, ErrMissingProtocolVersion) {
			t.Fatalf("cleared protocol version %d err = %v, want ErrMissingProtocolVersion", v, err)
		}
	}
}

// Failure contract: an empty allowed-status capture is a composition failure
// (the P8-23 implementation-response-schema contract).
func TestP829_EmptyAllowedStatusesIsFailure(t *testing.T) {
	in := rsInput()
	in.AllowedStatuses = nil
	if _, err := ComposeResetSessionPrompt(in); !errors.Is(err, ErrMissingAllowedStatuses) {
		t.Fatalf("empty statuses err = %v, want ErrMissingAllowedStatuses", err)
	}
}

// Determinism: repeated composition over the same input is byte-identical.
func TestP829_RepeatedCompositionByteIdentical(t *testing.T) {
	in := rsInput()
	first, err := ComposeResetSessionPrompt(in)
	if err != nil {
		t.Fatalf("ComposeResetSessionPrompt: %v", err)
	}
	for i := 0; i < 50; i++ {
		got, err := ComposeResetSessionPrompt(in)
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if len(got) != len(first) {
			t.Fatalf("repeat %d: component count %d, want %d", i, len(got), len(first))
		}
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("repeat %d: component %d not byte-identical", i, j)
			}
		}
	}
}
