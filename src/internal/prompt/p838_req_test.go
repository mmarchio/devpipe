package prompt

import (
	"testing"

	"github.com/google/uuid"

	"overseer/internal/role"
)

// P8-38 — WBS 8.25 composition request, tests T8.25.1–T8.25.4.
//
// The request is an explicit object replacing loosely related arguments
// (WBS 8.25). It pins the snapshot, the template identity+version, and an
// optional continuation status (whose work-unit association is validated
// against the snapshot's bound unit — T8.25.4; the P8-08
// ValidateAssociation contract).

var (
	p838SnapID  = uuid.New()
	p838TmplID  = uuid.New()
	p838UnitID  = uuid.New()
	p838OtherID = uuid.New()
)

// T8.25.1 — Valid request accepted.
func TestT8251_ValidRequestAccepted(t *testing.T) {
	req, err := NewComposePromptRequest(ComposePromptRequest{
		ContextSnapshotID: p838SnapID,
		TemplateID:        p838TmplID,
		TemplateVersion:   1,
	})
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if req.ContextSnapshotID != p838SnapID || req.TemplateID != p838TmplID || req.TemplateVersion != 1 || req.ContinuationStatusID != nil {
		t.Fatalf("request fields not retained verbatim: %+v", req)
	}
	// A request carrying a continuation status is also valid.
	c := p838UnitID
	req2, err := NewComposePromptRequest(ComposePromptRequest{
		ContextSnapshotID:    p838SnapID,
		TemplateID:           p838TmplID,
		TemplateVersion:      2,
		ContinuationStatusID: &c,
	})
	if err != nil {
		t.Fatalf("request with continuation rejected: %v", err)
	}
	if req2.ContinuationStatusID == nil || *req2.ContinuationStatusID != p838UnitID {
		t.Fatalf("continuation id not retained: %+v", req2)
	}
}

// T8.25.2 — Missing snapshot rejected.
func TestT8252_MissingSnapshotRejected(t *testing.T) {
	_, err := NewComposePromptRequest(ComposePromptRequest{
		ContextSnapshotID: uuid.Nil,
		TemplateID:        p838TmplID,
		TemplateVersion:   1,
	})
	if err == nil {
		t.Fatalf("missing snapshot must be rejected")
	}
	if !IsMissingSnapshot(err) {
		t.Fatalf("want ErrMissingSnapshot, got %v", err)
	}
}

// T8.25.3 — Invalid template rejected (missing id OR missing/zero version).
func TestT8253_InvalidTemplateRejected(t *testing.T) {
	if _, err := NewComposePromptRequest(ComposePromptRequest{
		ContextSnapshotID: p838SnapID,
		TemplateID:        uuid.Nil,
		TemplateVersion:   1,
	}); !IsInvalidTemplate(err) {
		t.Fatalf("missing template id: want ErrInvalidTemplate, got %v", err)
	}
	if _, err := NewComposePromptRequest(ComposePromptRequest{
		ContextSnapshotID: p838SnapID,
		TemplateID:        p838TmplID,
		TemplateVersion:   0,
	}); !IsInvalidTemplate(err) {
		t.Fatalf("zero template version: want ErrInvalidTemplate, got %v", err)
	}
	if _, err := NewComposePromptRequest(ComposePromptRequest{
		ContextSnapshotID: p838SnapID,
		TemplateID:        p838TmplID,
		TemplateVersion:   -1,
	}); !IsInvalidTemplate(err) {
		t.Fatalf("negative template version: want ErrInvalidTemplate, got %v", err)
	}
}

// T8.25.4 — Continuation status validated against the work unit.
//
// The request-level half pins the CONTINUATION_STATUS association against
// a given work unit via the P8-08 ValidateAssociation contract: a
// continuation status built for unit A is accepted for unit A and
// rejected for unit B (cross-unit). A nil status (no continuation) is
// accepted for any unit.
func TestT8254_ContinuationValidatedAgainstWorkUnit(t *testing.T) {
	stA, err := NewContinuationStatus(ContinuationStatusInput{
		WorkUnitID: p838UnitID, Summary: "done so far",
	})
	if err != nil {
		t.Fatalf("NewContinuationStatus: %v", err)
	}
	if err := ValidateContinuationForUnit(stA, p838UnitID); err != nil {
		t.Fatalf("same-unit continuation must be accepted: %v", err)
	}
	if err := ValidateContinuationForUnit(stA, p838OtherID); err == nil {
		t.Fatalf("cross-unit continuation must be rejected")
	}
	// Nil (absent) status is accepted for any unit.
	if err := ValidateContinuationForUnit(nil, p838OtherID); err != nil {
		t.Fatalf("nil continuation must be accepted: %v", err)
	}
}

// T8.32.1/T8.32.2 (P8-38 share) — repeated composition over identical pinned
// inputs yields byte-identical content and hash. (The full DB-row-order
// determinism half is P8-39.)
func TestT83212_RepeatedCompositionByteIdentical(t *testing.T) {
	in := CanonicalCompositionInput{
		ProjectID: p838SnapID, ContextSnapshotID: p838SnapID,
		TemplateID: p838TmplID, TemplateVersion: 3,
		RoleID: p838UnitID, RoleVersion: 2,
		StateMachineID: p838OtherID, StateMachineVersion: 4, StateKey: "IMPLEMENT",
		Role: role.RoleSnapshot{
			RoleID: p838UnitID, RoleKey: "ENGINEER", RoleVersion: 2,
			SystemPrompt: role.PromptSnapshot{Version: 2, Content: "You are the engineer.", ContentHash: "h"},
			Instructions: []role.InstructionSnapshot{{Key: "A", Name: "a", Version: 1, Content: "instr-a", ContentHash: "h"}},
		},
		Unit:       &WorkUnitContext{ID: p838UnitID, Name: "u", Kind: "ATOMIC", Objective: "do it"},
		ProtocolID: p838OtherID, ProtocolVersion: 5, ProtocolContent: "schema body",
		AllowedStatuses: []string{"DONE", "INCOMPLETE"},
		TemplateSource:  "Work: {{work_unit.task}}\nProto: {{response_protocol.schema}}",
		SnapshotHash:    "snap-hash",
	}
	first, _, err := ComposeCanonicalPrompt(in)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, _, err := ComposeCanonicalPrompt(in)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		if again.Content != first.Content {
			t.Fatalf("repeated composition content not byte-identical")
		}
		if again.Hash != first.Hash {
			t.Fatalf("repeated composition hash not identical")
		}
		if again.EstimatedTokens != first.EstimatedTokens {
			t.Fatalf("repeated composition token estimate not identical")
		}
	}
}
