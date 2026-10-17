package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/service"
)

// fakeProjectRepo is a controllable in-memory persistence.ProjectRepository
// for P3-01 unit tests.
type fakeProjectRepo struct {
	projects map[string]*project.Project

	createErr  error
	getErr     error
	updateErr  error
	deleteErr  error
	listErr    error
	getByID    string
	updateName string
}

var _ persistence.ProjectRepository = (*fakeProjectRepo)(nil)

func newFakeRepo() *fakeProjectRepo {
	return &fakeProjectRepo{projects: map[string]*project.Project{}}
}

// WithTx returns the same fake (unit-test scope: tx binding is a no-op;
// P3-02/3.1.8 drive the real repositories for transaction behavior).
func (f *fakeProjectRepo) WithTx(tx persistence.Transaction) persistence.ProjectRepository {
	return f
}

func (f *fakeProjectRepo) Create(ctx context.Context, p *project.Project) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.projects[p.ID.String()] = p
	return nil
}

func (f *fakeProjectRepo) Get(ctx context.Context, id string) (*project.Project, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getByID != "" && id != f.getByID {
		return nil, persistence.ErrNotFound
	}
	p, ok := f.projects[id]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return p, nil
}

func (f *fakeProjectRepo) Update(ctx context.Context, p *project.Project) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	if f.updateName != "" {
		p.Name = f.updateName
	}
	if _, ok := f.projects[p.ID.String()]; !ok {
		return persistence.ErrNotFound
	}
	f.projects[p.ID.String()] = p
	return nil
}

// UpdateWithVersion (P3-24, WBS 3.24): stale version is a conflict.
func (f *fakeProjectRepo) UpdateWithVersion(ctx context.Context, p *project.Project, expectedVersion int) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	cur, ok := f.projects[p.ID.String()]
	if !ok {
		return persistence.ErrNotFound
	}
	if cur.Version != expectedVersion {
		return persistence.ErrConflict
	}
	cur.Version = expectedVersion + 1
	p.Version = cur.Version
	f.projects[p.ID.String()] = p
	return nil
}

func (f *fakeProjectRepo) Delete(ctx context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.projects[id]; !ok {
		return persistence.ErrNotFound
	}
	delete(f.projects, id)
	return nil
}

func (f *fakeProjectRepo) List(ctx context.Context, filter persistence.ProjectListFilter) ([]*project.Project, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*project.Project
	for _, p := range f.projects {
		if filter.Status != "" && p.Status != filter.Status {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// T3.1.1 — Service creates project: valid input creates and returns a
// project.
func TestT311CreateReturnsProject(t *testing.T) {
	repo := newFakeRepo()
	svc := service.NewProjectService(repo, nil, nil)
	got, _, err := svc.CreateProject(context.Background(), "svc project", "description")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got == nil {
		t.Fatal("nil project returned")
	}
	if got.Name != "svc project" || got.Description != "description" {
		t.Fatalf("project not returned to caller: %+v", got)
	}
	if got.Status != project.StatusDRAFT {
		t.Fatalf("initial status = %s, want DRAFT", got.Status)
	}
	if got.ID == [16]byte{} {
		t.Fatal("project has zero ID")
	}
}

// T3.1.2 — Domain validation propagated: invalid project data returns
// application validation errors (and nothing is persisted).
func TestT312ValidationPropagated(t *testing.T) {
	repo := newFakeRepo()
	svc := service.NewProjectService(repo, nil, nil)
	ctx := context.Background()

	for _, name := range []string{"", "   ", "\t\n"} {
		if _, _, err := svc.CreateProject(ctx, name, ""); !errors.Is(err, service.ErrInvalidInput) {
			t.Fatalf("create(%q): got %v, want ErrInvalidInput", name, err)
		}
	}
	if len(repo.projects) != 0 {
		t.Fatalf("invalid creates persisted: %d", len(repo.projects))
	}
}

// deleteSeedID is a fixed project ID so the delete case's setup and call
// reference the same project (seed() mints a fresh ID per call).
var deleteSeedID = func() string {
	p, _ := project.New("seed", "")
	return p.ID.String()
}()

// T3.1.3 — Repository failures translated: every repository error type
// arrives at the caller as a service-level domain error, never a raw
// persistence error.
func TestT313RepositoryFailuresTranslated(t *testing.T) {
	ctx := context.Background()
	seed := func() *project.Project {
		p, _ := project.New("seed", "")
		return p
	}

	cases := []struct {
		name   string
		seedID string
		repo   *fakeProjectRepo
		call   func(s *service.ProjectService) (*project.Project, error)
		expect error
	}{
		{
			name: "create-conflict",
			repo: func() *fakeProjectRepo { r := newFakeRepo(); r.createErr = persistence.ErrConflict; return r }(),
			call: func(s *service.ProjectService) (*project.Project, error) {
				_, _, e := s.CreateProject(ctx, "x", "")
				return nil, e
			},
			expect: service.ErrConflict,
		},
		{
			name: "create-infrastructure",
			repo: func() *fakeProjectRepo {
				r := newFakeRepo()
				r.createErr = &persistence.InfrastructureError{Op: "create", Cause: errors.New("boom")}
				return r
			}(),
			call: func(s *service.ProjectService) (*project.Project, error) {
				_, _, e := s.CreateProject(ctx, "x", "")
				return nil, e
			},
			expect: service.ErrInfrastructure,
		},
		{
			name: "get-not-found",
			repo: func() *fakeProjectRepo { r := newFakeRepo(); r.getErr = persistence.ErrNotFound; return r }(),
			call: func(s *service.ProjectService) (*project.Project, error) {
				return s.GetProject(ctx, seed().ID.String())
			},
			expect: service.ErrNotFound,
		},
		{
			name: "update-conflict",
			repo: func() *fakeProjectRepo { r := newFakeRepo(); r.updateErr = persistence.ErrConflict; return r }(),
			call: func(s *service.ProjectService) (*project.Project, error) {
				return s.UpdateProject(ctx, seed())
			},
			expect: service.ErrConflict,
		},
		{
			// Deletion-eligibility (P3-13) reads the project's status first,
			// so the fake must hold the project for Get to succeed; the
			// forced deleteErr is then what the translation layer must map.
			name: "delete-invalid-reference",
			repo: func() *fakeProjectRepo {
				r := newFakeRepo()
				r.deleteErr = persistence.ErrInvalidReference
				id, _ := uuid.Parse(deleteSeedID)
				r.projects[id.String()] = &project.Project{ID: id, Status: project.StatusDRAFT}
				return r
			}(),
			call: func(s *service.ProjectService) (*project.Project, error) {
				return s.DeleteProject(ctx, deleteSeedID)
			},
			expect: service.ErrInvalidReference,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := service.NewProjectService(tc.repo, nil, nil)
			_, err := tc.call(svc)
			if !errors.Is(err, tc.expect) {
				t.Fatalf("got %v, want %v", err, tc.expect)
			}
			var le *persistence.InfrastructureError
			if errors.As(err, &le) {
				t.Fatalf("raw persistence error escaped service boundary: %v", err)
			}
		})
	}
}

// T3.1.4 — Service retrieves project.
func TestT314Retrieve(t *testing.T) {
	repo := newFakeRepo()
	svc := service.NewProjectService(repo, nil, nil)
	ctx := context.Background()

	created, _, err := svc.CreateProject(ctx, "retrieve me", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.GetProject(ctx, created.ID.String())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != created.ID || got.Name != created.Name {
		t.Fatalf("mismatch: %+v", got)
	}

	if _, err := svc.GetProject(ctx, "not-a-uuid"); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("invalid id: got %v, want ErrInvalidInput", err)
	}
	unknown := uuid.New().String()
	if _, err := svc.GetProject(ctx, unknown); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown id: got %v, want ErrNotFound", err)
	}
	repo.getErr = persistence.ErrNotFound
	if _, err := svc.GetProject(ctx, created.ID.String()); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("repository failure: got %v, want ErrNotFound", err)
	}
}

// T3.1.5 — Service lists projects.
func TestT315List(t *testing.T) {
	repo := newFakeRepo()
	svc := service.NewProjectService(repo, nil, nil)
	ctx := context.Background()

	got, err := svc.ListProjects(ctx, persistence.ProjectListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty list = %d items", len(got))
	}

	if _, _, err := svc.CreateProject(ctx, "one", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateProject(ctx, "two", ""); err != nil {
		t.Fatal(err)
	}
	got, err = svc.ListProjects(ctx, persistence.ProjectListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("list = %d items, want 2", len(got))
	}
	repo.listErr = &persistence.InfrastructureError{Op: "list", Cause: errors.New("boom")}
	if _, err := svc.ListProjects(ctx, persistence.ProjectListFilter{}); !errors.Is(err, service.ErrInfrastructure) {
		t.Fatalf("list failure: got %v, want ErrInfrastructure", err)
	}
}

// T3.1.6 — Service updates project.
func TestT316Update(t *testing.T) {
	repo := newFakeRepo()
	svc := service.NewProjectService(repo, nil, nil)
	ctx := context.Background()

	created, _, err := svc.CreateProject(ctx, "before", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created.Name = "after"
	updated, err := svc.UpdateProject(ctx, created)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "after" {
		t.Fatalf("update not persisted: %q", updated.Name)
	}

	bad := &project.Project{ID: created.ID, Name: "   ", Description: updated.Description, Status: updated.Status}
	if _, err := svc.UpdateProject(ctx, bad); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("blank name: got %v, want ErrInvalidInput", err)
	}
	if got, err := svc.GetProject(ctx, created.ID.String()); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.Name != "after" {
		t.Fatalf("rejected update mutated data: %q", got.Name)
	}

	ghost, _ := project.New("ghost", "")
	if _, err := svc.UpdateProject(ctx, ghost); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("unknown id: got %v, want ErrNotFound", err)
	}
}

// T3.1.7 — Service deletes eligible project.
func TestT317Delete(t *testing.T) {
	repo := newFakeRepo()
	svc := service.NewProjectService(repo, nil, nil)
	ctx := context.Background()

	created, _, err := svc.CreateProject(ctx, "delete me", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.DeleteProject(ctx, created.ID.String()); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.GetProject(ctx, created.ID.String()); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("deleted project still retrievable: %v", err)
	}

	if _, err := svc.DeleteProject(ctx, created.ID.String()); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("delete unknown: got %v, want ErrNotFound", err)
	}
	if _, err := svc.DeleteProject(ctx, "not-a-uuid"); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("delete invalid id: got %v, want ErrInvalidInput", err)
	}
	// A fresh project so the deletion-eligibility Get succeeds; the forced
	// deleteErr is what the translation layer must map to ErrConflict.
	repo.deleteErr = persistence.ErrConflict
	later, _, err := svc.CreateProject(ctx, "delete conflict", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.DeleteProject(ctx, later.ID.String()); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("delete conflict: got %v, want ErrConflict", err)
	}
}

// T3.1.8 — Service coordinates transactions where required: the service
// exposes a transaction seam and work executed through it commits to the
// real database.
func TestT318TransactionCoordination(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	pool := h.App.DB().Raw()

	svc := service.NewProjectService(
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewSettingsRepository(pool),
		pool,
	)
	var created *project.Project
	err := svc.RunInTx(ctx, func(tx persistence.Transaction) error {
		var err error
		created, _, err = svc.CreateProject(ctx, "tx project", "")
		return err
	})
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}

	var count int
	if err := h.DBScan(ctx, "SELECT COUNT(*) FROM projects WHERE id = '"+created.ID.String()+"'", &count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("transactional create not committed: %d rows", count)
	}
}
