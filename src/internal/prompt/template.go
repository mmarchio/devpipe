package prompt

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Package doc: prompt contains the pure prompt-template domain model
// (WBS 8.1). No DB, web, gateway, or net/http imports (archtest-enforced).

// PromptPurpose is the template purpose discriminator (WBS 8.1).
type PromptPurpose string

// The 7 WBS purposes.
const (
	PurposePhasePlanning  PromptPurpose = "PHASE_PLANNING"
	PurposeImplementation PromptPurpose = "IMPLEMENTATION"
	PurposeContinuation   PromptPurpose = "CONTINUATION"
	PurposeValidation     PromptPurpose = "VALIDATION"
	PurposeReview         PromptPurpose = "REVIEW"
	PurposeControl        PromptPurpose = "CONTROL"
	PurposeCustom         PromptPurpose = "CUSTOM"
)

// Known reports whether p is one of the 7 WBS purposes.
func (p PromptPurpose) Known() bool {
	switch p {
	case PurposePhasePlanning,
		PurposeImplementation,
		PurposeContinuation,
		PurposeValidation,
		PurposeReview,
		PurposeControl,
		PurposeCustom:
		return true
	}
	return false
}

// PromptTemplate is the configurable prompt-template domain type (WBS 8.1).
// A nil ProjectID is the GLOBAL scope (defaults/templates); a set ProjectID
// is a PROJECT scope. Key uniqueness is scoped: the same key may exist in
// different projects and globally, but never twice within one scope
// (T8.1.3).
type PromptTemplate struct {
	ID        uuid.UUID
	ProjectID *uuid.UUID

	Key         string
	Name        string
	Description string

	Purpose PromptPurpose

	Enabled bool
	Version int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TemplateInput carries the caller-supplied fields for NewTemplate. Key is
// normalized per the documented policy before validation; timestamps and
// Version are assigned by the domain.
type TemplateInput struct {
	ProjectID   *uuid.UUID
	Key         string
	Name        string
	Description string

	Purpose PromptPurpose
}

// NormalizeKey applies the key normalization policy inherited from the
// Phase 5/6/7 precedent: trim leading/trailing whitespace, then uppercase.
// Keys are identifiers, so case-insensitive comparison is canonicalized by
// storing them uppercase; interior characters are preserved exactly.
func NormalizeKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// NewTemplate validates and constructs a new prompt template (WBS 8.1):
// normalized non-empty key (T8.1.2), known purpose (T8.1.4/T8.1.5), initial
// Version 1 (T8.1.6), Enabled true, timestamps set (T8.1.1).
func NewTemplate(in TemplateInput) (*PromptTemplate, error) {
	key := NormalizeKey(in.Key)
	if key == "" {
		return nil, fmt.Errorf("prompt: key must not be empty")
	}
	if !in.Purpose.Known() {
		return nil, fmt.Errorf("prompt: unknown purpose %q", in.Purpose)
	}
	ts := time.Now().UTC()
	return &PromptTemplate{
		ID:          uuid.New(),
		ProjectID:   in.ProjectID,
		Key:         key,
		Name:        strings.TrimSpace(in.Name),
		Description: in.Description,
		Purpose:     in.Purpose,
		Enabled:     true,
		Version:     1,
		CreatedAt:   ts,
		UpdatedAt:   ts,
	}, nil
}

// ExistingKeyInScope reports an error when a candidate (key, projectID)
// collides with an existing template's scoped key (T8.1.3): same key AND
// same scope. Different scopes (other projects, or global vs project) are
// allowed.
func ExistingKeyInScope(existing *PromptTemplate, candidateKey string, candidateProjectID *uuid.UUID) error {
	if existing == nil {
		return nil
	}
	if NormalizeKey(candidateKey) != existing.Key {
		return nil
	}
	if sameProjectID(existing.ProjectID, candidateProjectID) {
		return fmt.Errorf("prompt: key %q already exists in scope", existing.Key)
	}
	return nil
}

func sameProjectID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
