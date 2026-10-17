package statefile

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// StateFile is the logical, project-scoped, versioned state-file entity
// (WBS 7.4). It carries no content itself: CurrentVersion points at the
// versioned content that is appended in P7-03/P7-06. Scope determines the
// project/phase/work-unit association the file must carry (WBS 7.3).
type StateFile struct {
	ID uuid.UUID

	ProjectID uuid.UUID
	TypeID    uuid.UUID

	Scope StateFileScope

	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	CurrentVersion int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// StateFileInput carries the caller-supplied fields for NewStateFile. ID and
// timestamps are assigned by the domain; CurrentVersion always starts at 0
// (no content yet — the first version arrives with content in P7-03/P7-06).
type StateFileInput struct {
	ProjectID *uuid.UUID
	TypeID    *uuid.UUID

	Scope StateFileScope

	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID
}

// NewStateFile validates and constructs a new logical state file (WBS 7.4):
// type required (T7.4.2), project required (T7.4.3), the scope association is
// enforced via P7-01's ValidateScopeAssociation (T7.4.4), CurrentVersion is 0,
// and UTC timestamps are set (T7.4.1).
func NewStateFile(in StateFileInput) (*StateFile, error) {
	return newStateFile(in, time.Now)
}

func newStateFile(in StateFileInput, now func() time.Time) (*StateFile, error) {
	if in.TypeID == nil {
		return nil, fmt.Errorf("statefile: type is required")
	}
	if in.ProjectID == nil {
		return nil, fmt.Errorf("statefile: project is required")
	}
	if err := ValidateScopeAssociation(in.Scope, in.PhaseID != nil, in.WorkUnitID != nil); err != nil {
		return nil, err
	}
	ts := now().UTC()
	return &StateFile{
		ID:             uuid.New(),
		ProjectID:      *in.ProjectID,
		TypeID:         *in.TypeID,
		Scope:          in.Scope,
		PhaseID:        in.PhaseID,
		WorkUnitID:     in.WorkUnitID,
		CurrentVersion: 0,
		CreatedAt:      ts,
		UpdatedAt:      ts,
	}, nil
}

// AssociationBelongsToProject is the domain-level cross-project association
// guard (T7.4.5/T7.4.6): a PHASE-scoped file's phase and a WORK_UNIT-scoped
// file's work unit must belong to the file's own project. The association's
// owning project is passed by the caller (resolved from the phase/work-unit
// row at the service layer); the DB FK enforces the same rule at persistence
// (P7-05). A file with no association (PROJECT/EXECUTION scope) is
// trivially satisfied for any project.
func (f *StateFile) AssociationBelongsToProject(associationProject uuid.UUID) error {
	if f == nil {
		return nil
	}
	switch f.Scope {
	case ScopePhase:
		if f.PhaseID == nil {
			return nil
		}
		if f.ProjectID != associationProject {
			return fmt.Errorf("statefile: phase association belongs to a different project")
		}
		return nil
	case ScopeWorkUnit:
		if f.WorkUnitID == nil {
			return nil
		}
		if f.ProjectID != associationProject {
			return fmt.Errorf("statefile: work-unit association belongs to a different project")
		}
		return nil
	}
	return nil
}
