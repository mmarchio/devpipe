package integration

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
)

// T2.6.1 — Phase persisted.
func TestT261PhasePersisted(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pid := uuid.New()
	if _, err := h.DBExec(ctx, insertProjectSQL(pid, "P")); err != nil {
		t.Fatalf("project: %v", err)
	}
	fid := uuid.New()
	if _, err := h.DBExec(ctx, `INSERT INTO phases (id, project_id, phase_order, name, status)
		VALUES ('`+fid.String()+`', '`+pid.String()+`', 1, 'Phase One', 'PENDING')`); err != nil {
		t.Fatalf("insert phase: %v", err)
	}
	var name string
	if err := h.DBScan(ctx, "SELECT name FROM phases WHERE id = '"+fid.String()+"'", &name); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if name != "Phase One" {
		t.Fatalf("persisted name %q", name)
	}
}

// T2.6.2 — Phase references valid project (FK violation).
func TestT262PhaseRequiresValidProject(t *testing.T) {
	h := NewHarness(t)
	_, err := h.DBExec(context.Background(), `INSERT INTO phases (id, project_id, phase_order, name, status)
		VALUES ('`+uuid.New().String()+`', '`+uuid.New().String()+`', 1, 'Orphan', 'PENDING')`)
	if err == nil {
		t.Fatal("phase with nonexistent project accepted; FK missing")
	}
}

// T2.6.3 — Duplicate phase order within a project rejected.
func TestT263DuplicateOrderRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pid := uuid.New()
	if _, err := h.DBExec(ctx, insertProjectSQL(pid, "P")); err != nil {
		t.Fatalf("project: %v", err)
	}
	sql := `INSERT INTO phases (id, project_id, phase_order, name, status)
		VALUES ('` + uuid.New().String() + `', '` + pid.String() + `', 1, 'A', 'PENDING')`
	if _, err := h.DBExec(ctx, sql); err != nil {
		t.Fatalf("first phase: %v", err)
	}
	sql2 := `INSERT INTO phases (id, project_id, phase_order, name, status)
		VALUES ('` + uuid.New().String() + `', '` + pid.String() + `', 1, 'B', 'PENDING')`
	if _, err := h.DBExec(ctx, sql2); err == nil {
		t.Fatal("duplicate order within project accepted")
	}
}

// T2.6.4 — Same order across different projects accepted.
func TestT264SameOrderAcrossProjects(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	p1, p2 := uuid.New(), uuid.New()
	if _, err := h.DBExec(ctx, insertProjectSQL(p1, "A")); err != nil {
		t.Fatalf("project: %v", err)
	}
	if _, err := h.DBExec(ctx, insertProjectSQL(p2, "B")); err != nil {
		t.Fatalf("project: %v", err)
	}
	for _, pid := range []uuid.UUID{p1, p2} {
		sql := `INSERT INTO phases (id, project_id, phase_order, name, status)
			VALUES ('` + uuid.New().String() + `', '` + pid.String() + `', 2, 'Phase Two', 'PENDING')`
		if _, err := h.DBExec(ctx, sql); err != nil {
			t.Fatalf("phase for project %s: %v", pid, err)
		}
	}
}

// T2.6.5 — Query returns phases ordered ascending.
func TestT265PhasesOrderedAscending(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pid := uuid.New()
	if _, err := h.DBExec(ctx, insertProjectSQL(pid, "P")); err != nil {
		t.Fatalf("project: %v", err)
	}
	// Insert out of order on purpose; the ascending query must reorder.
	for _, order := range []int{3, 1, 2} {
		sql := "INSERT INTO phases (id, project_id, phase_order, name, status) VALUES ('" +
			uuid.New().String() + "', '" + pid.String() + "', " + strconv.Itoa(order) +
			", 'P" + strconv.Itoa(order) + "', 'PENDING')"
		if _, err := h.DBExec(ctx, sql); err != nil {
			t.Fatalf("phase %d: %v", order, err)
		}
	}
	conn := h.connect(ctx)
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT phase_order FROM phases WHERE project_id = '"+pid.String()+"' ORDER BY phase_order ASC")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var orders []int
	for rows.Next() {
		var o int
		if err := rows.Scan(&o); err != nil {
			t.Fatalf("scan: %v", err)
		}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(orders) != 3 || orders[0] != 1 || orders[1] != 2 || orders[2] != 3 {
		t.Fatalf("orders %v, want [1 2 3]", orders)
	}
}
