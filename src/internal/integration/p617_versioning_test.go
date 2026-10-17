package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

// P6-17 (WBS 6.20): state-machine versioning. Edits create new versions
// (v1, v2, ...); every historical version remains retrievable with its
// states/transitions reconstructable; an active execution's binding
// (machine + version) is never mutated by later edits.

// p617Service wires the versioning service over the harness pool.
func p617Service(h *Harness) *service.StateMachineVersioningService {
	pool := h.App.DB().Raw()
	return service.NewStateMachineVersioningService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
	).WithTxPool(pool)
}

// p617SeedMachine builds a machine (v1) with the given states, transitions
// and initial-state key through the repositories and returns the machine ID.
func p617SeedMachine(t *testing.T, h *Harness, projectID string, name string,
	states []service.MachineStateDef, transitions []service.MachineTransitionDef,
	initialKey string,
) string {
	t.Helper()
	ctx := context.Background()
	pool := h.App.DB().Raw()
	machines := dbrepo.NewStateMachineRepository(pool)
	wstates := dbrepo.NewWorkflowStateRepository(pool)
	wtrans := dbrepo.NewStateTransitionRepository(pool)
	projID := uuid.MustParse(projectID)
	now := time.Now().UTC()
	mid := uuid.New()
	ids := map[string]uuid.UUID{}
	for _, s := range states {
		ids[s.Key] = uuid.New()
	}
	rec := &persistence.MachineRecord{
		ID: mid, ProjectID: projID, FamilyID: mid, Name: name, Version: 1, Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := machines.Create(ctx, rec); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	for _, s := range states {
		if err := wstates.Create(ctx, &persistence.WorkflowStateRecord{
			ID: ids[s.Key], MachineID: mid, Version: 1, Key: s.Key, Name: s.Key,
			Kind: s.Kind, Enabled: true, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed state %s: %v", s.Key, err)
		}
	}
	if err := machines.SetInitialState(ctx, mid, ids[initialKey]); err != nil {
		t.Fatalf("seed initial state: %v", err)
	}
	for _, tr := range transitions {
		if err := wtrans.Create(ctx, &persistence.StateTransitionRecord{
			ID: uuid.New(), MachineID: mid, Version: 1, FromStateID: ids[tr.From],
			ToStateID: ids[tr.To], Outcome: tr.Outcome, Enabled: true,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed transition: %v", err)
		}
	}
	return mid.String()
}

// T6.20.1 — A freshly created machine is version 1.
func TestP617InitialVersionIsOne(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-v1",
		[]service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"},
		},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	machines := dbrepo.NewStateMachineRepository(h.App.DB().Raw())
	got, err := machines.Get(ctx, uuid.MustParse(mid))
	if err != nil {
		t.Fatalf("get machine: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("initial machine version = %d, want 1", got.Version)
	}
}

// T6.20.2 — An edit creates version 2; the original v1 machine row keeps
// version 1 (it is the historical version's anchor).
func TestP617EditCreatesVersionTwo(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-edit",
		[]service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"},
		},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	svc := p617Service(h)
	newID, err := svc.Edit(ctx, mid, &service.MachineEditInput{
		InitialStateKey: "A",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "C", Kind: "ACTION"},
			{Key: "B", Kind: "TERMINAL"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "C"}, {From: "C", Outcome: "GO", To: "B"},
		},
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	machines := dbrepo.NewStateMachineRepository(h.App.DB().Raw())
	old, err := machines.Get(ctx, uuid.MustParse(mid))
	if err != nil {
		t.Fatalf("get old machine: %v", err)
	}
	if old.Version != 1 {
		t.Fatalf("pre-edit machine version = %d, want 1 (v1 anchor untouched)", old.Version)
	}
	nu, err := machines.Get(ctx, uuid.MustParse(newID))
	if err != nil {
		t.Fatalf("get new machine: %v", err)
	}
	if nu.Version != 2 {
		t.Fatalf("post-edit machine version = %d, want 2", nu.Version)
	}
}

// T6.20.3 — After an edit, v1 remains retrievable at its original version
// number (unknown versions yield ErrNotFound).
func TestP617VersionOneRetrievable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-retr",
		[]service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"},
		},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	svc := p617Service(h)
	if _, err := svc.Edit(ctx, mid, &service.MachineEditInput{
		InitialStateKey: "A",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"},
			{Key: "C", Kind: "ACTION"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "B"}, {From: "A", Outcome: "NOW", To: "C"},
		},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	machines := dbrepo.NewStateMachineRepository(h.App.DB().Raw())
	v1, err := machines.GetVersion(ctx, uuid.MustParse(mid), 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if v1.Version != 1 || v1.ID.String() != mid {
		t.Fatalf("v1 = version %d id %s, want version 1 id %s", v1.Version, v1.ID, mid)
	}
	_, err = machines.GetVersion(ctx, uuid.MustParse(mid), 3)
	if !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("unknown version err = %v, want ErrNotFound", err)
	}
}

// T6.20.4 — Historical states/transitions of any version remain
// reconstructable: a read for a given version returns exactly that
// version's states and transitions.
func TestP617HistoricalReconstructable(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-hist",
		[]service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"},
		},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	svc := p617Service(h)
	if _, err := svc.Edit(ctx, mid, &service.MachineEditInput{
		InitialStateKey: "A",
		States: []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "C", Kind: "ACTION"},
			{Key: "B", Kind: "TERMINAL"},
		},
		Transitions: []service.MachineTransitionDef{
			{From: "A", Outcome: "GO", To: "C"}, {From: "C", Outcome: "GO", To: "B"},
		},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	pool := h.App.DB().Raw()
	machines := dbrepo.NewStateMachineRepository(pool)
	wstates := dbrepo.NewWorkflowStateRepository(pool)
	wtrans := dbrepo.NewStateTransitionRepository(pool)
	for _, want := range []struct {
		version  int
		states   int
		trans    int
		initialK string
	}{
		{1, 2, 1, "A"},
		{2, 3, 2, "A"},
	} {
		m, err := machines.GetVersion(ctx, uuid.MustParse(mid), want.version)
		if err != nil {
			t.Fatalf("get v%d: %v", want.version, err)
		}
		ss, err := wstates.ListByMachine(ctx, m.ID, m.Version)
		if err != nil {
			t.Fatalf("states v%d: %v", want.version, err)
		}
		if len(ss) != want.states {
			t.Fatalf("v%d states = %d, want %d", want.version, len(ss), want.states)
		}
		ts, err := wtrans.ListByMachine(ctx, m.ID, m.Version)
		if err != nil {
			t.Fatalf("transitions v%d: %v", want.version, err)
		}
		if len(ts) != want.trans {
			t.Fatalf("v%d transitions = %d, want %d", want.version, len(ts), want.trans)
		}
		if m.InitialStateID == nil {
			t.Fatalf("v%d has no initial state", want.version)
		}
		ini, err := wstates.Get(ctx, *m.InitialStateID)
		if err != nil {
			t.Fatalf("v%d initial state: %v", want.version, err)
		}
		if ini.Key != want.initialK {
			t.Fatalf("v%d initial = %s, want %s", want.version, ini.Key, want.initialK)
		}
	}
}

// T6.20.5 — An active execution bound to (machine, version) is not mutated
// by edits that create newer versions: the binding still resolves to the
// exact same machine row and the same state set.
func TestP617ActiveBindingNotMutated(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-bind",
		[]service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"},
		},
		[]service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}},
		"A",
	)
	// The execution's binding: (machine, version) as captured at start.
	boundMachineID := mid
	boundVersion := 1
	pool := h.App.DB().Raw()
	machines := dbrepo.NewStateMachineRepository(pool)
	wstates := dbrepo.NewWorkflowStateRepository(pool)
	before, err := machines.GetVersion(ctx, uuid.MustParse(boundMachineID), boundVersion)
	if err != nil {
		t.Fatalf("capture binding: %v", err)
	}
	beforeStates, err := wstates.ListByMachine(ctx, before.ID, before.Version)
	if err != nil {
		t.Fatalf("capture binding states: %v", err)
	}
	// Two later edits create v3 (and v4).
	svc := p617Service(h)
	for _, extra := range []string{"C", "D"} {
		defs := []service.MachineStateDef{
			{Key: "A", Kind: "CONTROL"}, {Key: "B", Kind: "TERMINAL"},
		}
		trs := []service.MachineTransitionDef{{From: "A", Outcome: "GO", To: "B"}}
		for _, e := range []string{extra} {
			defs = append(defs, service.MachineStateDef{Key: e, Kind: "ACTION"})
			trs = append(trs, service.MachineTransitionDef{From: "A", Outcome: "X", To: e})
		}
		if _, err := svc.Edit(ctx, boundMachineID, &service.MachineEditInput{
			InitialStateKey: "A", States: defs, Transitions: trs,
		}); err != nil {
			t.Fatalf("edit: %v", err)
		}
	}
	// The binding resolves to the identical machine and state set.
	after, err := machines.GetVersion(ctx, uuid.MustParse(boundMachineID), boundVersion)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	if after.ID != before.ID {
		t.Fatalf("binding machine id changed: %s -> %s", before.ID, after.ID)
	}
	afterStates, err := wstates.ListByMachine(ctx, after.ID, after.Version)
	if err != nil {
		t.Fatalf("resolve binding states: %v", err)
	}
	if len(afterStates) != len(beforeStates) {
		t.Fatalf("binding state count = %d, want %d", len(afterStates), len(beforeStates))
	}
	for i := range beforeStates {
		if afterStates[i].ID != beforeStates[i].ID || afterStates[i].Key != beforeStates[i].Key {
			t.Fatalf("binding state %d changed: %s -> %s", i, beforeStates[i].ID, afterStates[i].ID)
		}
	}
	// The machine family's current version has advanced.
	vers, err := machines.ListVersions(ctx, uuid.MustParse(boundMachineID))
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(vers) != 3 || vers[2].Version != 3 {
		t.Fatalf("versions = %v, want [1 2 3]", vers)
	}
}

// Edit rejects an empty state set (the new version must be non-empty).
func TestP617EditEmptyRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	proj := p605Project(t, h)
	mid := p617SeedMachine(t, h, proj, "m-empty",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}}, nil, "A")
	svc := p617Service(h)
	_, err := svc.Edit(ctx, mid, &service.MachineEditInput{InitialStateKey: "A"})
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("empty edit err = %v, want ErrInvalidInput", err)
	}
	// The machine is untouched: still exactly one version.
	machines := dbrepo.NewStateMachineRepository(h.App.DB().Raw())
	vers, err := machines.ListVersions(ctx, uuid.MustParse(mid))
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if len(vers) != 1 {
		t.Fatalf("versions after rejected edit = %d, want 1", len(vers))
	}
}
