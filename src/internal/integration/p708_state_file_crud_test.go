package integration

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/service"
	"overseer/internal/statefile"
)

// P7-08 (WBS 7.16 service half + 7.27): the state-file CRUD service —
// create, read current/historical, and the controlled update guarded by the
// source-classification policy (DERIVED read-only; MANAGED/USER controlled
// update) plus the size limits. Driven over the harness pool (service-only
// surface — the REST API is P7-21).

// p708Service wires the state-file CRUD service against the harness pool.
func p708Service(t *testing.T, h *Harness) *service.StateFileService {
	t.Helper()
	raw := h.App.DB().Raw()
	svc := service.NewStateFileService(
		dbrepo.NewStateFileTypeRepository(raw),
		dbrepo.NewStateFileRepository(raw),
		dbrepo.NewStateFileVersionRepository(raw),
	).WithPhases(dbrepo.NewPhaseRepository(raw)).
		WithUnits(dbrepo.NewWorkUnitRepository(raw))
	if svc == nil {
		t.Fatal("nil service")
	}
	return svc.WithTxPool(raw)
}

// p708Type creates a project-scoped type with an explicit source class and
// returns its id (through the real type repository).
func p708Type(t *testing.T, h *Harness, projectID uuid.UUID, key, scope string, source statefile.SourceClass) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ft, err := statefile.NewFileType(statefile.StateFileTypeInput{
		ProjectID:       &projectID,
		Key:             key,
		Name:            key,
		DefaultFilename: key + ".md",
		Scope:           statefile.StateFileScope(scope),
		Source:          &source,
	})
	if err != nil {
		t.Fatalf("NewFileType: %v", err)
	}
	rec := p705TypeFromDomain(t, ft)
	if err := dbrepo.NewStateFileTypeRepository(h.App.DB().Raw()).Create(ctx, rec); err != nil {
		t.Fatalf("create type: %v", err)
	}
	return rec.ID
}

// p708File creates a state file through the service and returns its id.
func p708File(t *testing.T, h *Harness, svc *service.StateFileService, projectID, typeID uuid.UUID, scope string, phaseID, unitID *uuid.UUID) uuid.UUID {
	t.Helper()
	var ph, un *string
	if phaseID != nil {
		s := phaseID.String()
		ph = &s
	}
	if unitID != nil {
		s := unitID.String()
		un = &s
	}
	v, err := svc.CreateFile(context.Background(), service.CreateFileInput{
		ProjectID:  projectID.String(),
		TypeID:     typeID.String(),
		Scope:      scope,
		PhaseID:    ph,
		WorkUnitID: un,
	})
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	return uuid.MustParse(v.ID)
}

// p708TypeAndFile creates a type with the given source class and a
// PROJECT-scoped file of that type, returning both ids.
func p708TypeAndFile(t *testing.T, h *Harness, svc *service.StateFileService, projectID uuid.UUID, key string, source statefile.SourceClass) (typeID, fileID uuid.UUID) {
	t.Helper()
	typeID = p708Type(t, h, projectID, key, "PROJECT", source)
	fileID = p708File(t, h, svc, projectID, typeID, "PROJECT", nil, nil)
	return typeID, fileID
}

// Create: the file persists and the cross-project association guard fires
// (T7.4.5/T7.4.6 service half — WBS 7.4).
func TestP708CreateFilePersists(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, phaseID, _ := p705Rig(t, h)
	typeID, fileID := p708TypeAndFile(t, h, svc, projectID, "project-state", statefile.SourceManaged)

	_ = typeID
	var storedProject, storedType string
	if err := h.DBScan(ctx, "SELECT project_id::text, type_id::text FROM state_files WHERE id = '"+fileID.String()+"'", &storedProject, &storedType); err != nil {
		t.Fatalf("scan file row: %v", err)
	}
	if storedProject != projectID.String() || storedType != typeID.String() {
		t.Fatalf("stored (project,type) = (%s,%s)", storedProject, storedType)
	}
	var cur int
	if err := h.DBScan(ctx, "SELECT current_version FROM state_files WHERE id = '"+fileID.String()+"'", &cur); err != nil {
		t.Fatalf("scan current_version: %v", err)
	}
	if cur != 0 {
		t.Fatalf("new file current_version = %d, want 0", cur)
	}
	_ = phaseID
}

// Create: a PHASE association belonging to ANOTHER project is rejected with
// the invalid-reference sentinel (T7.4.5 service half).
func TestP708CreateFileCrossProjectPhaseRejected(t *testing.T) {
	h := NewHarness(t)
	svc := p708Service(t, h)
	projectA, phaseA, _ := p705Rig(t, h)
	projectBStr := createPhaseProject(t, h)
	projectB := uuid.MustParse(projectBStr)
	phaseB := seedPhaseStatus(t, h, projectB, 1, "Foundation", "PENDING")
	typeID := p708Type(t, h, projectA, "phase-state", "PHASE", statefile.SourceManaged)

	_, err := svc.CreateFile(context.Background(), service.CreateFileInput{
		ProjectID: projectA.String(),
		TypeID:    typeID.String(),
		Scope:     "PHASE",
		PhaseID:   ptrStr(phaseB.String()),
	})
	if !errors.Is(err, service.ErrInvalidReference) {
		t.Fatalf("cross-project phase association: err = %v, want ErrInvalidReference", err)
	}
	// The in-project association is accepted (non-vacuity).
	if _, err := svc.CreateFile(context.Background(), service.CreateFileInput{
		ProjectID: projectA.String(),
		TypeID:    typeID.String(),
		Scope:     "PHASE",
		PhaseID:   ptrStr(phaseA.String()),
	}); err != nil {
		t.Fatalf("in-project phase association rejected: %v", err)
	}
}

// T7.16.3 service half — a DERIVED file rejects the controlled update (the
// only content-write path at this layer); nothing is persisted.
func TestP708DerivedFileUpdateRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	_, fileID := p708TypeAndFile(t, h, svc, projectID, "active-task", statefile.SourceDerived)

	_, err := svc.UpdateContent(ctx, fileID.String(), "hand-edited", 0)
	if err == nil {
		t.Fatal("DERIVED controlled update: expected refusal, got success")
	}
	var n int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_file_versions WHERE state_file_id = '"+fileID.String()+"'", &n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if n != 0 {
		t.Fatalf("version rows after refused DERIVED update = %d, want 0", n)
	}
}

// T7.16.4 service half — MANAGED and USER files permit the controlled update
// path: a successful update appends a new version.
func TestP708ManagedUserUpdatePermitted(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)

	for i, sc := range []struct {
		key    string
		source statefile.SourceClass
	}{
		{"project-state", statefile.SourceManaged},
		{"architecture", statefile.SourceUser},
	} {
		_, fileID := p708TypeAndFile(t, h, svc, projectID, sc.key, sc.source)
		v, err := svc.UpdateContent(ctx, fileID.String(), "content-"+sc.key, 0)
		if err != nil {
			t.Fatalf("%s controlled update: %v", sc.key, err)
		}
		if v.Version != 1 {
			t.Fatalf("%s update version = %d, want 1", sc.key, v.Version)
		}
		var n int
		if err := h.DBScan(ctx, "SELECT count(*) FROM state_file_versions WHERE state_file_id = '"+fileID.String()+"'", &n); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		if n != 1 {
			t.Fatalf("%s version rows = %d, want 1", sc.key, n)
		}
		_ = i
	}
}

// T7.27.1 + T7.27.2 — content below the limit is accepted; content AT the
// limit (byte-exact) is accepted.
func TestP708SizeBelowAndAtLimitAccepted(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	_, fileID := p708TypeAndFile(t, h, svc, projectID, "project-state", statefile.SourceManaged)

	if _, err := svc.UpdateContent(ctx, fileID.String(), strings.Repeat("a", service.DefaultMaxContentBytes-1), 0); err != nil {
		t.Fatalf("below-limit update rejected: %v", err)
	}
	if _, err := svc.UpdateContent(ctx, fileID.String(), strings.Repeat("a", service.DefaultMaxContentBytes), 1); err != nil {
		t.Fatalf("at-limit update rejected: %v", err)
	}
	if got := p706Current(t, h, fileID); got != 2 {
		t.Fatalf("current_version = %d, want 2", got)
	}
}

// T7.27.3 + T7.27.4 — content above the hard limit is rejected and the error
// identifies the offending state file.
func TestP708AboveLimitRejectedIdentifiesFile(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	_, fileID := p708TypeAndFile(t, h, svc, projectID, "project-state", statefile.SourceManaged)

	_, err := svc.UpdateContent(ctx, fileID.String(), strings.Repeat("a", service.DefaultMaxContentBytes+1), 0)
	if err == nil {
		t.Fatal("above-limit update: expected refusal, got success")
	}
	if !errors.Is(err, service.ErrContentLimitExceeded) {
		t.Fatalf("error = %v, want ErrContentLimitExceeded", err)
	}
	if !strings.Contains(err.Error(), fileID.String()) {
		t.Fatalf("error %q does not identify the offending state file %s", err, fileID)
	}
}

// T7.27.5 — a rejected (over-limit) update leaves the stored content
// unchanged: same version count, same current pointer, byte-identical
// current content.
func TestP708RejectedUpdateLeavesContentUnchanged(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	_, fileID := p708TypeAndFile(t, h, svc, projectID, "project-state", statefile.SourceManaged)

	base := "original-content"
	if _, err := svc.UpdateContent(ctx, fileID.String(), base, 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if _, err := svc.UpdateContent(ctx, fileID.String(), strings.Repeat("z", service.DefaultMaxContentBytes+1), 1); err == nil {
		t.Fatal("over-limit update accepted, want refusal")
	}

	var n, cur, contentLen int
	if err := h.DBScan(ctx, "SELECT count(*) FROM state_file_versions WHERE state_file_id = '"+fileID.String()+"'", &n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT current_version FROM state_files WHERE id = '"+fileID.String()+"'", &cur); err != nil {
		t.Fatalf("scan pointer: %v", err)
	}
	if err := h.DBScan(ctx, "SELECT length(content) FROM state_file_versions WHERE state_file_id = '"+fileID.String()+"'", &contentLen); err != nil {
		t.Fatalf("scan content: %v", err)
	}
	if n != 1 || cur != 1 || contentLen != len(base) {
		t.Fatalf("after rejected update: versions=%d pointer=%d len=%d, want 1/1/%d", n, cur, contentLen, len(base))
	}
	v, err := svc.GetVersion(ctx, fileID.String(), 1)
	if err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if v.Content != base {
		t.Fatalf("v1 content = %q, want %q", v.Content, base)
	}
}

// Read ops — current version and historical version reads return the exact
// stored content (WBS 7.5 read surface).
func TestP708ReadCurrentAndHistorical(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	svc := p708Service(t, h)
	projectID, _, _ := p705Rig(t, h)
	_, fileID := p708TypeAndFile(t, h, svc, projectID, "project-state", statefile.SourceManaged)

	if _, err := svc.UpdateContent(ctx, fileID.String(), "v1-content", 0); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if _, err := svc.UpdateContent(ctx, fileID.String(), "v2-content", 1); err != nil {
		t.Fatalf("v2: %v", err)
	}
	cur, err := svc.GetCurrentVersion(ctx, fileID.String())
	if err != nil {
		t.Fatalf("GetCurrentVersion: %v", err)
	}
	if cur.Version != 2 || cur.Content != "v2-content" {
		t.Fatalf("current = %+v", cur)
	}
	hist, err := svc.GetVersion(ctx, fileID.String(), 1)
	if err != nil {
		t.Fatalf("GetVersion(1): %v", err)
	}
	if hist.Content != "v1-content" {
		t.Fatalf("historical v1 = %q", hist.Content)
	}
	// A file without content returns (nil, nil) — the P7-06 contract.
	_, other := p708TypeAndFile(t, h, svc, projectID, "test-state", statefile.SourceUser)
	empty, err := svc.GetCurrentVersion(ctx, other.String())
	if err != nil || empty != nil {
		t.Fatalf("GetCurrentVersion(empty) = (%+v, %v), want (nil, nil)", empty, err)
	}
}

// ptrStr is a small pointer-to-string helper for optional input fields.
func ptrStr(s string) *string { return &s }
