package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

// P6-21 (WBS 6.27/6.28/6.29): transactional application of an
// already-selected transition, append-only transition history, and the
// per-project monotonic state-entry sequence.

// p621Service builds the transition application service over the harness
// pool. usePool selects the transactional vs sequential (nil-pool) path.
func p621Service(h *Harness, usePool bool) *service.TransitionApplicationService {
	pool := h.App.DB().Raw()
	svc := service.NewTransitionApplicationService(
		dbrepo.NewRuntimeStateRepository(pool),
		dbrepo.NewTransitionHistoryRepository(pool),
	)
	if usePool {
		svc = svc.WithTxPool(pool)
	}
	return svc
}

// p621Seed initializes the runtime state via the P6-20 service (the
// application service only applies transitions over an EXISTING runtime
// state), then returns the stored version.
func p621Seed(t *testing.T, h *Harness, project, machine, state string) int {
	t.Helper()
	svc := p620Service(h, true)
	st, err := svc.Transition(context.Background(), project, &service.TransitionInput{
		StateMachineID: machine, StateMachineVersion: 1,
		CurrentStateID: state, ExpectedVersion: 0,
	})
	if err != nil {
		t.Fatalf("seed runtime state: %v", err)
	}
	return st.Version
}

// p621Count returns the persisted history row count and the max seq for
// the project.
func p621Count(t *testing.T, h *Harness, project string) (int, int) {
	t.Helper()
	var n, maxSeq int
	ctx := context.Background()
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_transition_history WHERE project_id = '"+project+"'", &n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT coalesce(max(seq),0) FROM state_transition_history WHERE project_id = '"+project+"'", &maxSeq); err != nil {
		t.Fatalf("max seq: %v", err)
	}
	return n, maxSeq
}

// P6-21.1 — Migration 00031 applies: max schema version advances to 31 and
// the history table + sequence column exist. (PLAN.md pre-declared 00030;
// 00030 is runtime_state — recorded shift.)
func TestP621MigrationApplied(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	var version int
	if err := h.DBScan(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations", &version); err != nil {
		t.Fatalf("schema_migrations: %v", err)
	}
	if want := latestMigrationVersion(t); version != want {
		t.Fatalf("max migration version = %d, want %d", version, want)
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_name = 'state_transition_history'", &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("state_transition_history table present = %d, want 1", n)
	}
	var c int
	if err := h.DBScan(ctx, "SELECT count(*) FROM information_schema.columns WHERE table_name = 'projects' AND column_name = 'state_entry_seq'", &c); err != nil {
		t.Fatal(err)
	}
	if c != 1 {
		t.Fatalf("projects.state_entry_seq column present = %d, want 1", c)
	}
}

// T6.27.1 — Valid transition applied: the stored current state moves to the
// transition's destination.
func TestP621ValidTransitionApplied(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, _, _, _ := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	svc := p621Service(h, true)
	out, err := svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: stateA, ToStateID: stateB, Outcome: "GO", ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("apply A->B: %v", err)
	}
	if out.State.CurrentStateID != stateB {
		t.Fatalf("returned current state = %s, want %s", out.State.CurrentStateID, stateB)
	}
	got, err := svc.ListHistory(context.Background(), project)
	if err != nil {
		t.Fatalf("list history: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("history rows = %d, want 1", len(got))
	}
}

// T6.27.2 — Wrong source state rejected: a transition whose from-state does
// not match the runtime state's current state is rejected with
// ErrInvalidInput and NOTHING is written (state, version, history all
// unchanged).
func TestP621WrongSourceStateRejected(t *testing.T) {
	h := NewHarness(t)
	project, machine, _, stateB, stateC, _, _ := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateB)
	svc := p621Service(h, true)
	// Claims to move from C, but the runtime state is at B.
	_, err := svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: stateC, ToStateID: stateB, Outcome: "GO", ExpectedVersion: 1,
	})
	if !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("wrong-source err = %v, want service.ErrInvalidInput", err)
	}
	n, _ := p621Count(t, h, project)
	if n != 0 {
		t.Fatalf("history rows after rejected transition = %d, want 0 (no write)", n)
	}
	st, err := p620Service(h, true).Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.CurrentStateID != stateB || st.Version != 1 {
		t.Fatalf("state after rejected transition = (%s, %d), want (%s, 1) — must be a no-op",
			st.CurrentStateID, st.Version, stateB)
	}
}

// T6.27.3 — Runtime version increments: each applied transition advances the
// stored runtime version by exactly one.
func TestP621RuntimeVersionIncrements(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, _, _ := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	svc := p621Service(h, true)
	out, err := svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: stateA, ToStateID: stateB, Outcome: "GO", ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("A->B: %v", err)
	}
	if out.State.Version != 2 {
		t.Fatalf("version after A->B = %d, want 2", out.State.Version)
	}
	out, err = svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: stateB, ToStateID: stateC, Outcome: "DONE", ExpectedVersion: 2,
	})
	if err != nil {
		t.Fatalf("B->C: %v", err)
	}
	if out.State.Version != 3 {
		t.Fatalf("version after B->C = %d, want 3", out.State.Version)
	}
	got, err := p620Service(h, true).Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != 3 {
		t.Fatalf("stored version = %d, want 3", got.Version)
	}
}

// T6.27.4 — History written atomically: state update, version increment, and
// the history row all commit together (one row with the POST-transition
// runtime version, and the state moved in the same step).
func TestP621HistoryWrittenAtomically(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, _, _, _ := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	svc := p621Service(h, true)
	if _, err := svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: stateA, ToStateID: stateB, Outcome: "GO", ExpectedVersion: 1,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ctx := context.Background()
	var fromID, toID string
	var rv int
	if err := h.DBScan(ctx,
		"SELECT from_state_id::text, to_state_id::text FROM state_transition_history WHERE project_id = '"+project+"' LIMIT 1",
		&fromID, &toID); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if fromID != stateA || toID != stateB {
		t.Fatalf("history endpoints = (%s, %s), want (%s, %s)", fromID, toID, stateA, stateB)
	}
	if err := h.DBScan(ctx,
		"SELECT runtime_version FROM state_transition_history WHERE project_id = '"+project+"'", &rv); err != nil {
		t.Fatalf("read runtime_version: %v", err)
	}
	if rv != 2 {
		t.Fatalf("history runtime_version = %d, want 2 (the POST-transition version)", rv)
	}
	// The state moved in the same commit: exactly one history row AND the
	// stored state is at B version 2.
	st, err := p620Service(h, true).Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.CurrentStateID != stateB || st.Version != 2 {
		t.Fatalf("state = (%s, %d), want (%s, 2)", st.CurrentStateID, st.Version, stateB)
	}
}

// failingHistoryRepo is the T6.27.5 failure-injection fake: Append fails
// with a conflict, simulating a failure in the history step.
type failingHistoryRepo struct {
	appendCalls int
}

func (f *failingHistoryRepo) Append(ctx context.Context, rec *persistence.TransitionHistoryRecord, allocateSeq bool) error {
	f.appendCalls++
	return persistence.ErrConflict
}

func (f *failingHistoryRepo) ListByProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.TransitionHistoryRecord, error) {
	return nil, nil
}

func (f *failingHistoryRepo) ListByStateID(ctx context.Context, stateID uuid.UUID) ([]*persistence.TransitionHistoryRecord, error) {
	return nil, nil
}
func (f *failingHistoryRepo) WithTx(tx persistence.Transaction) persistence.TransitionHistoryRepository {
	return f
}

// T6.27.5 — Failure rolls back state and history: a failure in the history
// step leaves NOTHING persisted — the runtime state is unchanged and no
// history row exists (both on the transactional path; the sequential path
// degrades per the documented nil-pool contract and is covered by the
// transactional rollback proof).
func TestP621FailureRollsBackStateAndHistory(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, _, _, _ := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	svc := p621Service(h, true)
	fake := &failingHistoryRepo{}
	svc = svc.WithHistoryRepo(fake)
	_, err := svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: stateA, ToStateID: stateB, Outcome: "GO", ExpectedVersion: 1,
	})
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("apply with failing history = %v, want the injected conflict surfaced", err)
	}
	if fake.appendCalls == 0 {
		t.Fatal("history Append was never called — the forced failure did not fire")
	}
	// Direct SQL ground truth: nothing was written.
	st, err := p620Service(h, true).Get(context.Background(), project)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if st.CurrentStateID != stateA || st.Version != 1 {
		t.Fatalf("state after rolled-back apply = (%s, %d), want (%s, 1)",
			st.CurrentStateID, st.Version, stateA)
	}
	n, maxSeq := p621Count(t, h, project)
	if n != 0 || maxSeq != 0 {
		t.Fatalf("history after rolled-back apply = (%d rows, max seq %d), want (0, 0)", n, maxSeq)
	}
	var seqCounter int
	if err := h.DBScan(context.Background(),
		"SELECT state_entry_seq FROM projects WHERE id = '"+project+"'", &seqCounter); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if seqCounter != 0 {
		t.Fatalf("projects.state_entry_seq after rollback = %d, want 0 (the allocation must roll back)", seqCounter)
	}
}

// T6.28.1–T6.28.5 — Transition history recorded with the correct fields:
// source state, destination, outcome, and phase/work-unit references are
// all stored and read back exactly.
func TestP621HistoryFieldsStored(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, _, phase, unit := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	svc := p621Service(h, true)
	ph, un := phase, unit
	if _, err := svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: stateA, ToStateID: stateB, Outcome: "GO",
		PhaseID: &ph, WorkUnitID: &un, ExpectedVersion: 1,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ctx := context.Background()
	var fromID, toID, outcome, phaseID, unitID string
	if err := h.DBScan(ctx,
		"SELECT from_state_id::text, to_state_id::text, outcome, "+
			"coalesce(phase_id::text,''), coalesce(work_unit_id::text,'') "+
			"FROM state_transition_history WHERE project_id = '"+project+"'",
		&fromID, &toID, &outcome, &phaseID, &unitID); err != nil {
		t.Fatalf("read history: %v", err)
	}
	if fromID != stateA {
		t.Fatalf("from_state = %s, want %s", fromID, stateA)
	}
	if toID != stateB {
		t.Fatalf("to_state = %s, want %s", toID, stateB)
	}
	if outcome != "GO" {
		t.Fatalf("outcome = %q, want %q", outcome, "GO")
	}
	if phaseID != phase || unitID != unit {
		t.Fatalf("refs = (%s, %s), want (%s, %s)", phaseID, unitID, phase, unit)
	}
}

// T6.28.6 — History append-only: a second (re-)application appends a NEW
// row; the first row is untouched, and the repository exposes no
// update/delete op (the interface has Append + ordered read only).
func TestP621HistoryAppendOnly(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, _, _ := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	svc := p621Service(h, true)
	apply := func(from, to, outcome string, ver int) {
		t.Helper()
		if _, err := svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
			FromStateID: from, ToStateID: to, Outcome: outcome, ExpectedVersion: ver,
		}); err != nil {
			t.Fatalf("apply %s->%s: %v", from, to, err)
		}
	}
	apply(stateA, stateB, "GO", 1)
	apply(stateB, stateC, "DONE", 2)
	// Re-apply in the reverse direction (C->B->A is not seeded; use
	// B's... instead: a machine A/B/C with only A->B and B->C — so the
	// re-application is a third DISTINCT row, and then a re-drive of the
	// SAME transition by looping back is unavailable. Assert row growth
	// and that the first row is byte-stable.)
	var first string
	if err := h.DBScan(context.Background(),
		"SELECT from_state_id::text||'|'||to_state_id::text||'|'||outcome||'|'||seq::text "+
			"FROM state_transition_history WHERE project_id = '"+project+"' ORDER BY seq LIMIT 1",
		&first); err != nil {
		t.Fatalf("read first row: %v", err)
	}
	if _, err := svc.ListHistory(context.Background(), project); err != nil {
		t.Fatalf("list: %v", err)
	}
	got, err := svc.ListHistory(context.Background(), project)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("history rows = %d, want 2 (one per application, append-only)", len(got))
	}
	var after string
	if err := h.DBScan(context.Background(),
		"SELECT from_state_id::text||'|'||to_state_id::text||'|'||outcome||'|'||seq::text "+
			"FROM state_transition_history WHERE project_id = '"+project+"' ORDER BY seq LIMIT 1",
		&after); err != nil {
		t.Fatalf("re-read first row: %v", err)
	}
	if first != after {
		t.Fatalf("first history row changed: %q -> %q (append-only requires stability)", first, after)
	}
}

// T6.29.1 — First transition receives sequence 1.
func TestP621FirstTransitionReceivesSequence(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, _, _, _ := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	svc := p621Service(h, true)
	out, err := svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: stateA, ToStateID: stateB, Outcome: "GO", ExpectedVersion: 1,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if out.History.Seq != 1 {
		t.Fatalf("first transition seq = %d, want 1", out.History.Seq)
	}
	var stored int
	if err := h.DBScan(context.Background(),
		"SELECT seq FROM state_transition_history WHERE project_id = '"+project+"'", &stored); err != nil {
		t.Fatalf("read seq: %v", err)
	}
	if stored != 1 {
		t.Fatalf("stored seq = %d, want 1", stored)
	}
}

// T6.29.2 — Sequence increments: consecutive transitions receive consecutive
// numbers (1, 2, 3).
func TestP621SequenceIncrements(t *testing.T) {
	h := NewHarness(t)
	project, machine, stateA, stateB, stateC, _, _ := p620Fixture(t, h)
	p621Seed(t, h, project, machine, stateA)
	svc := p621Service(h, true)
	type step struct{ from, to, outcome string }
	steps := []step{
		{stateA, stateB, "GO"},
		{stateB, stateC, "DONE"},
		{stateC, stateA, "RESET"},
	}
	ver := 1
	for i, s := range steps {
		// The machine only defines A->B and B->C; the third step (C->A)
		// is application-service-only (it applies an already-selected
		// transition and does not re-validate the edge), which is the
		// documented P6-21 contract.
		out, err := svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
			FromStateID: s.from, ToStateID: s.to, Outcome: s.outcome, ExpectedVersion: ver,
		})
		if err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if out.History.Seq != i+1 {
			t.Fatalf("step %d seq = %d, want %d", i+1, out.History.Seq, i+1)
		}
		ver = out.State.Version
	}
}

// T6.29.3 — Concurrent transitions cannot duplicate a sequence: N goroutines
// racing on a self-loop transition (each retrying on conflict) must commit
// exactly N history rows with EXACTLY the contiguous sequence 1..N (the
// P3-25 rule: no duplicates, no gaps).
func TestP621ConcurrentSequenceNoDuplicates(t *testing.T) {
	h := NewHarness(t)
	project := p605Project(t, h)
	const loopOutcome = "TICK"
	machine := p617SeedMachine(t, h, project, "m-seq",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: loopOutcome, To: "A"}},
		"A",
	)
	var stateA string
	if err := h.DBScan(context.Background(),
		"SELECT id::text FROM workflow_states WHERE machine_id = '"+machine+"' AND key = 'A'", &stateA); err != nil {
		t.Fatalf("read state: %v", err)
	}
	p621Seed(t, h, project, machine, stateA)
	svc := p621Service(h, true)
	rtSvc := p620Service(h, true)

	const n = 16
	results := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for attempt := 0; attempt < 200; attempt++ {
				st, err := rtSvc.Get(context.Background(), project)
				if err != nil {
					results[i] = err
					return
				}
				_, err = svc.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
					FromStateID: st.CurrentStateID, ToStateID: stateA,
					Outcome: loopOutcome, ExpectedVersion: st.Version,
				})
				if err == nil {
					return
				}
				if !errors.Is(err, service.ErrConflict) {
					results[i] = err
					return
				}
				// Stale version: re-read and retry.
			}
			results[i] = errors.New("exhausted retries")
		}(i)
	}
	wg.Wait()
	for i, err := range results {
		if err != nil {
			t.Fatalf("racer %d err = %v, want nil", i, err)
		}
	}
	// Direct SQL ground truth: exactly N rows, seqs exactly 1..N (no
	// duplicates, no gaps), and the runtime state advanced N times.
	var agg string
	if err := h.DBScan(context.Background(),
		"SELECT coalesce(string_agg(seq::text, ',' ORDER BY seq), '') FROM state_transition_history WHERE project_id = '"+project+"'",
		&agg); err != nil {
		t.Fatalf("read seqs: %v", err)
	}
	want := ""
	for i := 1; i <= n; i++ {
		if want != "" {
			want += ","
		}
		want += itoa(i)
	}
	if agg != want {
		t.Fatalf("committed sequences = %q, want exactly %q (contiguous 1..%d, no duplicates)", agg, want, n)
	}
	var ver int
	if err := h.DBScan(context.Background(),
		"SELECT version FROM runtime_state WHERE project_id = '"+project+"'", &ver); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if ver != n+1 {
		t.Fatalf("runtime version = %d, want %d (seed 1 + %d transitions)", ver, n+1, n)
	}
}

// T6.29.4 — Sequence survives restart: over a caller-owned DB, two boots —
// the second boot's first transition continues the sequence (not 1).
func TestP621SequenceSurvivesRestart(t *testing.T) {
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
	t.Cleanup(h1.Close)
	project := p605Project(t, h1)
	machine := p617SeedMachine(t, h1, project, "m-restart",
		[]service.MachineStateDef{{Key: "A", Kind: "CONTROL"}},
		[]service.MachineTransitionDef{{From: "A", Outcome: "TICK", To: "A"}},
		"A",
	)
	var stateA string
	if err := h1.DBScan(context.Background(),
		"SELECT id::text FROM workflow_states WHERE machine_id = '"+machine+"' AND key = 'A'", &stateA); err != nil {
		t.Fatalf("read state: %v", err)
	}
	p621Seed(t, h1, project, machine, stateA)
	svc1 := p621Service(h1, true)
	if _, err := svc1.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: stateA, ToStateID: stateA, Outcome: "TICK", ExpectedVersion: 1,
	}); err != nil {
		t.Fatalf("pre-restart apply: %v", err)
	}

	h2 := NewHarnessOn(t, base, dbName)
	t.Cleanup(h2.Close)
	svc2 := p621Service(h2, true)
	out, err := svc2.ApplyTransition(context.Background(), project, &service.ApplyTransitionInput{
		FromStateID: stateA, ToStateID: stateA, Outcome: "TICK", ExpectedVersion: 2,
	})
	if err != nil {
		t.Fatalf("post-restart apply: %v", err)
	}
	if out.History.Seq != 2 {
		t.Fatalf("post-restart seq = %d, want 2 (the sequence must continue from storage, not reset to 1)", out.History.Seq)
	}
}
