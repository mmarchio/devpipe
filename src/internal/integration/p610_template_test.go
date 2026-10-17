package integration

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
)

// P6-10 (WBS 6.7): the prompt-template reference — a prompt-template key on
// workflow_states (migration 00028), round-tripped through the repository,
// and surviving a service restart.

// T6.7.1 (DB half) — Migration 00028 applies cleanly and the
// prompt_template column exists on workflow_states.
func TestP610MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	// Bumped 27 -> 28 by P6-10 (migration 00028) — accepted
	// contract change, not a weakened test.
	if version != 126 {
		t.Fatalf("max migration version = %d, want 82", version)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name = 'workflow_states' AND column_name = 'prompt_template'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("workflow_states.prompt_template column = %d, want 1", n)
	}
}

// T6.7.1 — A prompt-template key persists and round-trips: both a set key
// and an empty one come back unchanged.
func TestP610TemplateRoundTrip(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projID := uuid.MustParse(p605Project(t, h))
	now := time.Now().UTC()
	machineRec := &persistence.MachineRecord{
		ID: uuid.New(), ProjectID: projID, Name: "m-tpl", Description: "",
		Version: 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := dbrepo.NewStateMachineRepository(pool).Create(ctx, machineRec); err != nil {
		t.Fatalf("create machine: %v", err)
	}
	states := dbrepo.NewWorkflowStateRepository(pool)
	rec := &persistence.WorkflowStateRecord{
		ID: uuid.New(), MachineID: machineRec.ID, Key: "PHASE_PLAN", Name: "Phase Plan",
		Kind: "ACTION", Enabled: true, PromptTemplate: "PLAN_PHASE",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := states.Create(ctx, rec); err != nil {
		t.Fatalf("create state: %v", err)
	}
	ctrl := &persistence.WorkflowStateRecord{
		ID: uuid.New(), MachineID: machineRec.ID, Key: "CONTINUE", Name: "Continue",
		Kind: "CONTROL", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := states.Create(ctx, ctrl); err != nil {
		t.Fatalf("create control state: %v", err)
	}
	got, err := states.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if got.PromptTemplate != "PLAN_PHASE" {
		t.Fatalf("template round trip = %q, want PLAN_PHASE", got.PromptTemplate)
	}
	got2, err := states.Get(ctx, ctrl.ID)
	if err != nil {
		t.Fatalf("get control state: %v", err)
	}
	if got2.PromptTemplate != "" {
		t.Fatalf("empty template round trip = %q, want empty", got2.PromptTemplate)
	}
	list, err := states.ListByMachine(ctx, machineRec.ID, machineRec.Version)
	if err != nil {
		t.Fatalf("list states: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("states = %d, want 2", len(list))
	}
}

// T6.7.4 — A state's template reference survives a service restart: two
// harness boots on the same persisted database, snapshot taken before the
// first boot closes and compared field-by-field after.
func TestP610TemplateSurvivesRestart(t *testing.T) {
	base := baseDSN()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Skipf("test PG unreachable (%v)", err)
	}
	dbName := fmt.Sprintf("overseer_e2e_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		c.Close(ctx)
		t.Skipf("create db: %v", err)
	}
	c.Close(ctx)
	t.Cleanup(func() { dropDB(t, base, dbName) })

	h1 := NewHarnessOn(t, base, dbName)
	p610SeedTemplateStates(t, h1)
	before := p610Snapshot(t, h1)
	// NewHarnessOn registers no t.Cleanup for the first instance: the
	// caller closes it so the drop in t.Cleanup can succeed.
	h1.Close()
	h2 := NewHarnessOn(t, base, dbName)
	t.Cleanup(h2.Close)
	after := p610Snapshot(t, h2)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("template snapshot after restart = %+v, want %+v", after, before)
	}
}

type p610Snap struct {
	Keys  []string
	Kinds []string
	Tmpls []string
	Term  []bool
	Enab  []bool
	Descs []string
}

func p610Snapshot(t *testing.T, h *Harness) p610Snap {
	t.Helper()
	ctx := context.Background()
	var machineID string
	if err := h.DBScan(ctx, "SELECT id::text FROM state_machines ORDER BY version LIMIT 1", &machineID); err != nil {
		t.Fatalf("machine id: %v", err)
	}
	states, err := dbrepo.NewWorkflowStateRepository(h.App.DB().Raw()).ListByMachine(ctx, p610UUID(t, machineID), 1)
	if err != nil {
		t.Fatalf("list states: %v", err)
	}
	out := p610Snap{}
	for _, s := range states {
		out.Keys = append(out.Keys, s.Key)
		out.Kinds = append(out.Kinds, s.Kind)
		out.Tmpls = append(out.Tmpls, s.PromptTemplate)
		out.Term = append(out.Term, s.Terminal)
		out.Enab = append(out.Enab, s.Enabled)
		out.Descs = append(out.Descs, s.Description)
	}
	return out
}

func p610UUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", s, err)
	}
	return u
}

// p610SeedTemplateStates creates a machine plus one action state with a
// template and one control state without one (via repositories on the live
// harness pool) so the restart snapshot has something to preserve.
func p610SeedTemplateStates(t *testing.T, h *Harness) {
	t.Helper()
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projectID := p605Project(t, h)
	now := time.Now().UTC()
	machineRec := &persistence.MachineRecord{
		ID: uuid.New(), ProjectID: p610UUID(t, projectID), Name: "m-tpl", Description: "",
		Version: 1, Enabled: true, CreatedAt: now, UpdatedAt: now,
	}
	if err := dbrepo.NewStateMachineRepository(pool).Create(ctx, machineRec); err != nil {
		t.Fatalf("create machine: %v", err)
	}
	states := dbrepo.NewWorkflowStateRepository(pool)
	for _, rec := range []*persistence.WorkflowStateRecord{
		{ID: uuid.New(), MachineID: machineRec.ID, Key: "PHASE_PLAN", Name: "Phase Plan",
			Kind: "ACTION", Enabled: true, PromptTemplate: "PLAN_PHASE", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), MachineID: machineRec.ID, Key: "CONTINUE", Name: "Continue",
			Kind: "CONTROL", Enabled: true, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), MachineID: machineRec.ID, Key: "PROJECT_DONE", Name: "Project Done",
			Kind: "TERMINAL", Terminal: true, Enabled: true, CreatedAt: now, UpdatedAt: now},
	} {
		if err := states.Create(ctx, rec); err != nil {
			t.Fatalf("create state %s: %v", rec.Key, err)
		}
	}
}
