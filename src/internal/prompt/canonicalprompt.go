package prompt

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CanonicalPrompt is the persisted record of one exact canonical prompt
// (WBS 8.26): the project/phase/work-unit associations, the
// context-snapshot + template (id+version) + role (id+version) +
// state-machine (id+version+state key) provenance, and the exact prompt
// content + hash + estimated-token count. Every association is retained and
// the content is stored exactly (byte-for-byte), so a historical prompt is
// reconstructable after every source advances (the P8-26/WBS 8.48
// reconstruction contract).
type CanonicalPrompt struct {
	ID uuid.UUID

	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	ContextSnapshotID uuid.UUID

	TemplateID      uuid.UUID
	TemplateVersion int

	RoleID      uuid.UUID
	RoleVersion int

	StateMachineID      uuid.UUID
	StateMachineVersion int
	StateKey            string

	Content         string
	Hash            string
	EstimatedTokens int

	CreatedAt time.Time
}

// CanonicalPromptInput carries the caller-supplied fields for
// NewCanonicalPrompt. PhaseID and WorkUnitID are legitimately optional (nil =
// not bound); every other association/version is required (a canonical prompt
// always pins specific versions — never "latest"). ID and CreatedAt are
// assigned/passed through by the caller (the repository pins the ID and the
// write instant).
type CanonicalPromptInput struct {
	ID uuid.UUID

	ProjectID  uuid.UUID
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	ContextSnapshotID uuid.UUID

	TemplateID      uuid.UUID
	TemplateVersion int

	RoleID      uuid.UUID
	RoleVersion int

	StateMachineID      uuid.UUID
	StateMachineVersion int
	StateKey            string

	Content         string
	Hash            string
	EstimatedTokens int

	CreatedAt time.Time
}

// NewCanonicalPrompt validates and constructs a canonical prompt (WBS 8.26):
// the required association/identity fields (ProjectID, ContextSnapshotID,
// TemplateID/TemplateVersion, RoleID/RoleVersion, StateMachineID/
// StateMachineVersion, StateKey) must be present with positive versions and a
// non-blank state key, and the content must be non-empty (a canonical prompt
// always carries exact content). The record is a byte-stable pass-through:
// no list fields to copy, timestamps passed through verbatim. PhaseID and
// WorkUnitID may be nil (T8.26.2-T8.26.5; see canonicalprompt_test.go).
func NewCanonicalPrompt(in CanonicalPromptInput) (CanonicalPrompt, error) {
	if in.ProjectID == uuid.Nil {
		return CanonicalPrompt{}, fmt.Errorf("prompt: canonical prompt requires a project ID")
	}
	if in.ContextSnapshotID == uuid.Nil {
		return CanonicalPrompt{}, fmt.Errorf("prompt: canonical prompt requires a context snapshot ID")
	}
	if in.TemplateID == uuid.Nil || in.TemplateVersion <= 0 {
		return CanonicalPrompt{}, fmt.Errorf("prompt: canonical prompt requires a pinned template version")
	}
	if in.RoleID == uuid.Nil || in.RoleVersion <= 0 {
		return CanonicalPrompt{}, fmt.Errorf("prompt: canonical prompt requires a pinned role version")
	}
	if in.StateMachineID == uuid.Nil || in.StateMachineVersion <= 0 {
		return CanonicalPrompt{}, fmt.Errorf("prompt: canonical prompt requires a pinned state-machine version")
	}
	if strings.TrimSpace(in.StateKey) == "" {
		return CanonicalPrompt{}, fmt.Errorf("prompt: canonical prompt requires a state key")
	}
	if in.Content == "" {
		return CanonicalPrompt{}, fmt.Errorf("prompt: canonical prompt requires exact content")
	}
	phase := in.PhaseID
	unit := in.WorkUnitID
	return CanonicalPrompt{
		ID:                  in.ID,
		ProjectID:           in.ProjectID,
		PhaseID:             phase,
		WorkUnitID:          unit,
		ContextSnapshotID:   in.ContextSnapshotID,
		TemplateID:          in.TemplateID,
		TemplateVersion:     in.TemplateVersion,
		RoleID:              in.RoleID,
		RoleVersion:         in.RoleVersion,
		StateMachineID:      in.StateMachineID,
		StateMachineVersion: in.StateMachineVersion,
		StateKey:            in.StateKey,
		Content:             in.Content,
		Hash:                in.Hash,
		EstimatedTokens:     in.EstimatedTokens,
		CreatedAt:           in.CreatedAt,
	}, nil
}
