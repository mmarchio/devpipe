package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/project"
	"overseer/internal/service"
)

// p303Rig is a harness with a service wired to real repositories plus one
// project (DRAFT) created through the creation operation.
type p303Rig struct {
	h    *Harness
	svc  *service.SettingsService
	p    *project.Project
	base *project.Settings
}

func newP303Rig(t *testing.T) *p303Rig {
	t.Helper()
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()
	projSvc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	p, st, err := projSvc.CreateProject(ctx, "settings service project", "")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	svc := service.NewSettingsService(
		dbrepo.NewSettingsRepository(pool),
		dbrepo.NewProjectRepository(pool),
		pool,
	)
	return &p303Rig{h: h, svc: svc, p: p, base: st}
}

func (r *p303Rig) setStatus(t *testing.T, status string) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.h.DBExec(ctx, "UPDATE projects SET status = '"+status+"' WHERE id = '"+r.p.ID.String()+"'"); err != nil {
		t.Fatalf("set status: %v", err)
	}
}

// mustUUID parses a literal uuid for test use.
func mustUUID(t *testing.T, s string) string {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	return u.String()
}

// uuidFromStr parses a literal uuid into a uuid.UUID.
func uuidFromStr(s string) uuid.UUID {
	u, _ := uuid.Parse(s)
	return u
}

// T3.3.1 — Settings retrieved through the service.
func TestT331SettingsRetrieved(t *testing.T) {
	r := newP303Rig(t)
	ctx := context.Background()
	got, err := r.svc.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ProjectID != r.p.ID || got.ContextWindow != service.DefaultContextWindow {
		t.Fatalf("unexpected settings: %+v", got)
	}
	if _, err := r.svc.GetSettings(ctx, "not-a-uuid"); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("invalid id: got %v, want ErrInvalidInput", err)
	}
	ghostID := r.p.ID
	_ = ghostID
	if _, err := r.svc.GetSettings(ctx, mustUUID(t, "22222222-2222-2222-2222-222222222222")); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown project: got %v, want ErrNotFound", err)
	}
}

// T3.3.2 — Settings updated while DRAFT and while READY; changes persist.
func TestT332SettingsUpdated(t *testing.T) {
	r := newP303Rig(t)
	ctx := context.Background()

	// DRAFT.
	upd, _ := project.NewSettings(r.p.ID, "/tmp/work", "prov-w", "model-w", "prov-c", "model-c", 123456, 0.75, true, 3, 2*time.Minute)
	if _, err := r.svc.UpdateSettings(ctx, upd, projectVersion(t, r.h, r.p.ID.String())); err != nil {
		t.Fatalf("update (DRAFT): %v", err)
	}
	got, err := r.svc.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WorkingDirectory != "/tmp/work" || got.WorkerProvider != "prov-w" || got.ContextWindow != 123456 || got.MaxRetries != 3 || !got.AutoContinue {
		t.Fatalf("DRAFT update not persisted: %+v", got)
	}

	// READY.
	r.setStatus(t, "READY")
	upd2, _ := project.NewSettings(r.p.ID, "/tmp/work2", "pw2", "mw2", "pc2", "mc2", 99999, 0.5, false, 1, 30*time.Second)
	if _, err := r.svc.UpdateSettings(ctx, upd2, projectVersion(t, r.h, r.p.ID.String())); err != nil {
		t.Fatalf("update (READY): %v", err)
	}
	got, err = r.svc.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.WorkingDirectory != "/tmp/work2" || got.ContextWindow != 99999 {
		t.Fatalf("READY update not persisted: %+v", got)
	}
}

// T3.3.3 — Invalid context window rejected; row unchanged.
func TestT333InvalidContextWindow(t *testing.T) {
	r := newP303Rig(t)
	ctx := context.Background()
	bad, _ := project.NewSettings(r.p.ID, "/tmp", "", "", "", "", -1, 0.5, false, 0, time.Minute)
	if _, err := r.svc.UpdateSettings(ctx, bad, projectVersion(t, r.h, r.p.ID.String())); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
	got, err := r.svc.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextWindow != service.DefaultContextWindow {
		t.Fatalf("row changed: %+v", got)
	}
}

// T3.3.4 — Invalid context threshold rejected.
func TestT334InvalidThreshold(t *testing.T) {
	r := newP303Rig(t)
	ctx := context.Background()
	for _, th := range []float64{0, 1.5} {
		bad, _ := project.NewSettings(r.p.ID, "/tmp", "", "", "", "", 1000, th, false, 0, time.Minute)
		if _, err := r.svc.UpdateSettings(ctx, bad, projectVersion(t, r.h, r.p.ID.String())); !errors.Is(err, service.ErrInvalidInput) {
			t.Fatalf("threshold %v: got %v, want ErrInvalidInput", th, err)
		}
	}
	got, err := r.svc.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextThreshold != service.DefaultContextThreshold {
		t.Fatalf("row changed: %+v", got)
	}
}

// T3.3.5 — Invalid retry count rejected.
func TestT335InvalidRetries(t *testing.T) {
	r := newP303Rig(t)
	ctx := context.Background()
	bad, _ := project.NewSettings(r.p.ID, "/tmp", "", "", "", "", 1000, 0.5, false, -2, time.Minute)
	if _, err := r.svc.UpdateSettings(ctx, bad, projectVersion(t, r.h, r.p.ID.String())); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
	got, err := r.svc.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRetries != service.DefaultMaxRetries {
		t.Fatalf("row changed: %+v", got)
	}
}

// T3.3.6 — Invalid worker timeout rejected.
func TestT336InvalidTimeout(t *testing.T) {
	r := newP303Rig(t)
	ctx := context.Background()
	bad, _ := project.NewSettings(r.p.ID, "/tmp", "", "", "", "", 1000, 0.5, false, 0, -time.Second)
	if _, err := r.svc.UpdateSettings(ctx, bad, projectVersion(t, r.h, r.p.ID.String())); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput", err)
	}
	got, err := r.svc.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkerTimeout != service.DefaultWorkerTimeout {
		t.Fatalf("row changed: %+v", got)
	}
}

// T3.3.7 — RUNNING project rejects protected-setting changes; non-protected
// (provider/model) changes are allowed.
func TestT337RunningProtectsSettings(t *testing.T) {
	r := newP303Rig(t)
	ctx := context.Background()
	r.setStatus(t, "RUNNING")

	// Protected: context window.
	protected, _ := project.NewSettings(r.p.ID, r.base.WorkingDirectory, r.base.WorkerProvider, r.base.WorkerModel,
		r.base.CompressionProvider, r.base.CompressionModel, 777, r.base.ContextThreshold,
		r.base.AutoContinue, r.base.MaxRetries, r.base.WorkerTimeout)
	if _, err := r.svc.UpdateSettings(ctx, protected, projectVersion(t, r.h, r.p.ID.String())); !errors.Is(err, service.ErrConflict) {
		// T3.3.7 updated for the T3.15.5 contract: protected change while
		// RUNNING is a conflict (409), not invalid input.
		t.Fatalf("protected change (RUNNING): got %v, want ErrConflict", err)
	}
	got, err := r.svc.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.ContextWindow != service.DefaultContextWindow {
		t.Fatalf("protected change persisted: %+v", got)
	}

	// Non-protected: worker provider/model.
	allowed, _ := project.NewSettings(r.p.ID, r.base.WorkingDirectory, "new-prov", "new-model",
		r.base.CompressionProvider, r.base.CompressionModel, r.base.ContextWindow, r.base.ContextThreshold,
		r.base.AutoContinue, r.base.MaxRetries, r.base.WorkerTimeout)
	if _, err := r.svc.UpdateSettings(ctx, allowed, projectVersion(t, r.h, r.p.ID.String())); err != nil {
		t.Fatalf("non-protected change (RUNNING) rejected: %v", err)
	}
	got, err = r.svc.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkerProvider != "new-prov" || got.WorkerModel != "new-model" {
		t.Fatalf("non-protected change not persisted: %+v", got)
	}
}

// T3.3.8 — Settings update is atomic: multiple field changes land together
// in one row state; unknown project is rejected without partial state.
func TestT338AtomicUpdate(t *testing.T) {
	r := newP303Rig(t)
	ctx := context.Background()

	upd, _ := project.NewSettings(r.p.ID, "/atomic", "pw", "mw", "pc", "mc", 424242, 0.66, true, 5, time.Hour)
	if _, err := r.svc.UpdateSettings(ctx, upd, projectVersion(t, r.h, r.p.ID.String())); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := r.svc.GetSettings(ctx, r.p.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	// Every field of the update must be present in the same row state.
	if got.WorkingDirectory != "/atomic" || got.WorkerProvider != "pw" || got.WorkerModel != "mw" ||
		got.CompressionProvider != "pc" || got.CompressionModel != "mc" ||
		got.ContextWindow != 424242 || got.ContextThreshold != 0.66 || !got.AutoContinue ||
		got.MaxRetries != 5 || got.WorkerTimeout != time.Hour {
		t.Fatalf("partial update observed: %+v", got)
	}

	ghost := mustUUID(t, "33333333-3333-3333-3333-333333333333")
	bad, _ := project.NewSettings(uuidFromStr(ghost), "/nowhere", "", "", "", "", 1, 0.5, false, 0, time.Minute)
	if _, err := r.svc.UpdateSettings(ctx, bad, projectVersion(t, r.h, r.p.ID.String())); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown project: got %v, want ErrNotFound", err)
	}
}
