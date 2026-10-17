package service

import (
	"context"
	"fmt"
	"overseer/internal/bounds"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/role"
)

// InstructionView is the API-facing instruction record (WBS 5.23).
type InstructionView struct {
	ID        string
	ProjectID *string
	Key       string
	Name      string
	Content   string
	Version   int
	Enabled   bool
}

func toInstructionView(r *persistence.InstructionRecord) InstructionView {
	v := InstructionView{
		ID:      r.ID.String(),
		Key:     r.Key,
		Name:    r.Name,
		Content: r.Content,
		Version: r.Version,
		Enabled: r.Enabled,
	}
	if r.ProjectID != nil {
		s := r.ProjectID.String()
		v.ProjectID = &s
	}
	return v
}

// CreateInstruction creates a reusable instruction document in the given scope
// (WBS 5.23): the instructions row plus its v1 content version. A scoped key
// duplicate is ErrConflict.
func (s *RoleService) CreateInstruction(ctx context.Context, projectID *string, key, name, description, content string) (*InstructionView, error) {
	if err := bounds.Check(bounds.Instruction, content); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	var pid *uuid.UUID
	if projectID != nil && *projectID != "" && *projectID != "global" {
		u := parseRoleID(*projectID)
		pid = &u
	}
	in, err := role.NewInstruction(role.InstructionInput{
		ProjectID: pid, Key: key, Name: name, Description: description, Content: content,
	})
	if err != nil {
		return nil, err
	}
	rec := &persistence.InstructionRecord{
		ID: in.ID, ProjectID: in.ProjectID, Key: in.Key, Name: in.Name,
		Description: in.Description, Content: in.Content,
		Version: in.Version, Enabled: in.Enabled, CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt,
	}
	if err := s.instructions.Create(ctx, rec); err != nil {
		return nil, err
	}
	if _, err := s.instructionVersions.AppendVersion(ctx, rec.ID.String(), content); err != nil {
		return nil, err
	}
	v := toInstructionView(rec)
	return &v, nil
}

// ListInstructions lists instructions in a scope (WBS 5.23): projectID
// nil/"" = global, set = that project's instructions.
func (s *RoleService) ListInstructions(ctx context.Context, projectID *string) ([]*InstructionView, error) {
	var pid *uuid.UUID
	if projectID != nil && *projectID != "" && *projectID != "global" {
		u := parseRoleID(*projectID)
		pid = &u
	}
	out, err := s.instructions.ListByScope(ctx, pid)
	if err != nil {
		return nil, err
	}
	views := make([]*InstructionView, 0, len(out))
	for _, r := range out {
		v := toInstructionView(r)
		views = append(views, &v)
	}
	return views, nil
}

// GetInstruction returns one instruction by id (WBS 5.23); ErrNotFound when
// missing.
func (s *RoleService) GetInstruction(ctx context.Context, id string) (*InstructionView, error) {
	r, err := s.instructions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	v := toInstructionView(r)
	return &v, nil
}

// UpdateInstruction stores new content as a NEW version (WBS 5.23 T5.23.3),
// using the expected-version conflict rule (T5.8.5): a stale expected version
// is ErrConflict.
func (s *RoleService) UpdateInstruction(ctx context.Context, id, content string, expectedVersion int) (*InstructionView, error) {
	if err := bounds.Check(bounds.Instruction, content); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	r, err := s.instructions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	stored, err := s.instructionVersions.UpdateWithExpected(ctx, r.ID.String(), content, expectedVersion)
	if err != nil {
		return nil, err
	}
	updated, err := s.instructions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	v := toInstructionView(updated)
	// The instruction_versions table is the source of truth for the current
	// version; surface it on the view (WBS 5.23 T5.23.3).
	v.Version = stored.Version
	v.Content = stored.Content
	return &v, nil
}

// DeleteInstruction removes an instruction (WBS 5.23) unless it is referenced
// by a role (T5.23.5 — a referenced instruction is protected; disable it
// instead).
func (s *RoleService) DeleteInstruction(ctx context.Context, id string) error {
	r, err := s.instructions.Get(ctx, id)
	if err != nil {
		return err
	}
	if s.assignments != nil {
		if refs, err := s.assignments.ListByInstruction(ctx, r.ID); err == nil && len(refs) > 0 {
			return persistence.ErrConflict
		}
	}
	return s.instructions.Delete(ctx, r.ID)
}
