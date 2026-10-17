package persistence

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// StateFileTypeRecord is the domain-facing view of a stored state-file type
// (WBS 7.1). A nil ProjectID is the GLOBAL scope; a set value is a project
// scope. The same normalized key may exist in different project scopes but
// never twice in one scope (T7.1.3/T7.1.4).
type StateFileTypeRecord struct {
	ID        uuid.UUID
	ProjectID *uuid.UUID

	Key         string
	Name        string
	Description string

	// DefaultFilename is the preferred materialization name (T7.1.5).
	DefaultFilename string

	Scope  string
	Source string

	Required bool
	Enabled  bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// StateFileRecord is the domain-facing view of a stored logical state file
// (WBS 7.4). CurrentVersion points at the content version appended in
// P7-06; it starts at 0 for a file without content.
type StateFileRecord struct {
	ID uuid.UUID

	ProjectID uuid.UUID
	TypeID    uuid.UUID

	Scope string

	PhaseID    *uuid.UUID
	WorkUnitID *uuid.UUID

	CurrentVersion int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// StateFileTypeRepository is the domain-facing persistence contract for
// state-file types (WBS 7.1/7.4 persistence half). Scoped-key uniqueness is
// enforced at the database level: a duplicate (project scope, key) yields
// ErrConflict.
type StateFileTypeRepository interface {
	// Create inserts a new type; a scoped key duplicate yields
	// ErrConflict.
	Create(ctx context.Context, r *StateFileTypeRecord) error
	// Get returns the type with the given id; ErrNotFound when missing.
	Get(ctx context.Context, id uuid.UUID) (*StateFileTypeRecord, error)
	// ListByScope lists all types in one scope: projectID nil lists the
	// global scope, a set id lists that project's types. Ordered by key.
	ListByScope(ctx context.Context, projectID *uuid.UUID) ([]*StateFileTypeRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) StateFileTypeRepository
}

// StateFileRepository is the domain-facing persistence contract for logical
// state files (WBS 7.4/7.7). Association FKs reject cross-project phase and
// work-unit references (T7.4.5/T7.4.6).
type StateFileRepository interface {
	// Create inserts a new file; a duplicate (type, scope, association)
	// yields ErrConflict and a cross-project association yields
	// ErrInvalidReference.
	Create(ctx context.Context, r *StateFileRecord) error
	// Get returns the file with the given id; ErrNotFound when missing.
	Get(ctx context.Context, id uuid.UUID) (*StateFileRecord, error)
	// GetByTypeAndScope returns the unique file for a (type, scope,
	// association) pair; phaseID/workUnitID are nil for associations
	// that do not apply. ErrNotFound when no such file exists.
	GetByTypeAndScope(ctx context.Context, projectID, typeID uuid.UUID, scope string, phaseID, workUnitID *uuid.UUID) (*StateFileRecord, error)
	// ListByProject lists all files of a project. Ordered by id.
	ListByProject(ctx context.Context, projectID uuid.UUID) ([]*StateFileRecord, error)
	// ListByPhase lists all files associated with a phase. Ordered by id.
	ListByPhase(ctx context.Context, phaseID uuid.UUID) ([]*StateFileRecord, error)
	// ListByWorkUnit lists all files associated with a work unit.
	// Ordered by id.
	ListByWorkUnit(ctx context.Context, workUnitID uuid.UUID) ([]*StateFileRecord, error)
	// WithTx returns a copy bound to the given transaction (WBS 2.21).
	WithTx(tx Transaction) StateFileRepository
}
