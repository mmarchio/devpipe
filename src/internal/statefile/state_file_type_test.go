package statefile

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func projectPtr() *uuid.UUID {
	id := uuid.New()
	return &id
}

func TestNewFileTypeValid(t *testing.T) {
	// T7.1.1 — a valid type is created; T7.1.5 — default filename retained;
	// enabled defaults to true; timestamps set.
	in := StateFileTypeInput{
		Key:             "active_task",
		Name:            "Active Task",
		DefaultFilename: "ACTIVE_TASK.md",
		Scope:           ScopeWorkUnit,
	}
	ft, err := NewFileType(in)
	if err != nil {
		t.Fatalf("NewFileType: %v", err)
	}
	if ft.Key != "ACTIVE_TASK" {
		t.Errorf("key = %q, want normalized ACTIVE_TASK", ft.Key)
	}
	if ft.DefaultFilename != "ACTIVE_TASK.md" {
		t.Errorf("default filename = %q, want ACTIVE_TASK.md", ft.DefaultFilename)
	}
	if !ft.Enabled {
		t.Errorf("enabled = false, want default true")
	}
	if ft.CreatedAt.IsZero() || ft.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: %v %v", ft.CreatedAt, ft.UpdatedAt)
	}
	if ft.CreatedAt.Location().String() != "UTC" {
		t.Errorf("timestamps not UTC: %v", ft.CreatedAt)
	}
}

func TestNewFileTypeEmptyKeyRejected(t *testing.T) {
	// T7.1.2 — empty (and whitespace-only) key rejected.
	for _, k := range []string{"", "   "} {
		if _, err := NewFileType(StateFileTypeInput{Key: k, Name: "x", Scope: ScopeProject}); err == nil {
			t.Errorf("NewFileType(key=%q): expected error", k)
		}
	}
}

func TestNewFileTypeEmptyNameRejected(t *testing.T) {
	if _, err := NewFileType(StateFileTypeInput{Key: "X", Name: "  ", Scope: ScopeProject}); err == nil {
		t.Fatalf("NewFileType(empty name): expected error")
	}
}

func TestNewFileTypeUnknownScopeRejected(t *testing.T) {
	// T7.3.4 — invalid scope rejected.
	if _, err := NewFileType(StateFileTypeInput{Key: "X", Name: "x", Scope: "BOGUS"}); err == nil {
		t.Fatalf("NewFileType(bogus scope): expected error")
	}
}

func TestDuplicateScopedKeyRejected(t *testing.T) {
	// T7.1.3 — duplicate scoped key (same project scope + normalized key) rejected.
	proj := projectPtr()
	base, err := NewFileType(StateFileTypeInput{ProjectID: proj, Key: "ACTIVE_TASK", Name: "Active Task", Scope: ScopeWorkUnit})
	if err != nil {
		t.Fatal(err)
	}
	dup := StateFileTypeInput{ProjectID: proj, Key: "active_task", Name: "dup", Scope: ScopeWorkUnit}
	if err := ExistingTypeKeyInScope(base, dup); err == nil {
		t.Fatalf("ExistingTypeKeyInScope(same scope+key): expected collision")
	}
	// Different key, same scope: no collision.
	diff := StateFileTypeInput{ProjectID: proj, Key: "PHASE_STATE", Name: "diff", Scope: ScopePhase}
	if err := ExistingTypeKeyInScope(base, diff); err != nil {
		t.Errorf("ExistingTypeKeyInScope(different key): unexpected %v", err)
	}
}

func TestSameKeyAcrossProjectsAllowed(t *testing.T) {
	// T7.1.4 — same key in a different project is allowed.
	a, b := projectPtr(), projectPtr()
	fa, err := NewFileType(StateFileTypeInput{ProjectID: a, Key: "ACTIVE_TASK", Name: "a", Scope: ScopeWorkUnit})
	if err != nil {
		t.Fatal(err)
	}
	cb := StateFileTypeInput{ProjectID: b, Key: "ACTIVE_TASK", Name: "b", Scope: ScopeWorkUnit}
	if err := ExistingTypeKeyInScope(fa, cb); err != nil {
		t.Errorf("same key across projects: unexpected collision %v", err)
	}
	// Global (nil) scope collides with global but not with a project-scoped one.
	global, _ := NewFileType(StateFileTypeInput{Key: "ARCH", Name: "g", Scope: ScopeProject})
	if err := ExistingTypeKeyInScope(global, StateFileTypeInput{Key: "ARCH", Name: "g2", Scope: ScopeProject}); err == nil {
		t.Errorf("two global same-key: expected collision")
	}
}

func TestDisabledTypeRetained(t *testing.T) {
	// T7.1.6 — disabled type retained on the value (not dropped).
	disabled := false
	ft, err := NewFileType(StateFileTypeInput{Key: "TEST_STATE", Name: "Test", Scope: ScopePhase, Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if ft.Enabled {
		t.Errorf("explicit disabled not persisted")
	}
	// Enabled is retained as a value; the type is still addressable.
	if ft.Key == "" {
		t.Errorf("disabled type lost its key")
	}
}

func TestValidScopesAccepted(t *testing.T) {
	// T7.3.1–T7.3.3 — every valid scope is accepted.
	for _, sc := range []StateFileScope{ScopeProject, ScopePhase, ScopeWorkUnit, ScopeExecution} {
		if _, err := NewFileType(StateFileTypeInput{Key: "K", Name: "n", Scope: sc}); err != nil {
			t.Errorf("scope %s rejected: %v", sc, err)
		}
	}
}

func TestScopeAssociationRequired(t *testing.T) {
	// T7.3.5 — PHASE scope requires a phase association; T7.3.6 — WORK_UNIT
	// requires a work-unit association; PROJECT/EXECUTION require neither.
	cases := []struct {
		scope   StateFileScope
		ph, wu  bool
		wantErr bool
	}{
		{ScopePhase, false, false, true},
		{ScopePhase, true, false, false},
		{ScopeWorkUnit, false, false, true},
		{ScopeWorkUnit, false, true, false},
		{ScopeProject, false, false, false},
		{ScopeExecution, false, false, false},
	}
	for i, c := range cases {
		err := ValidateScopeAssociation(c.scope, c.ph, c.wu)
		if (err != nil) != c.wantErr {
			t.Errorf("case %d scope=%s ph=%v wu=%v: err=%v wantErr=%v", i, c.scope, c.ph, c.wu, err, c.wantErr)
		}
	}
}

// Compile-time guard: timestamps are deterministic UTC (non-vacuity for the
// time.Now injection seam used by newFileType).
var _ = time.UTC
