package role

import (
	"testing"

	"github.com/google/uuid"
)

// T5.1.1 — Valid role created.
func TestT511ValidRoleCreated(t *testing.T) {
	r, err := NewRole(RoleInput{
		ProjectID:   nil,
		Key:         "engineer",
		Name:        "Implementation Engineer",
		Description: "Implements assigned work units.",
	})
	if err != nil {
		t.Fatalf("NewRole: %v", err)
	}
	if r.ID == uuid.Nil {
		t.Fatalf("new role has no ID")
	}
	if r.Key != "ENGINEER" {
		t.Fatalf("key = %q, want normalized ENGINEER", r.Key)
	}
	if r.Name != "Implementation Engineer" || r.Description != "Implements assigned work units." {
		t.Fatalf("name/description not preserved: %q / %q", r.Name, r.Description)
	}
	if r.Version != 1 {
		t.Fatalf("initial version = %d, want 1", r.Version)
	}
	if !r.Enabled {
		t.Fatalf("new role must start enabled")
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		t.Fatalf("timestamps must be set on creation")
	}
}

// T5.1.2 — Empty role key rejected.
func TestT512EmptyKeyRejected(t *testing.T) {
	for _, in := range []RoleInput{
		{Key: "", Name: "N"},
		{Key: "   ", Name: "N"},
		{Key: "\t\n", Name: "N"},
	} {
		if _, err := NewRole(in); err == nil {
			t.Fatalf("key %q: expected rejection, got none", in.Key)
		}
	}
}

// T5.1.3 — Empty role name rejected.
func TestT513EmptyNameRejected(t *testing.T) {
	for _, name := range []string{"", "   "} {
		if _, err := NewRole(RoleInput{Key: "engineer", Name: name}); err == nil {
			t.Fatalf("name %q: expected rejection, got none", name)
		}
	}
}

// T5.1.4 — Role key normalized according to documented policy.
func TestT514KeyNormalized(t *testing.T) {
	cases := map[string]string{
		"engineer":        "ENGINEER",
		"  engineer  ":    "ENGINEER",
		"\tTESTER\n":      "TESTER",
		"project-manager": "PROJECT-MANAGER",
	}
	for in, want := range cases {
		got := NormalizeKey(in)
		if got != want {
			t.Fatalf("NormalizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// T5.1.5 — Duplicate key within same scope rejected.
func TestT515DuplicateScopedKeyRejected(t *testing.T) {
	base, err := NewRole(RoleInput{Key: "engineer", Name: "E"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Same global scope.
	if err := roleDupKey(base, RoleInput{Key: "ENGINEER", Name: "E2"}); err == nil {
		t.Fatalf("duplicate global key accepted")
	}
	// Same project scope.
	pid := uuid.New()
	proj, err := NewRole(RoleInput{ProjectID: &pid, Key: "tester", Name: "T"})
	if err != nil {
		t.Fatalf("seed project role: %v", err)
	}
	if err := roleDupKey(proj, RoleInput{ProjectID: &pid, Key: "tester", Name: "T2"}); err == nil {
		t.Fatalf("duplicate project-scoped key accepted")
	}
}

// T5.1.6 — Same key allowed in different projects (and globally).
func TestT516SameKeyDifferentProjects(t *testing.T) {
	pidA, pidB := uuid.New(), uuid.New()
	a, err := NewRole(RoleInput{ProjectID: &pidA, Key: "engineer", Name: "A"})
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	b, err := NewRole(RoleInput{ProjectID: &pidB, Key: "engineer", Name: "B"})
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	g, err := NewRole(RoleInput{Key: "engineer", Name: "G"})
	if err != nil {
		t.Fatalf("G: %v", err)
	}
	if err := roleDupKey(a, RoleInput{ProjectID: &pidB, Key: "engineer", Name: "B2"}); err != nil {
		t.Fatalf("same key in different project scope must be allowed: %v", err)
	}
	if err := roleDupKey(a, RoleInput{Key: "engineer", Name: "G2"}); err != nil {
		t.Fatalf("same key in project scope vs global scope must be allowed: %v", err)
	}
	_ = b
	_ = g
}

// T5.1.7 — Initial version is 1.
func TestT517InitialVersion(t *testing.T) {
	for i := 0; i < 3; i++ {
		r, err := NewRole(RoleInput{Key: "planner", Name: "P"})
		if err != nil {
			t.Fatalf("NewRole: %v", err)
		}
		if r.Version != 1 {
			t.Fatalf("role %d initial version = %d, want 1", i, r.Version)
		}
	}
}

// roleDupKey checks the candidate against existing role r for scoped key
// duplication. A nil ProjectID on both sides is the global scope.
func roleDupKey(existing *Role, in RoleInput) error {
	return ExistingKeyInScope(existing, in.Key, in.ProjectID)
}

// Compile-level guard: the documented normalization policy is a pure string
// function (T5.1.4) — no DB, no state.
var _ = func(s string) string { return NormalizeKey(s) }
