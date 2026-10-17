// P8-41 (WBS 8.36): the prompt history read op.
//
// PromptHistoryService lists and retrieves a project's persisted canonical
// prompts for debugging, supporting conjunctive filters by phase, work unit,
// state key, role, and template. Both ops are pure reads: no CanonicalPrompt
// row is created, mutated, or deleted. Project isolation is enforced — the
// list is scoped to the project; the by-id lookup returns ErrNotFound for an
// id that belongs to a different project.
package service

import (
	"context"

	"github.com/google/uuid"

	"overseer/internal/persistence"
)

// PromptHistoryFilter carries the optional, conjunctive filters (WBS 8.36):
// each non-nil field narrows the result set. An empty/zero value means "no
// filter on that dimension".
type PromptHistoryFilter struct {
	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID
	StateKey   *string
	RoleID     *uuid.UUID
	TemplateID *uuid.UUID
}

// PromptHistoryRecord is the read model for one persisted canonical prompt
// (WBS 8.36): the full provenance plus the exact content and hash.
type PromptHistoryRecord struct {
	ID                  uuid.UUID
	PhaseID             *uuid.UUID
	WorkUnitID          *uuid.UUID
	ContextSnapshotID   uuid.UUID
	TemplateID          uuid.UUID
	TemplateVersion     int
	RoleID              uuid.UUID
	RoleVersion         int
	StateMachineID      uuid.UUID
	StateMachineVersion int
	StateKey            string
	Content             string
	Hash                string
	EstimatedTokens     int
	CreatedAt           string
}

// PromptHistoryService reads a project's canonical prompt history.
type PromptHistoryService struct {
	repos persistence.CanonicalPromptRepository
}

// NewPromptHistoryService wires the history read op.
func NewPromptHistoryService(repos persistence.CanonicalPromptRepository) *PromptHistoryService {
	return &PromptHistoryService{repos: repos}
}

// List returns the project's canonical prompts matching the given filter
// (WBS 8.36.5). An empty filter returns all. Results are ordered by
// created_at ASC, id ASC (the repository's existing creation-order contract).
// A project with no matching prompts returns an empty slice, not an error.
func (s *PromptHistoryService) List(ctx context.Context, projectID string, f PromptHistoryFilter) ([]*PromptHistoryRecord, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	records, err := s.repos.ListByProject(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}
	var out []*PromptHistoryRecord
	for _, rec := range records {
		if !matchesFilter(rec, &f) {
			continue
		}
		out = append(out, toHistoryRecord(rec))
	}
	return out, nil
}

// Get returns one canonical prompt by id, scoped to the project (WBS 8.36.1,
// T8.36.4). An id that does not exist in the project yields ErrNotFound.
func (s *PromptHistoryService) Get(ctx context.Context, projectID, promptID string) (*PromptHistoryRecord, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rid, err := uuid.Parse(promptID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rec, err := s.repos.Get(ctx, rid)
	if err != nil {
		return nil, translate(err)
	}
	if rec.ProjectID != pid {
		return nil, ErrNotFound
	}
	return toHistoryRecord(rec), nil
}

// matchesFilter returns true when the record satisfies every non-nil filter
// dimension (conjunctive).
func matchesFilter(rec *persistence.CanonicalPromptRecord, f *PromptHistoryFilter) bool {
	if f.PhaseID != nil {
		if rec.PhaseID == nil || *rec.PhaseID != *f.PhaseID {
			return false
		}
	}
	if f.WorkUnitID != nil {
		if rec.WorkUnitID == nil || *rec.WorkUnitID != *f.WorkUnitID {
			return false
		}
	}
	if f.StateKey != nil {
		if rec.StateKey != *f.StateKey {
			return false
		}
	}
	if f.RoleID != nil {
		if rec.RoleID != *f.RoleID {
			return false
		}
	}
	if f.TemplateID != nil {
		if rec.TemplateID != *f.TemplateID {
			return false
		}
	}
	return true
}

// toHistoryRecord projects a persistence record into the read model.
func toHistoryRecord(rec *persistence.CanonicalPromptRecord) *PromptHistoryRecord {
	return &PromptHistoryRecord{
		ID:                  rec.ID,
		PhaseID:             rec.PhaseID,
		WorkUnitID:          rec.WorkUnitID,
		ContextSnapshotID:   rec.ContextSnapshotID,
		TemplateID:          rec.TemplateID,
		TemplateVersion:     rec.TemplateVersion,
		RoleID:              rec.RoleID,
		RoleVersion:         rec.RoleVersion,
		StateMachineID:      rec.StateMachineID,
		StateMachineVersion: rec.StateMachineVersion,
		StateKey:            rec.StateKey,
		Content:             rec.Content,
		Hash:                rec.Hash,
		EstimatedTokens:     rec.EstimatedTokens,
		CreatedAt:           rec.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
}
