package role

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Instruction is a reusable instruction document, kept separate from a
// role's system prompt (WBS 5.7). Like roles, it is scoped: a nil ProjectID
// is GLOBAL, a set value is PROJECT scope. Key uniqueness is scoped (T5.7.4).
// A disabled instruction is retained (not deleted) — exclusion happens in
// resolution, not storage (T5.7.5).
type Instruction struct {
	ID        uuid.UUID
	ProjectID *uuid.UUID

	Key         string
	Name        string
	Description string
	Content     string

	Version int
	Enabled bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// InstructionInput carries caller-supplied fields for NewInstruction.
type InstructionInput struct {
	ProjectID   *uuid.UUID
	Key         string
	Name        string
	Description string
	Content     string
}

// NewInstruction validates and constructs a new instruction (WBS 5.7):
// normalized non-empty key (T5.7.3), non-empty name, initial Version 1,
// Enabled true (T5.7.1).
func NewInstruction(in InstructionInput) (*Instruction, error) {
	key := NormalizeKey(in.Key)
	if key == "" {
		return nil, fmt.Errorf("instruction: key must not be empty")
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("instruction: name must not be empty")
	}
	ts := time.Now().UTC()
	return &Instruction{
		ID:          uuid.New(),
		ProjectID:   in.ProjectID,
		Key:         key,
		Name:        strings.TrimSpace(in.Name),
		Description: in.Description,
		Content:     in.Content,
		Version:     1,
		Enabled:     true,
		CreatedAt:   ts,
		UpdatedAt:   ts,
	}, nil
}

// InstructionDuplicateKey reports an error when a candidate instruction key
// collides with an existing instruction's scoped key (T5.7.4).
func InstructionDuplicateKey(existing *Instruction, candidateKey string, candidateProjectID *uuid.UUID) error {
	if existing == nil {
		return nil
	}
	if NormalizeKey(candidateKey) != existing.Key {
		return nil
	}
	if sameProjectID(existing.ProjectID, candidateProjectID) {
		return fmt.Errorf("instruction: key %q already exists in scope", existing.Key)
	}
	return nil
}
