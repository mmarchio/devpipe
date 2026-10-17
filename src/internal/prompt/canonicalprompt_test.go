package prompt

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// P8-30 — WBS 8.26 canonical prompt domain model, TDD T8.26.1-T8.26.5.
//
// CanonicalPrompt is the persisted record of one exact canonical prompt:
// the project/phase/work-unit associations, the context-snapshot + template
// (id+version) + role (id+version) + state-machine (id+version+state key)
// provenance, and the exact prompt content + hash + estimated-token count.
//
// Constructor/validation policy (stated per ACTIVE_TASK NOTES): the
// REQUIRED association/identity fields (ProjectID, ContextSnapshotID,
// TemplateID/TemplateVersion, RoleID/RoleVersion, StateMachineID/
// StateMachineVersion, StateKey) must be present and the versions positive;
// PhaseID and WorkUnitID are legitimately optional (nil = not bound). The
// record carries no lists, so construction is byte-stable; timestamps are
// passed through verbatim (the repository pins the instant at write time).
//
// DB-backed half (T8.26.1-T8.26.5) lives in
// internal/integration/p830_canonical_prompt_test.go.

var (
	cpProjectID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p830-project"))
	cpPhaseID   = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p830-phase"))
	cpUnitID    = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p830-unit"))
	cpSnapID    = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p830-snapshot"))
	cpTmplID    = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p830-template"))
	cpRoleID    = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p830-role"))
	cpMachineID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("p830-machine"))
	cpFixedTS   = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
)

func cpFixture() CanonicalPromptInput {
	phase, unit := cpPhaseID, cpUnitID
	return CanonicalPromptInput{
		ProjectID:           cpProjectID,
		PhaseID:             &phase,
		WorkUnitID:          &unit,
		ContextSnapshotID:   cpSnapID,
		TemplateID:          cpTmplID,
		TemplateVersion:     3,
		RoleID:              cpRoleID,
		RoleVersion:         2,
		StateMachineID:      cpMachineID,
		StateMachineVersion: 5,
		StateKey:            "IMPLEMENT",
		Content:             "## Implementation\n\nDo the exact thing.",
		Hash:                "deadbeef",
		EstimatedTokens:     42,
		CreatedAt:           cpFixedTS,
	}
}

// T8.26.1 (domain half) — A valid canonical prompt is accepted and every
// field is retained exactly: the constructor is a byte-stable pass-through
// over the validated input (no list fields to copy; timestamps verbatim).
func TestT8261_CanonicalPromptValidatedAndRetained(t *testing.T) {
	in := cpFixture()
	got, err := NewCanonicalPrompt(in)
	if err != nil {
		t.Fatalf("NewCanonicalPrompt: %v", err)
	}
	if got.ProjectID != cpProjectID || got.ContextSnapshotID != cpSnapID ||
		got.TemplateID != cpTmplID || got.TemplateVersion != 3 ||
		got.RoleID != cpRoleID || got.RoleVersion != 2 ||
		got.StateMachineID != cpMachineID || got.StateMachineVersion != 5 ||
		got.StateKey != "IMPLEMENT" || got.Content != in.Content ||
		got.Hash != in.Hash || got.EstimatedTokens != 42 {
		t.Fatalf("field not retained: %+v", got)
	}
	if got.CreatedAt != cpFixedTS {
		t.Fatalf("created_at = %v, want passed-through %v", got.CreatedAt, cpFixedTS)
	}
}

// T8.26.2 (domain half) — The snapshot association is retained: a cleared
// (zero) ContextSnapshotID is a composition failure (the P8-16/P8-17
// typed-sentinel choice), a set snapshot id is carried verbatim.
func TestT8262_SnapshotAssociationRetained(t *testing.T) {
	in := cpFixture()
	got, err := NewCanonicalPrompt(in)
	if err != nil {
		t.Fatalf("NewCanonicalPrompt: %v", err)
	}
	if got.ContextSnapshotID != cpSnapID {
		t.Fatalf("snapshot id = %s, want %s", got.ContextSnapshotID, cpSnapID)
	}
	// Cleared snapshot id is a validation failure.
	bad := cpFixture()
	bad.ContextSnapshotID = uuid.Nil
	if _, err := NewCanonicalPrompt(bad); err == nil {
		t.Fatal("cleared snapshot id accepted, want rejection")
	}
}

// T8.26.3 (domain half) — The template version is retained: the
// (TemplateID, TemplateVersion) pair is carried verbatim; a zero
// TemplateVersion is a validation failure (a canonical prompt always pins a
// specific template version — never "latest").
func TestT8263_TemplateVersionRetained(t *testing.T) {
	got, err := NewCanonicalPrompt(cpFixture())
	if err != nil {
		t.Fatalf("NewCanonicalPrompt: %v", err)
	}
	if got.TemplateID != cpTmplID || got.TemplateVersion != 3 {
		t.Fatalf("template pin = (%s,%d), want (%s,3)", got.TemplateID, got.TemplateVersion, cpTmplID)
	}
	bad := cpFixture()
	bad.TemplateVersion = 0
	if _, err := NewCanonicalPrompt(bad); err == nil {
		t.Fatal("zero template version accepted, want rejection")
	}
}

// T8.26.4 (domain half) — The state and role versions are retained:
// (StateMachineID, StateMachineVersion, StateKey) and (RoleID, RoleVersion)
// are carried verbatim; a zero version on either is a validation failure.
func TestT8264_StateRoleVersionsRetained(t *testing.T) {
	got, err := NewCanonicalPrompt(cpFixture())
	if err != nil {
		t.Fatalf("NewCanonicalPrompt: %v", err)
	}
	if got.RoleID != cpRoleID || got.RoleVersion != 2 ||
		got.StateMachineID != cpMachineID || got.StateMachineVersion != 5 ||
		got.StateKey != "IMPLEMENT" {
		t.Fatalf("state/role pin = %+v", got)
	}
	badRole := cpFixture()
	badRole.RoleVersion = 0
	if _, err := NewCanonicalPrompt(badRole); err == nil {
		t.Fatal("zero role version accepted, want rejection")
	}
	badMachine := cpFixture()
	badMachine.StateMachineVersion = 0
	if _, err := NewCanonicalPrompt(badMachine); err == nil {
		t.Fatal("zero state-machine version accepted, want rejection")
	}
	badKey := cpFixture()
	badKey.StateKey = "   "
	if _, err := NewCanonicalPrompt(badKey); err == nil {
		t.Fatal("cleared state key accepted, want rejection")
	}
}

// T8.26.5 (domain half) — The content is retained EXACTLY (byte-for-byte):
// the record carries the raw content bytes, never a normalized/canonicalized
// variant. Whitespace and case are preserved.
func TestT8265_ContentRetainedExactly(t *testing.T) {
	const exact = "## Implementation\n\n  do the thing  \t"
	in := cpFixture()
	in.Content = exact
	got, err := NewCanonicalPrompt(in)
	if err != nil {
		t.Fatalf("NewCanonicalPrompt: %v", err)
	}
	if got.Content != exact {
		t.Fatalf("content mutated: got %q, want %q", got.Content, exact)
	}
	// A cleared content is a validation failure (a canonical prompt always
	// carries exact content).
	bad := cpFixture()
	bad.Content = ""
	if _, err := NewCanonicalPrompt(bad); err == nil {
		t.Fatal("empty content accepted, want rejection")
	}
}

// Determinism pin: repeated construction over the same input is byte-identical
// (the P8 domain-package determinism contract — no timestamp re-derivation).
func TestT8266_RepeatedConstructionByteIdentical(t *testing.T) {
	first, err := NewCanonicalPrompt(cpFixture())
	if err != nil {
		t.Fatalf("NewCanonicalPrompt: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := NewCanonicalPrompt(cpFixture())
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if !reflect.DeepEqual(again, first) {
			t.Fatalf("repeat %d: not byte-identical", i)
		}
	}
}

// Optional associations: PhaseID and WorkUnitID may be nil (a prompt is not
// always bound to a phase/unit); the record carries nils through.
func TestT8267_OptAssociationsMayBeNil(t *testing.T) {
	in := cpFixture()
	in.PhaseID = nil
	in.WorkUnitID = nil
	got, err := NewCanonicalPrompt(in)
	if err != nil {
		t.Fatalf("NewCanonicalPrompt: %v", err)
	}
	if got.PhaseID != nil || got.WorkUnitID != nil {
		t.Fatalf("optional associations not nil-passed: %+v", got)
	}
}
