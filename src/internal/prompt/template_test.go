package prompt

import (
	"testing"

	"github.com/google/uuid"
)

// T8.1.1 — Valid template created.
func TestT811ValidTemplateCreated(t *testing.T) {
	tmpl, err := NewTemplate(TemplateInput{
		ProjectID:   nil,
		Key:         "implement-work-unit",
		Name:        "Implementation prompt",
		Description: "Composes the implementation worker prompt.",
		Purpose:     PurposeImplementation,
	})
	if err != nil {
		t.Fatalf("NewTemplate: %v", err)
	}
	if tmpl.ID == uuid.Nil {
		t.Fatalf("new template has no ID")
	}
	if tmpl.Key != "IMPLEMENT-WORK-UNIT" {
		t.Fatalf("key = %q, want normalized IMPLEMENT-WORK-UNIT", tmpl.Key)
	}
	if tmpl.Purpose != PurposeImplementation {
		t.Fatalf("purpose = %q, want IMPLEMENTATION", tmpl.Purpose)
	}
	if tmpl.Version != 1 {
		t.Fatalf("initial version = %d, want 1", tmpl.Version)
	}
	if !tmpl.Enabled {
		t.Fatalf("new template must start enabled")
	}
	if tmpl.CreatedAt.IsZero() || tmpl.UpdatedAt.IsZero() {
		t.Fatalf("timestamps must be set on creation")
	}
}

// T8.1.2 — Empty key rejected.
func TestT812EmptyKeyRejected(t *testing.T) {
	for _, in := range []TemplateInput{
		{Key: "", Purpose: PurposeValidation},
		{Key: "   ", Purpose: PurposeValidation},
		{Key: "\t\n", Purpose: PurposeValidation},
	} {
		if _, err := NewTemplate(in); err == nil {
			t.Fatalf("key %q: expected rejection, got none", in.Key)
		}
	}
}

// T8.1.3 — Duplicate scoped key rejected; same key in a different project
// scope is allowed.
func TestT813DuplicateScopedKeyRejected(t *testing.T) {
	base, err := NewTemplate(TemplateInput{Key: "implement", Purpose: PurposeImplementation, Name: "I"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := templateDupKey(base, TemplateInput{Key: "IMPLEMENT", Purpose: PurposeImplementation}); err == nil {
		t.Fatalf("duplicate global scoped key accepted")
	}
	pid := uuid.New()
	proj, err := NewTemplate(TemplateInput{ProjectID: &pid, Key: "validate", Purpose: PurposeValidation, Name: "V"})
	if err != nil {
		t.Fatalf("seed project template: %v", err)
	}
	if err := templateDupKey(proj, TemplateInput{ProjectID: &pid, Key: "validate", Purpose: PurposeValidation}); err == nil {
		t.Fatalf("duplicate project-scoped key accepted")
	}
	// Different project scope with the same key is allowed.
	pidB := uuid.New()
	if err := templateDupKey(proj, TemplateInput{ProjectID: &pidB, Key: "validate", Purpose: PurposeValidation}); err != nil {
		t.Fatalf("same key in a different project scope must be allowed: %v", err)
	}
	// Project scope vs global scope is allowed.
	if err := templateDupKey(proj, TemplateInput{Key: "validate", Purpose: PurposeValidation}); err != nil {
		t.Fatalf("same key in project scope vs global scope must be allowed: %v", err)
	}
}

// T8.1.4 — Valid purpose accepted.
func TestT814ValidPurposeAccepted(t *testing.T) {
	purposes := []PromptPurpose{
		PurposePhasePlanning,
		PurposeImplementation,
		PurposeContinuation,
		PurposeValidation,
		PurposeReview,
		PurposeControl,
		PurposeCustom,
	}
	for _, p := range purposes {
		if !p.Known() {
			t.Fatalf("purpose %q must be known", p)
		}
		if _, err := NewTemplate(TemplateInput{Key: "k", Purpose: p}); err != nil {
			t.Fatalf("purpose %q: %v", p, err)
		}
	}
}

// T8.1.5 — Unknown purpose rejected.
func TestT815UnknownPurposeRejected(t *testing.T) {
	for _, p := range []PromptPurpose{"", "WORKER", "PHASE_PLAN", "implementation", "custom "} {
		if p.Known() {
			t.Fatalf("purpose %q must be unknown", p)
		}
		if _, err := NewTemplate(TemplateInput{Key: "k", Purpose: p}); err == nil {
			t.Fatalf("purpose %q: expected rejection, got none", p)
		}
	}
}

// T8.1.6 — Initial version is 1.
func TestT816InitialVersion(t *testing.T) {
	for i := 0; i < 3; i++ {
		tmpl, err := NewTemplate(TemplateInput{Key: "review", Purpose: PurposeReview})
		if err != nil {
			t.Fatalf("NewTemplate: %v", err)
		}
		if tmpl.Version != 1 {
			t.Fatalf("template %d initial version = %d, want 1", i, tmpl.Version)
		}
	}
}

// templateDupKey checks the candidate against an existing template for
// scoped key duplication. A nil ProjectID on both sides is the global scope.
func templateDupKey(existing *PromptTemplate, in TemplateInput) error {
	return ExistingKeyInScope(existing, in.Key, in.ProjectID)
}
