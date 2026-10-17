package statefile

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// StateFileScope classifies which runtime association a state file is bound
// to (WBS 7.3). A scope determines which project/phase/work-unit association
// a concrete state file of this scope must carry.
type StateFileScope string

// The initial scope set (WBS 7.3).
const (
	ScopeProject   StateFileScope = "PROJECT"
	ScopePhase     StateFileScope = "PHASE"
	ScopeWorkUnit  StateFileScope = "WORK_UNIT"
	ScopeExecution StateFileScope = "EXECUTION"
)

// Known reports whether s is one of the initial scopes (T7.3.4 rejects
// anything else at construction).
func (s StateFileScope) Known() bool {
	switch s {
	case ScopeProject, ScopePhase, ScopeWorkUnit, ScopeExecution:
		return true
	}
	return false
}

// SourceClass classifies how a state file's content is produced (WBS 7.16).
// It drives the mutation/import policy predicates below, which P7-08 (CRUD
// guard) and P7-16 (drift/import) consume verbatim.
type SourceClass string

// The source classification set (WBS 7.16).
const (
	SourceDerived  SourceClass = "DERIVED"
	SourceManaged  SourceClass = "MANAGED"
	SourceUser     SourceClass = "USER"
	SourceExternal SourceClass = "EXTERNAL"
)

// Known reports whether sc is a recognized source classification (T7.16.1).
func (sc SourceClass) Known() bool {
	switch sc {
	case SourceDerived, SourceManaged, SourceUser, SourceExternal:
		return true
	}
	return false
}

// StateFileType is a logical state-file type defined independently of any
// filesystem path (WBS 7.1). A type is project-scoped when ProjectID is set
// and global (nil ProjectID) when shared across projects; the (project scope,
// normalized key) pair is the uniqueness scope.
type StateFileType struct {
	ID        uuid.UUID
	ProjectID *uuid.UUID

	Key         string
	Name        string
	Description string

	// DefaultFilename is the preferred materialization name (e.g.
	// "ACTIVE_TASK.md"). It is a default, not an authoritative path;
	// application logic must not branch on it (T7.1.5 retains it).
	DefaultFilename string

	Scope StateFileScope

	// Source classifies how this type's content is produced
	// (WBS 7.16) and selects its mutation/import policy.
	Source SourceClass

	Required bool
	Enabled  bool

	CreatedAt time.Time
	UpdatedAt time.Time
}

// StateFileTypeInput carries the caller-supplied fields for NewFileType. ID
// and timestamps are assigned by the domain; Key is normalized (T7.1.2);
// Enabled defaults to true unless an explicit value is supplied (nil means
// "not specified", mirroring the state-machine Enabled policy).
type StateFileTypeInput struct {
	ProjectID       *uuid.UUID
	Key             string
	Name            string
	Description     string
	DefaultFilename string
	Scope           StateFileScope
	Required        bool

	// Source is the caller-supplied source classification (WBS 7.16); nil
	// means "not specified" and defaults to SourceManaged (the controlled-
	// update class), mirroring the Enabled pointer policy.
	Source  *SourceClass
	Enabled *bool
}

// NewFileType validates and constructs a new state-file type (WBS 7.1/7.3):
// a normalized non-empty key (T7.1.2 rejects empty), a non-empty name, a
// known scope (T7.3.4), a retained default filename (T7.1.5), Required and
// Enabled persisted on the value (T7.1.6 keeps a disabled type), and UTC
// timestamps set (T7.1.1).
func NewFileType(in StateFileTypeInput) (*StateFileType, error) {
	return newFileType(in, time.Now)
}

func newFileType(in StateFileTypeInput, now func() time.Time) (*StateFileType, error) {
	key := NormalizeKey(in.Key)
	if key == "" {
		return nil, fmt.Errorf("statefile: key must not be empty")
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("statefile: name must not be empty")
	}
	if !in.Scope.Known() {
		return nil, fmt.Errorf("statefile: unknown scope %q", in.Scope)
	}
	source := SourceManaged
	if in.Source != nil {
		if !in.Source.Known() {
			return nil, fmt.Errorf("statefile: unknown source class %q", *in.Source)
		}
		source = *in.Source
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	ts := now().UTC()
	return &StateFileType{
		ID:              uuid.New(),
		ProjectID:       in.ProjectID,
		Key:             key,
		Name:            strings.TrimSpace(in.Name),
		Description:     in.Description,
		DefaultFilename: in.DefaultFilename,
		Scope:           in.Scope,
		Source:          source,
		Required:        in.Required,
		Enabled:         enabled,
		CreatedAt:       ts,
		UpdatedAt:       ts,
	}, nil
}

// NormalizeKey applies the same key-normalization policy as the role and
// state domains: trim surrounding whitespace, then uppercase. This keeps
// state-file keys consistent with the keys used elsewhere in the workflow.
func NormalizeKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// ExistingTypeKeyInScope reports an error when a candidate type collides with
// an existing type's scoped key (T7.1.3): the same normalized key AND the
// same project scope (both global, or the same project). The same key in a
// different project is allowed (T7.1.4).
func ExistingTypeKeyInScope(existing *StateFileType, candidate StateFileTypeInput) error {
	if existing == nil {
		return nil
	}
	if NormalizeKey(candidate.Key) != existing.Key {
		return nil
	}
	if sameProjectScope(existing.ProjectID, candidate.ProjectID) {
		return fmt.Errorf("statefile: key %q already exists in this project scope", existing.Key)
	}
	return nil
}

// sameProjectScope reports whether two type scopes refer to the same
// project-level uniqueness scope: both global (nil) or the same project.
func sameProjectScope(a, b *uuid.UUID) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// ValidateScopeAssociation enforces the WBS 7.3 association rule: a
// PHASE-scoped file requires a phase association (T7.3.5) and a
// WORK_UNIT-scoped file requires a work-unit association (T7.3.6); PROJECT
// and EXECUTION scopes require neither. It is a pure check over the
// association flags so the StateFile entity (P7-02) can reuse it before any
// persistence.
func ValidateScopeAssociation(scope StateFileScope, hasPhase bool, hasWorkUnit bool) error {
	if scope == ScopePhase && !hasPhase {
		return fmt.Errorf("statefile: PHASE scope requires a phase association")
	}
	if scope == ScopeWorkUnit && !hasWorkUnit {
		return fmt.Errorf("statefile: WORK_UNIT scope requires a work-unit association")
	}
	return nil
}

// DirectMutationAllowed is the unauthorized-direct-mutation policy (T7.16.3):
// a DERIVED file's content is only produced by re-derivation, so a direct
// content update without a derivation re-run is refused. USER/EXTERNAL
// content is equally foreign to the controlled writer and is refused the
// same way; a MANAGED file is the only class that tolerates neither a
// derivation nor an import, so its direct-mutation refusal is the same as
// DERIVED's — the class difference is that MANAGED additionally offers a
// controlled update path.
func DirectMutationAllowed(sc SourceClass) error {
	switch sc {
	case SourceDerived, SourceManaged, SourceUser, SourceExternal:
		return fmt.Errorf("statefile: %s content requires %s", sc, controlledPath(sc))
	}
	return fmt.Errorf("statefile: unknown source class %q", sc)
}

// ControlledUpdateAllowed reports whether the class has a controlled update
// path (T7.16.4): MANAGED (controlled update) and USER (user edit) always;
// EXTERNAL only via explicit import (P7-16); DERIVED never — its content is
// re-derived, not updated.
func ControlledUpdateAllowed(sc SourceClass) bool {
	switch sc {
	case SourceManaged, SourceUser, SourceExternal:
		return true
	}
	return false
}

// ReDerivationAllowed reports whether the class's content is (re)produced by
// Overseer's derivation: only DERIVED. EXTERNAL content is never re-derived;
// MANAGED/USER content is updated, not derived.
func ReDerivationAllowed(sc SourceClass) bool {
	return sc == SourceDerived
}

func controlledPath(sc SourceClass) string {
	switch sc {
	case SourceDerived:
		return "a derivation re-run"
	case SourceExternal:
		return "an explicit import"
	case SourceUser:
		return "a controlled update"
	default:
		return "a controlled update"
	}
}
