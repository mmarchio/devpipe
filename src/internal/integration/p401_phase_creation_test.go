package integration

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/phase"
	"overseer/internal/service"
)

// newPhaseService wires the real PhaseService to the real repositories on
// the harness pool (WBS 4.2: phase-creation contract at the service layer).
func newPhaseService(t *testing.T, h *Harness) *service.PhaseService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewPhaseService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		nil,
		nil,
		nil,
		nil,
	)
}

// createPhaseProject creates a project through the service and returns its ID.
func createPhaseProject(t *testing.T, h *Harness) string {
	t.Helper()
	pool := h.App.DB().Raw()
	projects := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, _, err := projects.CreateProject(context.Background(), "p4 phase creation", "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p.ID.String()
}

// T4.2.1 — Valid phase created: the service returns a fully populated,
// server-identified phase that is persisted.
func TestT421ValidPhaseCreated(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Foundation", "first phase")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	if p.ID == [16]byte{} {
		t.Fatal("returned phase has zero ID")
	}
	if p.Name != "Foundation" || p.Description != "first phase" {
		t.Fatalf("phase fields = (%q %q), want (Foundation first phase)", p.Name, p.Description)
	}
	if p.ProjectID.String() != projectID {
		t.Fatalf("phase project = %s, want %s", p.ProjectID, projectID)
	}
	var storedName, storedDesc string
	if err := h.DBScan(ctx,
		"SELECT name, description FROM phases WHERE id = '"+p.ID.String()+"'", &storedName, &storedDesc); err != nil {
		t.Fatal(err)
	}
	if storedName != "Foundation" || storedDesc != "first phase" {
		t.Fatalf("stored phase = (%q %q)", storedName, storedDesc)
	}
}

// T4.2.2 — Empty name rejected: blank names fail validation before any
// write; nothing is persisted.
func TestT422EmptyNameRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	for _, name := range []string{"", "   ", "\t\n"} {
		if _, err := svc.CreatePhase(ctx, projectID, name, ""); !errors.Is(err, service.ErrInvalidInput) {
			t.Fatalf("CreatePhase(%q): got %v, want ErrInvalidInput", name, err)
		}
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM phases WHERE project_id = '"+projectID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected creates persisted %d rows, want 0", count)
	}
}

// T4.2.3 — Unknown project rejected: a project the service cannot find is
// ErrNotFound; a malformed project id is ErrInvalidInput.
func TestT423UnknownProjectRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := newPhaseService(t, h)

	unknown := "00000000-0000-0000-0000-000000000000"
	if _, err := svc.CreatePhase(ctx, unknown, "X", ""); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown project: got %v, want ErrNotFound", err)
	}
	if _, err := svc.CreatePhase(ctx, "not-a-uuid", "X", ""); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("malformed project id: got %v, want ErrInvalidInput", err)
	}
}

// T4.2.4 — Initial status PENDING: both the returned object and the stored
// row carry PENDING with no start/completion timestamps.
func TestT424InitialStatusPending(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "Pending Check", "")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	if p.Status != phase.StatusPENDING {
		t.Fatalf("returned status = %s, want PENDING", p.Status)
	}
	if p.StartedAt != nil || p.CompletedAt != nil {
		t.Fatalf("new phase carries timestamps: started=%v completed=%v", p.StartedAt, p.CompletedAt)
	}
	var status, started, completed string
	if err := h.DBScan(ctx,
		"SELECT status, coalesce(started_at::text,''), coalesce(completed_at::text,'') FROM phases WHERE id = '"+p.ID.String()+"'",
		&status, &started, &completed); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING" || started != "" || completed != "" {
		t.Fatalf("stored status = %q (started=%q completed=%q), want PENDING with no timestamps", status, started, completed)
	}
}

// T4.2.5 — First phase receives order 1, server-assigned.
func TestT425FirstPhaseOrderOne(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	p, err := svc.CreatePhase(ctx, projectID, "First", "")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	if p.Order != 1 {
		t.Fatalf("first phase order = %d, want 1", p.Order)
	}
	var stored int
	if err := h.DBScan(ctx, "SELECT phase_order FROM phases WHERE id = '"+p.ID.String()+"'", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored order = %d, want 1", stored)
	}
}

// T4.2.6 — Subsequent phase receives MAX(order)+1: sequential appends
// produce exactly the contiguous sequence 1..N.
func TestT426SubsequentPhaseMaxPlusOne(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)
	svc := newPhaseService(t, h)

	for i, name := range []string{"One", "Two", "Three", "Four"} {
		p, err := svc.CreatePhase(ctx, projectID, name, "")
		if err != nil {
			t.Fatalf("CreatePhase(%s): %v", name, err)
		}
		if p.Order != i+1 {
			t.Fatalf("phase %d order = %d, want %d (MAX+1)", i+1, p.Order, i+1)
		}
	}
	list, err := svc.ListPhases(ctx, projectID)
	if err != nil {
		t.Fatalf("ListPhases: %v", err)
	}
	for i, p := range list {
		if p.Order != i+1 {
			t.Fatalf("list order %d = %d, want %d", i, p.Order, i+1)
		}
	}
}

// T4.2.7 — Client cannot override assigned order: the service API takes no
// order argument (compile-level), and the HTTP create body has no order
// field — a supplied order is rejected 400 and nothing is persisted.
func TestT427NoClientOrderOverride(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID := createPhaseProject(t, h)

	status, _ := doJSON(t, http.MethodPost, phasesURL(t, h, projectID),
		`{"name":"X","order":99}`)
	if status != http.StatusBadRequest {
		t.Fatalf("POST with client order = %d, want 400 (order not in create contract)", status)
	}
	status, _ = doJSON(t, http.MethodPost, phasesURL(t, h, projectID), `{"order":5}`)
	if status != http.StatusBadRequest {
		t.Fatalf("POST order-only = %d, want 400 (name required)", status)
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM phases WHERE project_id = '"+projectID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected creates persisted %d rows, want 0", count)
	}

	svc := newPhaseService(t, h)
	p, err := svc.CreatePhase(ctx, projectID, "Server Assigned", "")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	var stored int
	if err := h.DBScan(ctx, "SELECT phase_order FROM phases WHERE id = '"+p.ID.String()+"'", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("server-assigned order = %d, want 1", stored)
	}
}

// Guard: the service create signature has exactly (ctx, projectID, name,
// description) — no ordering input. This compiles only while the contract
// holds; a future client-order parameter breaks the build here.
var _ = func(svc *service.PhaseService, ctx context.Context, projectID, name, description string) (*phase.Phase, error) {
	return svc.CreatePhase(ctx, projectID, name, description)
}
