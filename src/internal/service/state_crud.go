// P6-25 (WBS 6.35): the service half of the state-machine CRUD REST API.
// The endpoints resolve the project's machine family, serve the current
// (MAX-version) definition through the P6-19 snapshot read model, publish
// new versions through the P6-17 versioning service, and list / read
// historical versions. No parallel machine-definition or versioning path:
// this service composes the two prior services, it does not re-read the
// machine tables for definitions itself.
package service

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/state"
)

// CurrentMachine is the REST view of one machine version (WBS 6.35): the
// P6-19 snapshot (states, transitions, initial state, configuration hash)
// plus the family's name.
type CurrentMachine struct {
	*state.StateMachineSnapshot
	Name string
}

// MachineVersionEntry is one row of a machine family's version list
// (WBS 6.35): the version number and the row's identity/liveness fields.
type MachineVersionEntry struct {
	Version   int
	MachineID string
	Enabled   bool
	UpdatedAt time.Time
}

// StateMachineCRUDService composes the P6-17 versioning service (publish a
// new version) and the P6-19 snapshot service (assemble a version) over
// the machine repository (family resolution). It is the single service
// behind the WBS 6.35 REST surface.
type StateMachineCRUDService struct {
	machines  persistence.StateMachineRepository
	snapshot  *StateMachineSnapshotService
	edit      *StateMachineVersioningService
	validator *StateMachineValidationService
}

// WithValidation wires the P6-16 validation service (WBS 6.38, T6.38.x):
// the Validate op composes it — no parallel validation path. Nil-safe:
// when unwired, Validate reports an infrastructure error, so prior
// construction sites keep their contract.
func (s *StateMachineCRUDService) WithValidation(v *StateMachineValidationService) *StateMachineCRUDService {
	s.validator = v
	return s
}

// NewStateMachineCRUDService wires the CRUD service to the machine
// repository and the two prior-task services.
func NewStateMachineCRUDService(
	machines persistence.StateMachineRepository,
	snapshot *StateMachineSnapshotService,
	edit *StateMachineVersioningService,
) *StateMachineCRUDService {
	return &StateMachineCRUDService{
		machines: machines,
		snapshot: snapshot,
		edit:     edit,
	}
}

// resolveFamily locates the project's machine family: the family whose
// current (MAX-version) row has the highest version. When two families
// tie on version, the lexicographically smallest family id wins, so the
// resolution is deterministic. A project with no machines reports
// ErrNotFound (the WBS 6.35 endpoints assume a configured machine).
func (s *StateMachineCRUDService) resolveFamily(ctx context.Context, projectID string) (familyID uuid.UUID, currentVersion int, name string, err error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return uuid.Nil, 0, "", ErrInvalidInput
	}
	list, err := s.machines.ListByProject(ctx, pid)
	if err != nil {
		return uuid.Nil, 0, "", translate(err)
	}
	var best *persistence.MachineRecord
	for _, m := range list {
		if best == nil ||
			m.Version > best.Version ||
			(m.Version == best.Version && m.FamilyID.String() < best.FamilyID.String()) {
			best = m
		}
	}
	if best == nil {
		return uuid.Nil, 0, "", ErrNotFound
	}
	return best.FamilyID, best.Version, best.Name, nil
}

// Validate returns the validation report (WBS 6.38) for the project's
// CURRENT machine (the family's MAX-version row, the same resolution as
// Get), composed through the P6-16 validation service — never a second
// validation path. A project with no machine reports ErrNotFound (404);
// the op is non-mutating (the validator is read-only).
func (s *StateMachineCRUDService) Validate(ctx context.Context, projectID string) (*ValidationReport, error) {
	if s.validator == nil {
		return nil, ErrInfrastructure
	}
	familyID, version, _, err := s.resolveFamily(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rec, err := s.machines.GetVersion(ctx, familyID, version)
	if err != nil {
		return nil, translate(err)
	}
	return s.validator.Validate(ctx, rec.ID.String())
}

// Get returns the project's current machine (WBS 6.35, T6.35.1): the
// family's MAX-version machine assembled through the P6-19 snapshot
// (definition + version + hash).
func (s *StateMachineCRUDService) Get(ctx context.Context, projectID string) (*CurrentMachine, error) {
	familyID, version, name, err := s.resolveFamily(ctx, projectID)
	if err != nil {
		return nil, err
	}
	snap, err := s.snapshot.Snapshot(ctx, familyID.String(), version)
	if err != nil {
		return nil, err
	}
	return &CurrentMachine{StateMachineSnapshot: snap, Name: name}, nil
}

// Edit publishes in as a NEW version of the family (WBS 6.35, T6.35.2)
// through the P6-17 versioning service, returning the new current machine.
// expectedVersion is an optional optimistic guard: when non-nil it must
// equal the family's current version, otherwise the edit is rejected with
// ErrConflict BEFORE any write (T6.35.5 — a stale edit is a conflict, not
// an input error). An invalid definition is rejected with ErrInvalidInput
// by the versioning service before any write (T6.35.4).
func (s *StateMachineCRUDService) Edit(ctx context.Context, projectID string, in *MachineEditInput, expectedVersion *int) (*CurrentMachine, error) {
	familyID, currentVersion, _, err := s.resolveFamily(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if expectedVersion != nil && *expectedVersion != currentVersion {
		return nil, ErrConflict
	}
	if _, err := s.edit.Edit(ctx, familyID.String(), in); err != nil {
		return nil, err
	}
	return s.Get(ctx, projectID)
}

// ListVersions returns the family's stored versions in ascending order
// (WBS 6.35, T6.35.3).
func (s *StateMachineCRUDService) ListVersions(ctx context.Context, projectID string) ([]MachineVersionEntry, error) {
	familyID, _, _, err := s.resolveFamily(ctx, projectID)
	if err != nil {
		return nil, err
	}
	recs, err := s.machines.ListVersions(ctx, familyID)
	if err != nil {
		return nil, translate(err)
	}
	out := make([]MachineVersionEntry, 0, len(recs))
	for _, m := range recs {
		out = append(out, MachineVersionEntry{
			Version: m.Version, MachineID: m.ID.String(),
			Enabled: m.Enabled, UpdatedAt: m.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// GetVersion returns one stored version of the family (WBS 6.35,
// T6.35.3): unknown versions report ErrNotFound (404).
func (s *StateMachineCRUDService) GetVersion(ctx context.Context, projectID string, version int) (*CurrentMachine, error) {
	familyID, _, name, err := s.resolveFamily(ctx, projectID)
	if err != nil {
		return nil, err
	}
	snap, err := s.snapshot.Snapshot(ctx, familyID.String(), version)
	if err != nil {
		return nil, err
	}
	return &CurrentMachine{StateMachineSnapshot: snap, Name: name}, nil
}
