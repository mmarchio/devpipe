package integration

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

// P4-10 (WBS 4.1 + 4.9): WBSDocumentService upgrade to versioned WBS.
// CreateWBS stores v1, UpdateWBS appends versions (never overwrites),
// GetWBS returns the current version, GetWBSVersion / ListWBSVersions read
// history. Markdown is retained byte-exact; empty content and unknown
// phases are rejected; repository errors are translated.

// failingWBSVersionRepo is a WBS version repository whose appends always
// fail with a persistence infrastructure error (T4.1.5: the service must
// translate it to its own error set).
type failingWBSVersionRepo struct{}

func (failingWBSVersionRepo) AppendVersion(ctx context.Context, phaseID, content string) (*persistence.WBSVersionRecord, error) {
	return nil, &persistence.InfrastructureError{Op: "append version", Cause: errors.New("boom")}
}
func (failingWBSVersionRepo) GetVersion(ctx context.Context, phaseID string, version int) (*persistence.WBSVersionRecord, error) {
	return nil, persistence.ErrNotFound
}
func (failingWBSVersionRepo) ListVersions(ctx context.Context, phaseID string) ([]*persistence.WBSVersionRecord, error) {
	return nil, nil
}
func (failingWBSVersionRepo) GetCurrentVersion(ctx context.Context, phaseID string) (int, error) {
	return 0, nil
}
func (failingWBSVersionRepo) WithTx(tx persistence.Transaction) persistence.WBSVersionRepository {
	return failingWBSVersionRepo{}
}

func newVersionedWBSDocService(t *testing.T, h *Harness) *service.WBSDocumentService {
	t.Helper()
	pool := h.App.DB().Raw()
	return service.NewWBSDocumentService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewDocumentRepository(pool),
		dbrepo.NewWBSVersionRepository(pool),
		pool,
	)
}

func wbsPhaseForService(t *testing.T, h *Harness) (string, string) {
	t.Helper()
	projectID := createPhaseProject(t, h)
	p, err := newPhaseService(t, h).CreatePhase(context.Background(), projectID, "versioned", "")
	if err != nil {
		t.Fatalf("CreatePhase: %v", err)
	}
	return projectID, p.ID.String()
}

// T4.1.1 — CreateWBS through the service: the first WBS is stored as
// version 1 (wbs_versions row + current document row + returned record).
func TestT411CreateWBSThroughService(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)

	const content = "# Phase 1\n\n## 1.1 Scope\n"
	rec, err := svc.CreateWBS(ctx, projectID, phaseID, content)
	if err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if rec.Version != 1 {
		t.Fatalf("created version = %d, want 1", rec.Version)
	}
	if rec.Content != content {
		t.Fatalf("created content = %q, want %q", rec.Content, content)
	}
	var v int
	var stored string
	if err := h.DBScan(ctx,
		"SELECT version, content FROM wbs_versions WHERE phase_id = '"+phaseID+"'",
		&v, &stored); err != nil {
		t.Fatal(err)
	}
	if v != 1 || stored != content {
		t.Fatalf("stored version/content = %d/%q, want 1/exact content", v, stored)
	}
}

// T4.1.2 — GetWBS retrieves the current version: after an edit, the
// returned record carries the latest content and version.
func TestT412GetWBSRetrievesCurrentVersion(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "first content"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "second content", 0); err != nil {
		t.Fatalf("UpdateWBS: %v", err)
	}
	rec, err := svc.GetWBS(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("GetWBS: %v", err)
	}
	if rec.Version != 2 {
		t.Fatalf("GetWBS version = %d, want 2", rec.Version)
	}
	if rec.Content != "second content" {
		t.Fatalf("GetWBS content = %q, want the current version's content", rec.Content)
	}
}

// T4.1.3 — ListWBSVersions returns stored versions in ascending order with
// their exact contents.
func TestT413ListWBSVersionsAscending(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)

	contents := []string{"one", "two", "three"}
	if _, err := svc.CreateWBS(ctx, projectID, phaseID, contents[0]); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	for _, c := range contents[1:] {
		if _, err := svc.UpdateWBS(ctx, projectID, phaseID, c, 0); err != nil {
			t.Fatalf("UpdateWBS(%q): %v", c, err)
		}
	}
	versions, err := svc.ListWBSVersions(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("ListWBSVersions: %v", err)
	}
	if len(versions) != len(contents) {
		t.Fatalf("listed %d versions, want %d", len(versions), len(contents))
	}
	for i, v := range versions {
		if v.Version != i+1 || v.Content != contents[i] {
			t.Fatalf("position %d = version %d content %q, want %d/%q", i, v.Version, v.Content, i+1, contents[i])
		}
	}
}

// T4.1.4 — Unknown phase returns ErrNotFound on every WBS surface (as do
// malformed ids and cross-project phases).
func TestT414UnknownPhaseErrNotFound(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _ := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)
	unknown := "00000000-0000-0000-0000-000000000000"

	if _, err := svc.CreateWBS(ctx, projectID, unknown, "x"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("CreateWBS unknown phase: got %v, want ErrNotFound", err)
	}
	if _, err := svc.GetWBS(ctx, projectID, unknown); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("GetWBS unknown phase: got %v, want ErrNotFound", err)
	}
	if _, err := svc.UpdateWBS(ctx, projectID, unknown, "x", 0); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("UpdateWBS unknown phase: got %v, want ErrNotFound", err)
	}
	if _, err := svc.GetWBSVersion(ctx, projectID, unknown, 1); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("GetWBSVersion unknown phase: got %v, want ErrNotFound", err)
	}
	if _, err := svc.ListWBSVersions(ctx, projectID, unknown); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("ListWBSVersions unknown phase: got %v, want ErrNotFound", err)
	}
	if _, err := svc.GetWBS(ctx, "not-a-uuid", unknown); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("GetWBS malformed project id: got %v, want ErrInvalidInput", err)
	}
	// A phase that belongs to another project is out of scope.
	otherProject, otherPhase := wbsPhaseForService(t, h)
	_ = otherProject
	otherID := otherPhase
	if _, err := svc.GetWBS(ctx, projectID, otherID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("GetWBS cross-project phase: got %v, want ErrNotFound", err)
	}
}

// T4.1.5 — Repository errors translated correctly: a version-repository
// infrastructure failure surfaces as the service-level infrastructure
// error, not the persistence type.
func TestT415RepositoryErrorsTranslated(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	pool := h.App.DB().Raw()
	svc := service.NewWBSDocumentService(
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewProjectRepository(pool),
		dbrepo.NewDocumentRepository(pool),
		failingWBSVersionRepo{},
		pool,
	)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "x"); !errors.Is(err, service.ErrInfrastructure) {
		t.Fatalf("CreateWBS repo failure: got %v, want ErrInfrastructure", err)
	}
	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "x", 0); !errors.Is(err, service.ErrInfrastructure) {
		t.Fatalf("UpdateWBS repo failure: got %v, want ErrInfrastructure", err)
	}
}

// T4.1.6 — Service enforces lifecycle rules: an executed phase's WBS is
// protected (ErrConflict, nothing appended); a phase without a WBS is
// ErrNotFound on reads.
func TestT416ServiceEnforcesLifecycleRules(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "original"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := h.DBExec(ctx, "UPDATE phases SET status = 'EXECUTING', started_at = now() WHERE id = '"+phaseID+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "rewritten", 0); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("UpdateWBS executed phase: got %v, want ErrConflict", err)
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("protected update appended %d versions, want 1 (unmutated)", count)
	}
	// A fresh phase with no WBS yields ErrNotFound on reads.
	emptyProject, emptyPhase := wbsPhaseForService(t, h)
	if _, err := svc.GetWBS(ctx, emptyProject, emptyPhase); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("GetWBS phase without WBS: got %v, want ErrNotFound", err)
	}
	if _, err := svc.GetWBSVersion(ctx, emptyProject, emptyPhase, 1); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("GetWBSVersion phase without WBS: got %v, want ErrNotFound", err)
	}
}

// T4.1.7 — HTTP layer does not coordinate phase repositories directly: the
// full application (real wiring) serves WBS mutations and history through
// the service — edits via REST append versions, history is intact, and the
// WBS web source references no repository package.
func TestT417HTTPLayerUsesServiceOnly(t *testing.T) {
	h := NewHarness(t)
	projectID, phaseID := wbsSetup(t, h, "p417 http")

	if status, _ := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
		wbsPutBody(t, "http v1")); status != http.StatusOK {
		t.Fatalf("PUT WBS = %d, want 200", status)
	}
	status, out := doJSON(t, http.MethodPut, wbsURL(t, h, projectID, phaseID),
		wbsPutBody(t, "http v2"))
	if status != http.StatusOK {
		t.Fatalf("PUT update = %d, want 200", status)
	}
	if out["version"] != float64(2) {
		t.Fatalf("response version = %v, want 2", out["version"])
	}
	ctx := context.Background()
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("wbs_versions rows after two REST writes = %d, want 2 (service-mediated append)", count)
	}
	var v1, v2 string
	if err := h.DBScan(ctx, "SELECT content FROM wbs_versions WHERE phase_id = '"+phaseID+"' AND version = 1", &v1); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(ctx, "SELECT content FROM wbs_versions WHERE phase_id = '"+phaseID+"' AND version = 2", &v2); err != nil {
		t.Fatal(err)
	}
	if v1 != "http v1" || v2 != "http v2" {
		t.Fatalf("stored versions = %q/%q, want http v1/http v2 (never overwritten)", v1, v2)
	}
	_, out = doJSON(t, http.MethodGet, wbsURL(t, h, projectID, phaseID), "")
	if out["version"] != float64(2) || out["content"] != "http v2" {
		t.Fatalf("GET WBS = version %v content %v, want the current version 2", out["version"], out["content"])
	}
	// Static guard: the WBS web layer references the service, never a
	// repository implementation.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var src []byte
	for _, rel := range []string{"../web/wbs.go", "../../web/wbs.go"} {
		if b, rerr := os.ReadFile(filepath.Join(wd, rel)); rerr == nil {
			src = b
			break
		}
	}
	if len(src) == 0 {
		t.Fatal("web/wbs.go not found relative to test working directory")
	}
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "internal/dbrepo") {
		t.Fatal("web/wbs.go imports the repository package; the HTTP layer must coordinate WBS through the service only")
	}
}

// T4.9.1 — WBS created for phase: the document row and the version-1
// history row both exist after CreateWBS.
func TestT491WBSCreatedForPhase(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "# Phase WBS\n"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	var docCount, versionCount int
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM phase_documents WHERE phase_id = '"+phaseID+"' AND doc_type = 'WBS'", &docCount); err != nil {
		t.Fatal(err)
	}
	if err := h.DBScan(ctx,
		"SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &versionCount); err != nil {
		t.Fatal(err)
	}
	if docCount != 1 {
		t.Fatalf("WBS document rows = %d, want 1", docCount)
	}
	if versionCount != 1 {
		t.Fatalf("wbs_versions rows = %d, want 1", versionCount)
	}
}

// T4.9.2 — Markdown retained exactly: tabs, quotes, backslashes, and
// trailing newlines round-trip byte-for-byte through create, update, and
// historical reads.
func TestT492MarkdownRetainedExactly(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)

	content := "# Phase 3\n\n## Work unit\n- `code` with **bold**, \"quotes\" and \\ backslash\n\n\ttab-indented\n\n"
	if _, err := svc.CreateWBS(ctx, projectID, phaseID, content); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, content+"<!-- edited -->\n", 0); err != nil {
		t.Fatalf("UpdateWBS: %v", err)
	}
	if rec, err := svc.GetWBSVersion(ctx, projectID, phaseID, 1); err != nil || rec.Content != content {
		t.Fatalf("v1 content = %q (err %v), want byte-exact original", rec.Content, err)
	}
	if rec, err := svc.GetWBSVersion(ctx, projectID, phaseID, 2); err != nil || rec.Content != content+"<!-- edited -->\n" {
		t.Fatalf("v2 content = %q (err %v), want byte-exact edited content", rec.Content, err)
	}
}

// T4.9.3 — Empty WBS rejected where required: empty content fails
// validation on create and update before any mutation.
func TestT493EmptyWBSRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)

	if _, err := svc.CreateWBS(ctx, projectID, phaseID, ""); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("CreateWBS(\"\"): got %v, want ErrInvalidInput", err)
	}
	var count int
	if err := h.DBScan(ctx, "SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected create persisted %d versions, want 0", count)
	}
	if _, err := svc.CreateWBS(ctx, projectID, phaseID, "ok"); err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if _, err := svc.UpdateWBS(ctx, projectID, phaseID, "", 0); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("UpdateWBS(\"\"): got %v, want ErrInvalidInput", err)
	}
	if err := h.DBScan(ctx, "SELECT count(*) FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("rejected update persisted %d versions, want 1", count)
	}
}

// T4.9.4 — Unknown phase rejected: create/update against a missing phase
// is ErrNotFound; a malformed phase id is ErrInvalidInput.
func TestT494UnknownPhaseRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _ := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)

	unknown := "00000000-0000-0000-0000-000000000000"
	if _, err := svc.CreateWBS(ctx, projectID, unknown, "x"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("CreateWBS unknown phase: got %v, want ErrNotFound", err)
	}
	if _, err := svc.UpdateWBS(ctx, projectID, unknown, "x", 0); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("UpdateWBS unknown phase: got %v, want ErrNotFound", err)
	}
	if _, err := svc.CreateWBS(ctx, projectID, "not-a-uuid", "x"); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("CreateWBS malformed phase id: got %v, want ErrInvalidInput", err)
	}
}

// T4.9.5 — Initial WBS version is 1: the first version allocated for a
// phase is exactly 1, in the returned record and in storage.
func TestT495InitialWBSVersionIsOne(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, phaseID := wbsPhaseForService(t, h)
	svc := newVersionedWBSDocService(t, h)

	rec, err := svc.CreateWBS(ctx, projectID, phaseID, "initial")
	if err != nil {
		t.Fatalf("CreateWBS: %v", err)
	}
	if rec.Version != 1 {
		t.Fatalf("initial version = %d, want 1", rec.Version)
	}
	var stored int
	if err := h.DBScan(ctx,
		"SELECT version FROM wbs_versions WHERE phase_id = '"+phaseID+"'", &stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("stored initial version = %d, want 1", stored)
	}
	cur, err := svc.GetWBS(ctx, projectID, phaseID)
	if err != nil {
		t.Fatalf("GetWBS: %v", err)
	}
	if cur.Version != 1 {
		t.Fatalf("current version after create = %d, want 1", cur.Version)
	}
}
