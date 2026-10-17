package statefile

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/wbs"
)

// P7-11 (WBS 7.13): the TASK_ARCHIVE renderer is a PURE function of the
// project's work units — deterministic, completed (status DONE) units only,
// and the DB work-unit records stay authoritative (the content is derived
// from them, never parsed back).
//
// Ordering rule (stated per the task NOTES and pinned by the integration
// test): ascending completed_at (the completion timestamp set once by the
// unit lifecycle), tie-broken by ascending (sequence, id) so the output is
// byte-stable even when completion timestamps coincide.

func p711Unit(seq int, name string, status wbs.WorkUnitStatus, completedAt *time.Time) *wbs.WorkUnit {
	u, _ := wbs.NewWorkUnit(uuid.New(), seq, name, wbs.WorkUnitKindATOMIC)
	u.Status = status
	u.CompletedAt = completedAt
	return u
}

// T7.13.1 — a completed unit appears in the generated archive.
func TestP711RenderTaskArchiveCompletedAppears(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	u := p711Unit(1, "p711-done", wbs.WorkUnitStatusDONE, &at)
	got := RenderTaskArchive([]*wbs.WorkUnit{u})
	if !strings.Contains(got, u.ID.String()) {
		t.Fatalf("archive missing completed unit id:\n%s", got)
	}
	if !strings.Contains(got, "p711-done") {
		t.Fatalf("archive missing completed unit name:\n%s", got)
	}
	if !strings.Contains(got, "Status: DONE") {
		t.Fatalf("archive missing completion status line:\n%s", got)
	}
	if !strings.Contains(got, "Completed: "+at.Format(time.RFC3339)) {
		t.Fatalf("archive missing completion timestamp:\n%s", got)
	}
}

// T7.13.2 — pending / non-DONE units are excluded.
func TestP711RenderTaskArchivePendingExcluded(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	done := p711Unit(1, "p711-done", wbs.WorkUnitStatusDONE, &at)
	for _, st := range []wbs.WorkUnitStatus{
		wbs.WorkUnitStatusPENDING, wbs.WorkUnitStatusREADY,
		wbs.WorkUnitStatusEXECUTING, wbs.WorkUnitStatusINCOMPLETE,
		wbs.WorkUnitStatusVALIDATING, wbs.WorkUnitStatusFAILED,
		wbs.WorkUnitStatusBLOCKED,
	} {
		pending := p711Unit(2, "p711-"+st.String(), st, &at)
		got := RenderTaskArchive([]*wbs.WorkUnit{done, pending})
		if strings.Contains(got, pending.ID.String()) || strings.Contains(got, pending.Name) {
			t.Fatalf("archive includes non-DONE unit %s (status %s):\n%s", pending.Name, st, got)
		}
		if !strings.Contains(got, done.ID.String()) {
			t.Fatalf("archive lost the completed unit:\n%s", got)
		}
	}
	// A DONE unit with no completed_at still appears (the timestamp is the
	// ordering key, not the inclusion criterion).
	late := p711Unit(3, "p711-done-nots", wbs.WorkUnitStatusDONE, nil)
	got := RenderTaskArchive([]*wbs.WorkUnit{late})
	if !strings.Contains(got, late.ID.String()) {
		t.Fatalf("archive excluded a DONE unit with nil completed_at:\n%s", got)
	}
}

// T7.13.3 — completion ordering is deterministic: ascending completed_at,
// tie-broken by (sequence, id); a later completion renders later even when
// its sequence is lower.
func TestP711RenderTaskArchiveOrderingDeterministic(t *testing.T) {
	first := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	// unit "later" completes AFTER "earlier" but has the LOWER sequence —
	// the timestamp ordering must rank it last (the sequence order would put
	// it first).
	earlier := p711Unit(2, "p711-earlier", wbs.WorkUnitStatusDONE, &first)
	later := p711Unit(1, "p711-later", wbs.WorkUnitStatusDONE, &second)

	got := RenderTaskArchive([]*wbs.WorkUnit{later, earlier})
	idxEarlier := strings.Index(got, earlier.ID.String())
	idxLater := strings.Index(got, later.ID.String())
	if idxEarlier < 0 || idxLater < 0 {
		t.Fatalf("archive missing units:\n%s", got)
	}
	if idxLater < idxEarlier {
		t.Fatalf("archive not ordered by completed_at (later completion rendered first):\n%s", got)
	}

	// Tied timestamps fall back to (sequence, id).
	tiedA := p711Unit(2, "p711-tied-a", wbs.WorkUnitStatusDONE, &first)
	tiedB := p711Unit(1, "p711-tied-b", wbs.WorkUnitStatusDONE, &first)
	got = RenderTaskArchive([]*wbs.WorkUnit{tiedB, tiedA})
	// Anchor on the unique names (the entry headings are shared text).
	iA := strings.Index(got, "Name: p711-tied-a")
	iB := strings.Index(got, "Name: p711-tied-b")
	if iB < 0 || iA < 0 || iA < iB {
		t.Fatalf("tied completed_at not tie-broken by sequence (want tied-b before tied-a):\n%s", got)
	}
}

// T7.13.4 — rebuilding the archive produces byte-identical content (two
// calls over the same records, and a re-render of the same records in a
// different input order, are byte-identical).
func TestP711RenderTaskArchiveRebuildStable(t *testing.T) {
	a := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	b := a.Add(time.Hour)
	u1 := p711Unit(1, "p711-u1", wbs.WorkUnitStatusDONE, &a)
	u2 := p711Unit(2, "p711-u2", wbs.WorkUnitStatusDONE, &b)
	first := RenderTaskArchive([]*wbs.WorkUnit{u1, u2})
	second := RenderTaskArchive([]*wbs.WorkUnit{u1, u2})
	if first != second {
		t.Fatalf("rebuild not byte-identical:\n%s\n---\n%s", first, second)
	}
	shuffled := RenderTaskArchive([]*wbs.WorkUnit{u2, u1})
	if shuffled != first {
		t.Fatalf("reordered input changed the content (ordering must be by completed_at, not input order):\n%s\n---\n%s", shuffled, first)
	}
}

// An archive with no completed units renders the stable empty-document
// shape (header only).
func TestP711RenderTaskArchiveEmpty(t *testing.T) {
	got := RenderTaskArchive(nil)
	if strings.Contains(got, "Status: ") {
		t.Fatalf("empty archive carries entries:\n%s", got)
	}
	if got != RenderTaskArchive(nil) {
		t.Fatal("empty archive not deterministic")
	}
}
