// P6-27 (WBS 6.37) unit half: the transition-configuration service over
// in-memory fakes of the persistence interfaces. DB-level and REST-level
// proof is the integration half (p627 integration tests over the harness).
package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/state"
)

// --- fakes (local to this test file; p627-prefixed per flat-package rule) ---

type p627Machines struct{ rec *persistence.MachineRecord }

func (f *p627Machines) Create(context.Context, *persistence.MachineRecord) error { return nil }
func (f *p627Machines) Get(ctx context.Context, id uuid.UUID) (*persistence.MachineRecord, error) {
	if f.rec != nil && f.rec.ID == id {
		return f.rec, nil
	}
	return nil, persistence.ErrNotFound
}
func (f *p627Machines) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.MachineRecord, error) {
	if f.rec == nil || f.rec.ProjectID != projectID {
		return nil, nil
	}
	return []*persistence.MachineRecord{f.rec}, nil
}
func (f *p627Machines) SetInitialState(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *p627Machines) GetVersion(ctx context.Context, id uuid.UUID, version int) (*persistence.MachineRecord, error) {
	return nil, persistence.ErrNotFound
}
func (f *p627Machines) ListVersions(ctx context.Context, id uuid.UUID) ([]*persistence.MachineRecord, error) {
	return nil, nil
}
func (f *p627Machines) WithTx(persistence.Transaction) persistence.StateMachineRepository { return f }

type p627States struct {
	ss []*persistence.WorkflowStateRecord
}

func (f *p627States) Create(context.Context, *persistence.WorkflowStateRecord) error { return nil }
func (f *p627States) Get(ctx context.Context, id uuid.UUID) (*persistence.WorkflowStateRecord, error) {
	for _, s := range f.ss {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (f *p627States) ListByMachine(context.Context, uuid.UUID, int) ([]*persistence.WorkflowStateRecord, error) {
	return f.ss, nil
}
func (f *p627States) Update(ctx context.Context, rec *persistence.WorkflowStateRecord) (*persistence.WorkflowStateRecord, error) {
	return rec, nil
}
func (f *p627States) Delete(context.Context, uuid.UUID) error                            { return nil }
func (f *p627States) WithTx(persistence.Transaction) persistence.WorkflowStateRepository { return f }

type p627Trans struct {
	ts    []*persistence.StateTransitionRecord
	upds  int
	upErr error
}

func (f *p627Trans) Create(ctx context.Context, rec *persistence.StateTransitionRecord) error {
	f.ts = append(f.ts, rec)
	return nil
}
func (f *p627Trans) Get(ctx context.Context, id uuid.UUID) (*persistence.StateTransitionRecord, error) {
	for _, t := range f.ts {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (f *p627Trans) ListByMachine(context.Context, uuid.UUID, int) ([]*persistence.StateTransitionRecord, error) {
	return f.ts, nil
}
func (f *p627Trans) Update(ctx context.Context, rec *persistence.StateTransitionRecord) (*persistence.StateTransitionRecord, error) {
	f.upds++
	if f.upErr != nil {
		return nil, f.upErr
	}
	for i, t := range f.ts {
		if t.ID == rec.ID {
			f.ts[i] = rec
			return rec, nil
		}
	}
	return nil, persistence.ErrNotFound
}
func (f *p627Trans) ReferencedByStates(context.Context, uuid.UUID, int) (map[uuid.UUID]bool, error) {
	return map[uuid.UUID]bool{}, nil
}
func (f *p627Trans) WithTx(persistence.Transaction) persistence.StateTransitionRepository { return f }

type p627Conditions struct {
	conds  map[uuid.UUID]*persistence.TransitionCondition
	stored int
	delErr error
}

func (f *p627Conditions) Create(ctx context.Context, id uuid.UUID, c *persistence.TransitionCondition) error {
	f.stored++
	if f.conds == nil {
		f.conds = map[uuid.UUID]*persistence.TransitionCondition{}
	}
	f.conds[id] = c
	return nil
}
func (f *p627Conditions) Get(ctx context.Context, id uuid.UUID) (*persistence.TransitionCondition, bool, error) {
	if f.conds == nil {
		return nil, false, nil
	}
	c, ok := f.conds[id]
	return c, ok, nil
}
func (f *p627Conditions) Remove(ctx context.Context, id uuid.UUID) error {
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.conds, id)
	return nil
}
func (f *p627Conditions) WithTx(persistence.Transaction) persistence.TransitionConditionRepository {
	return f
}

// p627Fixture wires the service to fakes with a v1 machine and states A/B.
func p627Fixture() (*TransitionConfigService, *p627Machines, *p627Trans, *p627Conditions, string) {
	proj := uuid.New()
	now := time.Now().UTC()
	mid := uuid.New()
	var initialID uuid.UUID
	machines := &p627Machines{rec: &persistence.MachineRecord{
		ID: mid, ProjectID: proj, FamilyID: mid, Name: "m", Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}}
	ss := &p627States{ss: []*persistence.WorkflowStateRecord{
		{ID: uuid.New(), MachineID: mid, Version: 1, Key: "A", Kind: "CONTROL", Enabled: true},
		{ID: uuid.New(), MachineID: mid, Version: 1, Key: "B", Kind: "TERMINAL", Enabled: true},
	}}
	initialID = ss.ss[0].ID
	machines.rec.InitialStateID = &initialID
	trans := &p627Trans{}
	conds := &p627Conditions{}
	svc := NewTransitionConfigService(machines, ss, trans, conds)
	return svc, machines, trans, conds, proj.String()
}

// T6.37.1 — Transition created: the create op stores the row in the
// current version and returns the view (addressed by state keys).
func TestP627CreateTransition(t *testing.T) {
	svc, _, trans, _, proj := p627Fixture()
	view, err := svc.CreateTransition(context.Background(), proj, &CreateTransitionInput{
		FromKey: "a", ToKey: "b", Outcome: "go",
	})
	if err != nil {
		t.Fatalf("CreateTransition: %v", err)
	}
	if view.From != "A" || view.To != "B" {
		t.Fatalf("view = %+v, want keys A/B", view)
	}
	if view.Priority != 0 || view.Enabled != true {
		t.Fatalf("defaults = %v/%v, want 0/true", view.Priority, view.Enabled)
	}
	if len(trans.ts) != 1 {
		t.Fatalf("stored transitions = %d, want 1", len(trans.ts))
	}
	if trans.ts[0].Version != 1 {
		t.Fatalf("stored version = %d, want the current 1", trans.ts[0].Version)
	}
}

// T6.37.2 — Condition persisted: a created transition with a condition
// stores it through the condition repository (not just in the response),
// and ConfigureCondition persists an update (and a clear).
func TestP627ConditionPersisted(t *testing.T) {
	svc, _, trans, conds, proj := p627Fixture()
	view, err := svc.CreateTransition(context.Background(), proj, &CreateTransitionInput{
		FromKey: "A", ToKey: "B", Outcome: "done",
		Condition: &TransitionConditionInput{
			Field: "context_usage", Operator: stateOp(">="), Literal: float64(0.8),
		},
	})
	if err != nil {
		t.Fatalf("CreateTransition with condition: %v", err)
	}
	if view.Condition == nil || view.Condition.Field != "context_usage" {
		t.Fatalf("view condition = %+v, want the persisted one", view.Condition)
	}
	// PERSISTED: the repository holds it (the T6.37.2 load-bearing half).
	stored, ok := conds.conds[trans.ts[len(trans.ts)-1].ID]
	if !ok {
		t.Fatalf("condition repository = %v, want a stored condition", conds.conds)
	}
	if stored.Field != "context_usage" || stored.Operator != ">=" {
		t.Fatalf("stored condition = %+v, want field/operator round-trip", stored)
	}
	// ConfigureCondition updates the persisted row.
	if _, err := svc.ConfigureCondition(context.Background(), proj, trans.ts[len(trans.ts)-1].ID.String(), &TransitionConditionInput{
		Field: "remaining_work_units", Operator: stateOp("=="), Literal: float64(0),
	}); err != nil {
		t.Fatalf("ConfigureCondition: %v", err)
	}
	stored2 := conds.conds[trans.ts[len(trans.ts)-1].ID]
	if stored2.Field != "remaining_work_units" || stored2.Operator != "==" {
		t.Fatalf("stored condition after configure = %+v, want the updated one", stored2)
	}
	// A nil input clears it.
	if _, err := svc.ConfigureCondition(context.Background(), proj, trans.ts[len(trans.ts)-1].ID.String(), nil); err != nil {
		t.Fatalf("clear condition: %v", err)
	}
	if _, ok := conds.conds[trans.ts[len(trans.ts)-1].ID]; ok {
		t.Fatal("condition must be cleared after a nil configure")
	}
}

// T6.37.5 (guard) — a disabled candidate can never be ambiguous: a
// disabled create into an equal-priority same-outcome pair is accepted
// (the detector skips disabled rows).
func TestP627AmbiguitySkipsDisabledCandidate(t *testing.T) {
	svc, _, trans, _, proj := p627Fixture()
	if _, err := svc.CreateTransition(context.Background(), proj, &CreateTransitionInput{
		FromKey: "A", ToKey: "B", Outcome: "go", Priority: 5,
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	false := false
	if _, err := svc.CreateTransition(context.Background(), proj, &CreateTransitionInput{
		FromKey: "A", ToKey: "B", Outcome: " GO ", Priority: 5, Enabled: &false,
	}); err != nil {
		t.Fatalf("disabled duplicate create = %v, want accepted", err)
	}
	if len(trans.ts) != 2 {
		t.Fatalf("stored = %d, want 2", len(trans.ts))
	}
}

// stateOp converts a string to the domain operator type (test helper).
func stateOp(o string) state.ConditionOperator { return state.ConditionOperator(o) }

// T6.37.3 — Priority persisted: the configure-priority op stores the value.
func TestP627PriorityPersisted(t *testing.T) {
	svc, _, trans, _, proj := p627Fixture()
	view, err := svc.CreateTransition(context.Background(), proj, &CreateTransitionInput{
		FromKey: "A", ToKey: "B", Outcome: "go",
	})
	if err != nil {
		t.Fatalf("CreateTransition: %v", err)
	}
	if _, err := svc.ConfigurePriority(context.Background(), proj, view.ID, 10); err != nil {
		t.Fatalf("ConfigurePriority: %v", err)
	}
	if trans.ts[0].Priority != 10 {
		t.Fatalf("stored priority = %d, want 10", trans.ts[0].Priority)
	}
	if trans.upds != 1 {
		t.Fatalf("updates = %d, want 1 (in-place, within the current version)", trans.upds)
	}
}

// T6.37.4 — Invalid state reference rejected: an unknown from/to state key
// is ErrNotFound before any write.
func TestP627InvalidStateReference(t *testing.T) {
	svc, _, trans, _, proj := p627Fixture()
	if _, err := svc.CreateTransition(context.Background(), proj, &CreateTransitionInput{
		FromKey: "A", ToKey: "NOPE", Outcome: "go",
	}); err == nil || !errorsIsNotFound(err) {
		t.Fatalf("unknown to-state err = %v, want ErrNotFound", err)
	}
	if _, err := svc.CreateTransition(context.Background(), proj, &CreateTransitionInput{
		FromKey: "GHOST", ToKey: "B", Outcome: "go",
	}); err == nil || !errorsIsNotFound(err) {
		t.Fatalf("unknown from-state err = %v, want ErrNotFound", err)
	}
	if len(trans.ts) != 0 {
		t.Fatalf("no transition may be stored: %d", len(trans.ts))
	}
}

// T6.37.5 — Ambiguous transition configuration rejected: a same-source,
// same-outcome, equal-priority create/update is a conflict (the P6-14
// detector) BEFORE any write; a distinct priority is accepted.
func TestP627AmbiguityRejected(t *testing.T) {
	svc, _, trans, _, proj := p627Fixture()
	if _, err := svc.CreateTransition(context.Background(), proj, &CreateTransitionInput{
		FromKey: "A", ToKey: "B", Outcome: "go", Priority: 10,
	}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Same from + same outcome (case-insensitive) + equal priority → 409.
	if _, err := svc.CreateTransition(context.Background(), proj, &CreateTransitionInput{
		FromKey: "a", ToKey: "B", Outcome: " GO ", Priority: 10,
	}); err == nil || !errorsIsConflict(err) {
		t.Fatalf("ambiguous create err = %v, want ErrConflict", err)
	}
	if len(trans.ts) != 1 {
		t.Fatalf("the ambiguous row must not be stored: %d rows", len(trans.ts))
	}
	// A distinct priority is legal (no ambiguity).
	if _, err := svc.CreateTransition(context.Background(), proj, &CreateTransitionInput{
		FromKey: "A", ToKey: "B", Outcome: "go", Priority: 20,
	}); err != nil {
		t.Fatalf("distinct-priority create: %v", err)
	}
	// An UPDATE into an ambiguity is rejected too.
	first, second := trans.ts[0], trans.ts[1]
	if _, err := svc.UpdateTransition(context.Background(), proj, second.ID.String(), &UpdateTransitionInput{
		Priority: intPtr(10),
	}); err == nil || !errorsIsConflict(err) {
		t.Fatalf("ambiguous update err = %v, want ErrConflict", err)
	}
	if trans.upds != 0 {
		t.Fatalf("the rejected update must not write: %d updates", trans.upds)
	}
	_ = first
}

func intPtr(i int) *int { return &i }

func errorsIsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
func errorsIsConflict(err error) bool { return errors.Is(err, ErrConflict) }
