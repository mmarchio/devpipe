// P6-22 (WBS 6.30): unit-level tests for the initial runtime-state
// operation over IN-MEMORY fakes (no DB). This is where the input
// validation, the invalid-machine rejection, and the idempotent duplicate
// short-circuit (no machine read, no write) are exercised; the
// integration file drives the same service over real persisted rows.
package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/persistence"
)

// --- in-memory fakes (the minimal surface Initialize uses) ---

type p622fakeMachines struct{ m *persistence.MachineRecord }

func (f *p622fakeMachines) Create(context.Context, *persistence.MachineRecord) error { return nil }
func (f *p622fakeMachines) Get(ctx context.Context, id uuid.UUID) (*persistence.MachineRecord, error) {
	if f.m == nil || f.m.ID != id {
		return nil, persistence.ErrNotFound
	}
	return f.m, nil
}
func (f *p622fakeMachines) ListByProject(context.Context, uuid.UUID) ([]*persistence.MachineRecord, error) {
	if f.m == nil {
		return nil, nil
	}
	return []*persistence.MachineRecord{f.m}, nil
}
func (f *p622fakeMachines) SetInitialState(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *p622fakeMachines) GetVersion(ctx context.Context, id uuid.UUID, version int) (*persistence.MachineRecord, error) {
	return f.Get(ctx, id)
}
func (f *p622fakeMachines) ListVersions(ctx context.Context, id uuid.UUID) ([]*persistence.MachineRecord, error) {
	m, err := f.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return []*persistence.MachineRecord{m}, nil
}
func (f *p622fakeMachines) WithTx(persistence.Transaction) persistence.StateMachineRepository {
	return f
}

type p622fakeRuntime struct {
	stored    *persistence.RuntimeStateRecord
	created   int
	createdIn []uuid.UUID
}

func (f *p622fakeRuntime) Create(ctx context.Context, rs *persistence.RuntimeStateRecord) error {
	f.created++
	f.createdIn = append(f.createdIn, rs.ProjectID)
	f.stored = rs
	return nil
}
func (f *p622fakeRuntime) Get(ctx context.Context, projectID uuid.UUID) (*persistence.RuntimeStateRecord, error) {
	if f.stored == nil || f.stored.ProjectID != projectID {
		return nil, persistence.ErrNotFound
	}
	return f.stored, nil
}
func (f *p622fakeRuntime) UpdateWithVersion(context.Context, *persistence.RuntimeStateRecord, int) error {
	return nil
}
func (f *p622fakeRuntime) WithTx(persistence.Transaction) persistence.RuntimeStateRepository {
	return f
}

func p622machine() *persistence.MachineRecord {
	a, mid := uuid.New(), uuid.New()
	// A v1 row's family id IS its own id (the repository sets it that way
	// at creation).
	return &persistence.MachineRecord{ID: mid, FamilyID: mid,
		Version: 1, Enabled: true, InitialStateID: &a,
	}
}

// T6.30.1 (unit half) — Initialize resolves the machine's configured
// initial state and the machine version binding, and writes the runtime
// state row exactly once at version 1.
func TestP622InitializeUnitWritesInitialState(t *testing.T) {
	m := p622machine()
	r := &p622fakeRuntime{}
	svc := NewRuntimeStateService(r).WithMachines(&p622fakeMachines{m: m})
	out, err := svc.Initialize(context.Background(), m.ProjectID.String(), m.ID.String())
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if r.created != 1 {
		t.Fatalf("creates = %d, want 1", r.created)
	}
	if r.stored.CurrentStateID != *m.InitialStateID {
		t.Fatalf("current state = %s, want the machine's configured initial state %s",
			r.stored.CurrentStateID, *m.InitialStateID)
	}
	if r.stored.StateMachineID != m.ID || r.stored.StateMachineVersion != m.Version {
		t.Fatalf("binding = (%s, %d), want (%s, %d)",
			r.stored.StateMachineID, r.stored.StateMachineVersion, m.ID, m.Version)
	}
	if r.stored.Version != 1 {
		t.Fatalf("version = %d, want 1", r.stored.Version)
	}
	if out.CurrentStateID != r.stored.CurrentStateID.String() || out.Version != 1 {
		t.Fatalf("returned view = (%s, %d), want the stored row", out.CurrentStateID, out.Version)
	}
}

// T6.30.2 (unit half) — Machine version captured: the recorded binding is
// the machine row's version.
func TestP622InitializeUnitCapturesMachineVersion(t *testing.T) {
	m := p622machine()
	m.Version = 3 // a historical machine version
	r := &p622fakeRuntime{}
	svc := NewRuntimeStateService(r).WithMachines(&p622fakeMachines{m: m})
	if _, err := svc.Initialize(context.Background(), m.ProjectID.String(), m.ID.String()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if r.stored.StateMachineVersion != 3 {
		t.Fatalf("stored machine version = %d, want 3", r.stored.StateMachineVersion)
	}
}

// T6.30.3 (unit half) — Duplicate initialization is idempotent: with an
// existing runtime row the op returns the stored state and performs NO
// machine read and NO write (the chosen semantics, documented in
// ACTIVE_TASK.md notes: no-op when already initialized).
func TestP622InitializeUnitDuplicateIsNoop(t *testing.T) {
	m := p622machine()
	proj := uuid.New()
	existing := &persistence.RuntimeStateRecord{ID: uuid.New(), ProjectID: proj,
		CurrentStateID: *m.InitialStateID, Version: 7}
	r := &p622fakeRuntime{stored: existing}
	svc := NewRuntimeStateService(r).WithMachines(&p622fakeMachines{m: m})
	out, err := svc.Initialize(context.Background(), proj.String(), m.ID.String())
	if err != nil {
		t.Fatalf("duplicate initialize: %v", err)
	}
	if r.created != 0 {
		t.Fatalf("duplicate init wrote a row (created = %d), want a no-op", r.created)
	}
	if out.Version != 7 {
		t.Fatalf("duplicate init returned version = %d, want the stored 7", out.Version)
	}
}

// T6.30.4 (unit halves) — Invalid machine cannot initialize runtime: an
// unknown machine id is rejected with ErrNotFound and a machine with no
// configured initial state is rejected with ErrInvalidInput; in both
// cases no row is written.
func TestP622InitializeUnitInvalidMachine(t *testing.T) {
	// Unknown machine id.
	r := &p622fakeRuntime{}
	svc := NewRuntimeStateService(r).WithMachines(&p622fakeMachines{m: p622machine()})
	_, err := svc.Initialize(context.Background(), uuid.New().String(), uuid.New().String())
	if err != ErrNotFound {
		t.Fatalf("unknown machine err = %v, want ErrNotFound", err)
	}
	if r.created != 0 {
		t.Fatalf("unknown machine wrote a row, want none")
	}
	// Machine with no initial state.
	m := p622machine()
	m.InitialStateID = nil
	r2 := &p622fakeRuntime{}
	svc2 := NewRuntimeStateService(r2).WithMachines(&p622fakeMachines{m: m})
	_, err = svc2.Initialize(context.Background(), m.ProjectID.String(), m.ID.String())
	if err != ErrInvalidInput {
		t.Fatalf("no-initial-state err = %v, want ErrInvalidInput", err)
	}
	if r2.created != 0 {
		t.Fatalf("no-initial-state wrote a row, want none")
	}
	// Invalid project id.
	_, err = svc.Initialize(context.Background(), "not-a-uuid", m.ID.String())
	if err != ErrInvalidInput {
		t.Fatalf("bad project id err = %v, want ErrInvalidInput", err)
	}
	// Missing machine wiring: the op needs the machine surface.
	r3 := &p622fakeRuntime{}
	_, err = NewRuntimeStateService(r3).Initialize(context.Background(), m.ProjectID.String(), m.ID.String())
	if err != ErrInfrastructure {
		t.Fatalf("unwired machines err = %v, want ErrInfrastructure", err)
	}
}
