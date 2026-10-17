package integration

import (
	"context"
	"errors"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
)

// P6-24 (WBS 6.33/6.34): lifecycle interactions with runtime state.
// Chosen semantics (stated here and in ACTIVE_TASK.md):
//
// T6.33.x: PAUSE/RESUME never touch the runtime_state row — the proof is a
// non-mutation snapshot of the FULL persisted row (direct SQL), not the
// service readback.
//
// T6.34.2: the rejection mechanism is a guard INSIDE the transition-
// application service (not a lifecycle-owned check): ApplyTransition reads
// the project's lifecycle status and, when it is CANCELLED, returns
// service.ErrInvalidInput BEFORE any write. A PAUSED project still accepts
// normal transitions (WBS 6.33 says pause does not block), so only
// CANCELLED is guarded. The runtime row and history are asserted untouched
// by direct SQL after the rejected call.

// p624Rig is the shared fixture: a project (driven to RUNNING by direct
// SQL — lifecycle-op owned, the accepted test pattern), a seeded runtime
// state positioned at stateB with the phase and work-unit bindings, and a
// lifecycle service over the harness pool. The phase also carries a WBS
// document (T6.34.4).
type p624Rig struct {
	h         *Harness
	lifecycle *service.LifecycleService
	project   string
	machine   string
	stateA    string
	stateB    string
	phase     string
	unit      string
}

func newP624Rig(t *testing.T) *p624Rig {
	t.Helper()
	h := NewHarness(t)
	pool := h.App.DB().Raw()
	project, machine, stateA, stateB, _, phase, unit := p620Fixture(t, h)
	seedWBSDoc(t, h, phase, "phase WBS content for p624")
	// Drive the project to RUNNING (lifecycle-op owned; direct SQL is the
	// accepted pattern for reaching states a test op cannot).
	if _, err := h.DBExec(context.Background(), "UPDATE projects SET status = 'RUNNING' WHERE id = '"+project+"'"); err != nil {
		t.Fatalf("set RUNNING: %v", err)
	}
	// Position the runtime at stateB with the phase/unit bindings (v2).
	p621Seed(t, h, project, machine, stateA)
	ph, un := phase, unit
	p623Apply(t, h, project, stateA, stateB, "GO", 1, &ph, &un)
	return &p624Rig{
		h: h,
		lifecycle: service.NewLifecycleService(
			dbrepo.NewProjectRepository(pool),
			dbrepo.NewStatusEventRepository(pool),
			pool,
		),
		project: project, machine: machine, stateA: stateA, stateB: stateB,
		phase: phase, unit: unit,
	}
}

// p624RuntimeSig is the byte signature of the FULL persisted runtime_state
// row (direct SQL) — the non-mutation snapshot T6.33.4 compares.
func p624RuntimeSig(t *testing.T, h *Harness, project string) string {
	t.Helper()
	var sig string
	err := h.DBScan(context.Background(),
		"SELECT current_state_id::text || '|' || coalesce(phase_id::text,'') || '|' || "+
			"coalesce(work_unit_id::text,'') || '|' || version::text || '|' || "+
			"state_machine_id::text || '|' || state_machine_version::text || '|' || "+
			"entered_at::text "+
			"FROM runtime_state WHERE project_id = '"+project+"'",
		&sig)
	if err != nil {
		t.Fatalf("runtime signature: %v", err)
	}
	return sig
}

// p624HistorySig is the count, max seq, and per-row content signature of
// the persisted transition history (direct SQL).
func p624HistorySig(t *testing.T, h *Harness, project string) (int, int, string) {
	t.Helper()
	var n, maxSeq int
	var sig string
	err := h.DBScan(context.Background(),
		"SELECT coalesce(count(*),0), coalesce(max(seq),0), coalesce(string_agg("+
			"from_state_id::text||to_state_id::text||outcome||seq::text, ',' ORDER BY seq),'') "+
			"FROM state_transition_history WHERE project_id = '"+project+"'",
		&n, &maxSeq, &sig)
	if err != nil {
		t.Fatalf("history signature: %v", err)
	}
	return n, maxSeq, sig
}

// T6.33.1 — Pause preserves current state: PAUSE (RUNNING -> PAUSED) leaves
// the persisted current_state_id unchanged.
func TestP624PausePreservesState(t *testing.T) {
	r := newP624Rig(t)
	if _, err := r.lifecycle.Pause(context.Background(), r.project); err != nil {
		t.Fatalf("pause: %v", err)
	}
	storedState, _, _, _ := p623Stored(t, r.h, r.project)
	if storedState != r.stateB {
		t.Fatalf("stored state after pause = %s, want %s", storedState, r.stateB)
	}
}

// T6.33.2 — Pause preserves phase: the same PAUSE leaves phase_id unchanged.
func TestP624PausePreservesPhase(t *testing.T) {
	r := newP624Rig(t)
	if _, err := r.lifecycle.Pause(context.Background(), r.project); err != nil {
		t.Fatalf("pause: %v", err)
	}
	_, storedPhase, _, _ := p623Stored(t, r.h, r.project)
	if storedPhase != r.phase {
		t.Fatalf("stored phase after pause = %s, want %s", storedPhase, r.phase)
	}
}

// T6.33.3 — Pause preserves work unit: the same PAUSE leaves work_unit_id
// unchanged.
func TestP624PausePreservesWorkUnit(t *testing.T) {
	r := newP624Rig(t)
	if _, err := r.lifecycle.Pause(context.Background(), r.project); err != nil {
		t.Fatalf("pause: %v", err)
	}
	_, _, storedUnit, _ := p623Stored(t, r.h, r.project)
	if storedUnit != r.unit {
		t.Fatalf("stored unit after pause = %s, want %s", storedUnit, r.unit)
	}
}

// T6.33.4 — Resume sees identical runtime state: after PAUSE then RESUME
// the full runtime_state row is byte-identical to pre-pause (no extra
// version bump, no extra history row).
func TestP624ResumeSeesIdenticalState(t *testing.T) {
	r := newP624Rig(t)
	before := p624RuntimeSig(t, r.h, r.project)
	bn, bseq, bhist := p624HistorySig(t, r.h, r.project)
	if _, err := r.lifecycle.Pause(context.Background(), r.project); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := r.lifecycle.Resume(context.Background(), r.project); err != nil {
		t.Fatalf("resume: %v", err)
	}
	after := p624RuntimeSig(t, r.h, r.project)
	if after != before {
		t.Fatalf("runtime row after pause+resume = %q, want byte-identical %q", after, before)
	}
	an, aseq, ahist := p624HistorySig(t, r.h, r.project)
	if an != bn || aseq != bseq || ahist != bhist {
		t.Fatalf("history after pause+resume = (%d, %d, %q), want (%d, %d, %q) — no extra history row",
			an, aseq, ahist, bn, bseq, bhist)
	}
}

// T6.34.1 — Cancellation preserves history: CANCEL (RUNNING -> CANCELLED)
// deletes and alters no transition_history rows.
func TestP624CancelPreservesHistory(t *testing.T) {
	r := newP624Rig(t)
	bn, bseq, bhist := p624HistorySig(t, r.h, r.project)
	if _, err := r.lifecycle.Cancel(context.Background(), r.project); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	an, aseq, ahist := p624HistorySig(t, r.h, r.project)
	if an != bn || aseq != bseq || ahist != bhist {
		t.Fatalf("history after cancel = (%d, %d, %q), want (%d, %d, %q)", an, aseq, ahist, bn, bseq, bhist)
	}
}

// T6.34.2 — Normal transitions rejected after cancellation: with the
// project CANCELLED, ApplyTransition returns service.ErrInvalidInput (the
// chosen guard mechanism — a project-status guard inside the transition-
// application service) and writes NOTHING: runtime row, history, and
// version all untouched (direct SQL). Both assembly paths (pool and
// sequential nil-pool) are covered.
func TestP624NormalTransitionRejectedAfterCancel(t *testing.T) {
	for _, usePool := range []bool{false, true} {
		usePool := usePool
		t.Run("pool", func(t *testing.T) {
			r := newP624Rig(t)
			before := p624RuntimeSig(t, r.h, r.project)
			bn, bseq, bhist := p624HistorySig(t, r.h, r.project)
			if _, err := r.lifecycle.Cancel(context.Background(), r.project); err != nil {
				t.Fatalf("cancel: %v", err)
			}
			_, _, _, version := p623Stored(t, r.h, r.project)
			svc := p621Service(r.h, usePool)
			if svc == nil {
				t.Fatal("nil service")
			}
			svc = svc.WithProjects(dbrepo.NewProjectRepository(r.h.App.DB().Raw()))
			_, err := svc.ApplyTransition(context.Background(), r.project, &service.ApplyTransitionInput{
				FromStateID: r.stateB, ToStateID: r.stateA, Outcome: "CONTINUE", ExpectedVersion: version,
			})
			if !errors.Is(err, service.ErrInvalidInput) {
				t.Fatalf("post-cancel apply err = %v, want service.ErrInvalidInput", err)
			}
			after := p624RuntimeSig(t, r.h, r.project)
			if after != before {
				t.Fatalf("runtime row after rejected apply = %q, want byte-identical %q", after, before)
			}
			an, aseq, ahist := p624HistorySig(t, r.h, r.project)
			if an != bn || aseq != bseq || ahist != bhist {
				t.Fatalf("history after rejected apply = (%d, %d, %q), want (%d, %d, %q)",
					an, aseq, ahist, bn, bseq, bhist)
			}
		})
	}
}

// T6.34.3 — Work-unit data retained: the stored work_unit_id and the
// work_units row itself survive cancellation.
func TestP624WorkUnitDataRetained(t *testing.T) {
	r := newP624Rig(t)
	if _, err := r.lifecycle.Cancel(context.Background(), r.project); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, _, storedUnit, _ := p623Stored(t, r.h, r.project)
	if storedUnit != r.unit {
		t.Fatalf("stored work_unit_id after cancel = %s, want %s", storedUnit, r.unit)
	}
	var n int
	if err := r.h.DBScan(context.Background(),
		"SELECT count(*) FROM work_units WHERE id = '"+r.unit+"'", &n); err != nil {
		t.Fatalf("count unit: %v", err)
	}
	if n != 1 {
		t.Fatalf("work_units row present = %d, want 1", n)
	}
}

// T6.34.4 — WBS/phase data retained: the phase row and its WBS document
// survive cancellation (data is preserved, not torn down).
func TestP624PhaseAndWBSDataRetained(t *testing.T) {
	r := newP624Rig(t)
	if _, err := r.lifecycle.Cancel(context.Background(), r.project); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	var phaseN, docN int
	if err := r.h.DBScan(context.Background(),
		"SELECT count(*) FROM phases WHERE id = '"+r.phase+"'", &phaseN); err != nil {
		t.Fatalf("count phase: %v", err)
	}
	if phaseN != 1 {
		t.Fatalf("phase row present = %d, want 1", phaseN)
	}
	if err := r.h.DBScan(context.Background(),
		"SELECT count(*) FROM phase_documents WHERE phase_id = '"+r.phase+"'", &docN); err != nil {
		t.Fatalf("count docs: %v", err)
	}
	if docN != 1 {
		t.Fatalf("WBS document present = %d, want 1", docN)
	}
}
