package statefile

import (
	"testing"
)

// sourcePolicy tests the WBS 7.16 source-classification policy (T7.16.1–
// T7.16.4). The DB-persisted half of T7.16.1 rides with P7-05 (migration
// 00034); this file pins the pure-domain half: the classification is a
// recognized value carried on the type, and the mutation/import policy
// predicates that P7-08 (CRUD guard) and P7-16 (drift/import) consume.

func TestSourceTypeClassificationPersistedOnValue(t *testing.T) {
	// T7.16.1 — source classification is a stored, recognized value: every
	// known class is accepted at construction and carried on the type.
	for _, sc := range []SourceClass{SourceDerived, SourceManaged, SourceUser, SourceExternal} {
		ft, err := NewFileType(StateFileTypeInput{
			Key:    "K" + string(sc),
			Name:   "n",
			Scope:  ScopeProject,
			Source: &sc,
		})
		if err != nil {
			t.Fatalf("NewFileType(source=%s): %v", sc, err)
		}
		if ft.Source != sc {
			t.Errorf("source = %q, want %q", ft.Source, sc)
		}
		if !ft.Source.Known() {
			t.Errorf("stored source %q not recognized", ft.Source)
		}
	}
}

func TestSourceDefaultClass(t *testing.T) {
	// T7.16.1 (default half) — a type constructed without an explicit
	// classification defaults to MANAGED: the controlled-update class, so a
	// bare type (e.g. PROJECT_STATE) is mutable without an import ceremony.
	ft, err := NewFileType(StateFileTypeInput{Key: "PROJECT_STATE", Name: "n", Scope: ScopeProject})
	if err != nil {
		t.Fatalf("NewFileType: %v", err)
	}
	if ft.Source != SourceManaged {
		t.Errorf("default source = %q, want %q", ft.Source, SourceManaged)
	}
}

func TestSourceTypeInvalidRejected(t *testing.T) {
	// T7.16.2 — an unrecognized source classification is rejected at
	// construction.
	for _, bogus := range []SourceClass{"", "VENDOR", "derived"} {
		if _, err := NewFileType(StateFileTypeInput{Key: "K", Name: "n", Scope: ScopeProject, Source: &bogus}); err == nil {
			t.Errorf("NewFileType(source=%q): expected error", bogus)
		}
	}
}

func TestSourcePolicyDerivedRejectsDirectMutation(t *testing.T) {
	// T7.16.3 — a DERIVED file rejects unauthorized direct mutation: a
	// direct content update without a derivation re-run is refused.
	ft, err := NewFileType(StateFileTypeInput{
		Key: "ACTIVE_TASK", Name: "n", Scope: ScopeWorkUnit,
		Source: func() *SourceClass { s := SourceDerived; return &s }(),
	})
	if err != nil {
		t.Fatalf("NewFileType: %v", err)
	}
	if ft.Source != SourceDerived {
		t.Fatalf("derived type lost its source classification: %q", ft.Source)
	}
	if err := DirectMutationAllowed(ft.Source); err == nil {
		t.Fatalf("DERIVED direct mutation: expected refusal, got none")
	}
	// The re-derivation path is the authorized mutation: it is permitted.
	if !ReDerivationAllowed(ft.Source) {
		t.Errorf("DERIVED re-derivation refused, want permitted")
	}
}

func TestSourcePolicyManagedPermitsControlledUpdate(t *testing.T) {
	// T7.16.4 — a MANAGED file permits controlled update: the controlled
	// update path is allowed, while an UNAUTHORIZED direct mutation is
	// refused (same direct-mutation refusal as DERIVED — the distinction
	// between the classes is that MANAGED additionally has a controlled
	// update path, which DERIVED lacks).
	ft, err := NewFileType(StateFileTypeInput{
		Key: "PROJECT_STATE", Name: "n", Scope: ScopeProject,
		Source: func() *SourceClass { s := SourceManaged; return &s }(),
	})
	if err != nil {
		t.Fatalf("NewFileType: %v", err)
	}
	if !ControlledUpdateAllowed(ft.Source) {
		t.Errorf("MANAGED controlled update refused, want permitted")
	}
	if DirectMutationAllowed(ft.Source) == nil {
		t.Errorf("MANAGED unauthorized direct mutation permitted, want refusal")
	}
}

func TestSourcePolicyUserAndExternal(t *testing.T) {
	// USER/EXTERNAL policy (documented choice, pinned here so P7-08/P7-16
	// consume it verbatim):
	//   USER — user-authored content: controlled update permitted (the user
	//   edits through the controlled path), unauthorized direct mutation
	//   refused, no re-derivation.
	//   EXTERNAL — content produced outside Overseer: controlled update
	//   permitted ONLY via explicit import (P7-16); direct mutation refused,
	//   re-derivation refused (an external file is never re-derived by us).
	user, err := NewFileType(StateFileTypeInput{
		Key: "ARCHITECTURE", Name: "n", Scope: ScopeProject,
		Source: func() *SourceClass { s := SourceUser; return &s }(),
	})
	if err != nil {
		t.Fatalf("NewFileType(user): %v", err)
	}
	if !ControlledUpdateAllowed(user.Source) {
		t.Errorf("USER controlled update refused, want permitted")
	}
	if DirectMutationAllowed(user.Source) == nil {
		t.Errorf("USER direct mutation permitted, want refusal")
	}
	if ReDerivationAllowed(user.Source) {
		t.Errorf("USER re-derivation permitted, want refused")
	}

	ext, err := NewFileType(StateFileTypeInput{
		Key: "EXTERNAL_NOTES", Name: "n", Scope: ScopeProject,
		Source: func() *SourceClass { s := SourceExternal; return &s }(),
	})
	if err != nil {
		t.Fatalf("NewFileType(external): %v", err)
	}
	if !ControlledUpdateAllowed(ext.Source) {
		t.Errorf("EXTERNAL import (controlled) update refused, want permitted")
	}
	if DirectMutationAllowed(ext.Source) == nil {
		t.Errorf("EXTERNAL direct mutation permitted, want refusal")
	}
	if ReDerivationAllowed(ext.Source) {
		t.Errorf("EXTERNAL re-derivation permitted, want refused")
	}
}
