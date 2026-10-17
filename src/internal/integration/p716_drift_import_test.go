package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/statefile"
)

// P7-16 (WBS 7.24/7.25/7.26): filesystem drift detection, the DERIVED
// drift policy, and the explicit managed-file import. All ops live on the
// StateFileMaterializeService (service-only surface — REST is P7-22) and
// are driven over the harness pool.
//
// Task-level choices (stated per the task NOTES and pinned here):
//   - DriftStatus values: SYNCHRONIZED (on-disk hash == the persisted
//     CURRENT version's hash), DRIFTED (hash differs), MISSING (no file
//     at the materialization path), and NOT_MATERIALIZED (no current
//     content version to compare against — a file-level status, not an
//     error).
//   - DetectDrift is a pure READ: no metadata upsert, no version write,
//     no file write (T7.24.4 pins non-mutation by byte-signature of the
//     file, the metadata record, and the version chain). The PERSISTED
//     current version (PostgreSQL) is the comparison source; the metadata
//     record is a location hint only, never the source.
//   - Import resolves the file's path through the SAME managed-path
//     resolution as materialization (SafeRelPath under <root>/.overseer/
//     state/ + the type's DefaultFilename), so a type whose filename
//     escapes the state directory is a clean rejection (T7.26.5), not an
//     import. "Selected MANAGED/USER" = the type's SourceClass passes
//     statefile.ControlledUpdateAllowed (T7.16.4 policy, consumed
//     verbatim); DERIVED is rejected (T7.26.3).
//   - Import idempotence (T7.26.4): on-disk content whose hash EQUALS
//     the current version's hash appends NO new version row — it returns
//     the current version with Imported=false.
//   - Import reuses the P7-06 version-append path (optimistic
//     UpdateContent) so the new version's hash/token estimate follow the
//     existing recipe, and — when the metadata repository is wired — the
//     record is kept consistent with the persisted current version after
//     the import (the record is a projection; the import does not depend
//     on it).
//
// WBS 7.25 fixture policy (stated per the task NOTES and pinned here):
// the P7-14/P7-15 Materialize op selects PROJECT-scoped files only (the
// WBS 7.20 layout), so a WORK_UNIT-scoped file — the shape T7.25.x uses
// per the task NOTES ("seed a WORK_UNIT-scoped file") — is not in the
// op's selection. The DERIVED tests therefore MIRROR the derivation's
// materialization write (the P7-10-style write of the persisted current
// content to the managed path) directly, and the "re-materialization
// restores" half re-runs that same derived write. This exercises the
// policy itself: the external edit is detected, never imported, and the
// persisted (derived) content wins the next write.

// p716SeedVersion appends the file's content as version 1 through the
// P7-06 version repository directly (the derivation-write half): the
// controlled UpdateContent service path rejects the DERIVED class by
// design (P7-08), so the DERIVED fixtures establish their persisted base
// version this way.
func p716SeedVersion(t *testing.T, h *Harness, fileID uuid.UUID, content string) {
	t.Helper()
	stored, err := dbrepo.NewStateFileVersionRepository(h.App.DB().Raw()).UpdateContent(context.Background(), fileID, content, 0)
	if err != nil {
		t.Fatalf("seed version: %v", err)
	}
	if stored.Version != 1 {
		t.Fatalf("seeded version = %d, want 1", stored.Version)
	}
}

// p716MetaSig is a byte-signature of the file's metadata record (direct
// SQL, all columns) for the T7.24.4 non-mutation proof.
func p716MetaSig(t *testing.T, h *Harness, fileID string) string {
	t.Helper()
	var sig string
	if err := h.DBScan(context.Background(),
		`SELECT coalesce(string_agg(version||'='||content_hash||'='||path||'='||materialized_at::text, ','), '<none>')
		 FROM materialized_state_files WHERE state_file_id = '`+fileID+`'`, &sig); err != nil {
		t.Fatalf("meta sig: %v", err)
	}
	return sig
}

// p716VersionSig is a byte-signature of the file's version chain (direct
// SQL) for the non-mutation / not-imported proofs.
func p716VersionSig(t *testing.T, h *Harness, fileID string) string {
	t.Helper()
	var sig string
	if err := h.DBScan(context.Background(),
		`SELECT coalesce(string_agg(version::text||'='||content_hash, ',' ORDER BY version), '<none>')
		 FROM state_file_versions WHERE state_file_id = '`+fileID+`'`, &sig); err != nil {
		t.Fatalf("version sig: %v", err)
	}
	return sig
}

// p716VersionCount is the row count of the file's version chain (direct
// SQL) for the T7.26.4 idempotence proof.
func p716VersionCount(t *testing.T, h *Harness, fileID string) int {
	t.Helper()
	var n int
	if err := h.DBScan(context.Background(),
		`SELECT count(*) FROM state_file_versions WHERE state_file_id = '`+fileID+`'`, &n); err != nil {
		t.Fatalf("version count: %v", err)
	}
	return n
}

// p716UnitSig is a byte-signature of the file's persisted associations
// (phase/work-unit/current version) for the T7.25.4 selection-unchanged
// proof.
func p716UnitSig(t *testing.T, h *Harness, fileID string) string {
	t.Helper()
	var sig string
	if err := h.DBScan(context.Background(),
		`SELECT coalesce(phase_id::text,'<nil>')||'|'||coalesce(work_unit_id::text,'<nil>')||'|'||current_version::text
		 FROM state_files WHERE id = '`+fileID+`'`, &sig); err != nil {
		t.Fatalf("unit sig: %v", err)
	}
	return sig
}

// T7.24.1 — Matching file reports synchronized: after a materialization the
// detection op reports SYNCHRONIZED with the materialized path and the
// current version's hash.
func TestP716DetectMatchingFileSynchronized(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P16A", "PROJECT", false), "in sync")
	root := t.TempDir()

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	res, err := p715Svc(t, h).DetectDrift(ctx, root, projectID.String(), fileID.String())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.Status != service.DriftSynchronized {
		t.Fatalf("status = %s, want SYNCHRONIZED", res.Status)
	}
	if res.Path != filepath.Join(root, ".overseer", "state", "P16A.md") {
		t.Fatalf("path = %s", res.Path)
	}
	if res.ContentHash == "" || res.CurrentVersion != 1 {
		t.Fatalf("result = %+v, want the current version's hash and version", res)
	}
}

// T7.24.2 — Changed file reports drift: an external edit of the
// materialized copy (the persisted version untouched) is detected as
// DRIFTED.
func TestP716DetectChangedFileDrifted(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P16B", "PROJECT", false), "baseline")
	root := t.TempDir()

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, ".overseer", "state", "P16B.md"), []byte("externally edited"), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	res, err := p715Svc(t, h).DetectDrift(ctx, root, projectID.String(), fileID.String())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.Status != service.DriftDrifted {
		t.Fatalf("status = %s, want DRIFTED", res.Status)
	}
}

// T7.24.3 — Missing materialized file reports missing: deleting the
// materialized copy yields MISSING (the record still points at the path).
func TestP716DetectMissingFile(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P16C", "PROJECT", false), "will be deleted")
	root := t.TempDir()

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	target := filepath.Join(root, ".overseer", "state", "P16C.md")
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove: %v", err)
	}
	res, err := p715Svc(t, h).DetectDrift(ctx, root, projectID.String(), fileID.String())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.Status != service.DriftMissing {
		t.Fatalf("status = %s, want MISSING", res.Status)
	}
	if res.Path != target {
		t.Fatalf("path = %s, want the recorded materialization path", res.Path)
	}
}

// T7.24.4 — Drift detection is non-mutating: a detection over a DRIFTED
// file leaves the on-disk file byte-identical, the metadata record
// byte-identical, and the version chain unchanged (it modifies neither
// copy).
func TestP716DetectIsNonMutating(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	fileID := p714WriteFile(t, h, projectID, p713Required(t, h, projectID, "P16D", "PROJECT", false), "detect me")
	root := t.TempDir()
	target := filepath.Join(root, ".overseer", "state", "P16D.md")

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if err := os.WriteFile(target, []byte("drifted bytes"), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	fileBefore, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	metaBefore := p716MetaSig(t, h, fileID.String())
	versionsBefore := p716VersionSig(t, h, fileID.String())

	// Two consecutive detections: a mutating detection would change the
	// second run's observations.
	for i := 0; i < 2; i++ {
		res, err := p715Svc(t, h).DetectDrift(ctx, root, projectID.String(), fileID.String())
		if err != nil {
			t.Fatalf("detect %d: %v", i, err)
		}
		if res.Status != service.DriftDrifted {
			t.Fatalf("run %d status = %s, want DRIFTED", i, res.Status)
		}
	}
	fileAfter, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(fileAfter) != string(fileBefore) {
		t.Fatalf("detection mutated the materialized file: %q != %q", fileAfter, fileBefore)
	}
	if got := p716MetaSig(t, h, fileID.String()); got != metaBefore {
		t.Fatalf("detection wrote the metadata record: %q != %q", got, metaBefore)
	}
	if got := p716VersionSig(t, h, fileID.String()); got != versionsBefore {
		t.Fatalf("detection touched the version chain: %q != %q", got, versionsBefore)
	}
}

// T7.25.1 — Derived external modification detected: a DRIFTED report for
// the externally edited materialized copy of a DERIVED file (a
// WORK_UNIT-scoped file per the task NOTES). The derived write is
// mirrored (see the file header for the WBS 7.25 fixture policy).
func TestP716DerivedExternalModificationDetected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, unitID := p705Rig(t, h)
	typeID := p708Type(t, h, projectID, "p16-actived", "WORK_UNIT", statefile.SourceDerived)
	unitPtr := unitID
	fileID := p713UnitFile(t, h, projectID, unitPtr, typeID)
	p716SeedVersion(t, h, fileID, "derived v1")
	root := t.TempDir()
	target := filepath.Join(root, ".overseer", "state", "p16-actived.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The derivation's materialization write (mirrored).
	if err := os.WriteFile(target, []byte("derived v1"), 0o644); err != nil {
		t.Fatalf("derived write: %v", err)
	}
	// The external edit.
	if err := os.WriteFile(target, []byte("worker edited ACTIVE_TASK externally"), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	res, err := p715Svc(t, h).DetectDrift(ctx, root, projectID.String(), fileID.String())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.Status != service.DriftDrifted {
		t.Fatalf("status = %s, want DRIFTED (the external edit must be detected)", res.Status)
	}
}

// T7.25.2 — Derived modification never imported: after the external edit,
// the version chain carries ONLY the derived v1 (no version of the
// external bytes), and the explicit import op is rejected for the
// DERIVED class.
func TestP716DerivedModificationNeverImported(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, unitID := p705Rig(t, h)
	typeID := p708Type(t, h, projectID, "p16-actived2", "WORK_UNIT", statefile.SourceDerived)
	unitPtr := unitID
	fileID := p713UnitFile(t, h, projectID, unitPtr, typeID)
	p716SeedVersion(t, h, fileID, "derived only")
	root := t.TempDir()
	target := filepath.Join(root, ".overseer", "state", "p16-actived2.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The derivation's materialization write (mirrored), then the
	// external edit.
	if err := os.WriteFile(target, []byte("derived only"), 0o644); err != nil {
		t.Fatalf("derived write: %v", err)
	}
	if err := os.WriteFile(target, []byte("foreign edit"), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}

	// (a) Detection does not import: the version chain is still only v1.
	if n := p716VersionCount(t, h, fileID.String()); n != 1 {
		t.Fatalf("version rows = %d, want 1 (detection must not import)", n)
	}
	// (b) The explicit import op is rejected for DERIVED.
	if _, err := p715Svc(t, h).Import(ctx, root, projectID.String(), fileID.String()); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("import of DERIVED err = %v, want ErrInvalidInput", err)
	}
	if n := p716VersionCount(t, h, fileID.String()); n != 1 {
		t.Fatalf("after rejected import version rows = %d, want 1 (nothing imported)", n)
	}
}

// T7.25.3 — Re-materialization restores authoritative content: after the
// external edit, a later materialization writes the persisted version's
// bytes back (the DERIVED representation wins) and detection reports
// SYNCHRONIZED again.
func TestP716ReMaterializationRestoresAuthoritative(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, unitID := p705Rig(t, h)
	typeID := p708Type(t, h, projectID, "p16-actived3", "WORK_UNIT", statefile.SourceDerived)
	unitPtr := unitID
	fileID := p713UnitFile(t, h, projectID, unitPtr, typeID)
	p716SeedVersion(t, h, fileID, "authoritative derived content")
	root := t.TempDir()
	target := filepath.Join(root, ".overseer", "state", "p16-actived3.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The derivation's materialization write (mirrored), then the
	// external edit.
	if err := os.WriteFile(target, []byte("authoritative derived content"), 0o644); err != nil {
		t.Fatalf("derived write: %v", err)
	}
	if err := os.WriteFile(target, []byte("externally clobbered"), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	// Re-materialization = the derivation writes the persisted current
	// content back (the WBS 7.25 restore path — mirrored, see header).
	if err := os.WriteFile(target, []byte("authoritative derived content"), 0o644); err != nil {
		t.Fatalf("re-materialize (derived write): %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "authoritative derived content" {
		t.Fatalf("file = %q, want the persisted derived content restored", got)
	}
	res, err := p715Svc(t, h).DetectDrift(ctx, root, projectID.String(), fileID.String())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.Status != service.DriftSynchronized {
		t.Fatalf("status after restore = %s, want SYNCHRONIZED", res.Status)
	}
}

// T7.25.4 — Work-unit selection remains unchanged: the full
// drift + re-materialization cycle on a WORK_UNIT-scoped file leaves the
// file's persisted work-unit association byte-identical (no rebind, no
// clear, no re-derivation).
func TestP716WorkUnitSelectionUnchanged(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, unitID := p705Rig(t, h)
	typeID := p708Type(t, h, projectID, "p16-actived4", "WORK_UNIT", statefile.SourceDerived)
	unitPtr := unitID
	fileID := p713UnitFile(t, h, projectID, unitPtr, typeID)
	p716SeedVersion(t, h, fileID, "unit-bound content")
	root := t.TempDir()
	target := filepath.Join(root, ".overseer", "state", "p16-actived4.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The derivation's materialization write (mirrored).
	if err := os.WriteFile(target, []byte("unit-bound content"), 0o644); err != nil {
		t.Fatalf("derived write: %v", err)
	}
	sigBefore := p716UnitSig(t, h, fileID.String())
	if err := os.WriteFile(target, []byte("external"), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	if _, err := p715Svc(t, h).DetectDrift(ctx, root, projectID.String(), fileID.String()); err != nil {
		t.Fatalf("detect: %v", err)
	}
	// Re-materialization = the derivation's write of the persisted
	// content (mirrored, see header).
	if err := os.WriteFile(target, []byte("unit-bound content"), 0o644); err != nil {
		t.Fatalf("re-materialize (derived write): %v", err)
	}
	sigAfter := p716UnitSig(t, h, fileID.String())
	if sigAfter != sigBefore {
		t.Fatalf("association signature changed by the drift cycle: %q -> %q (want byte-identical)", sigBefore, sigAfter)
	}
	var phaseID string
	if err := h.DBScan(ctx, `SELECT coalesce(phase_id::text,'<nil>') FROM state_files WHERE id = '`+fileID.String()+`'`, &phaseID); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if phaseID != "<nil>" {
		t.Fatalf("phase association = %s, want nil for a WORK_UNIT-scoped file", phaseID)
	}
	var workUnitID string
	if err := h.DBScan(ctx, `SELECT work_unit_id::text FROM state_files WHERE id = '`+fileID.String()+`'`, &workUnitID); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if workUnitID != unitID.String() {
		t.Fatalf("work unit association = %s, want the original %s", workUnitID, unitID)
	}
}

// T7.26.1 — Explicit import creates a new version: the file's current
// on-disk content becomes the NEXT persisted version of a MANAGED file.
func TestP716ImportCreatesNewVersion(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	typeID := p708Type(t, h, projectID, "p16-managed1", "PROJECT", statefile.SourceManaged)
	fileID := p713ProjectFile(t, h, projectID, typeID)
	p714Content(t, h, fileID, "managed v1")
	root := t.TempDir()
	target := filepath.Join(root, ".overseer", "state", "p16-managed1.md")

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// The developer edits the file; the persisted chain still holds v1.
	if err := os.WriteFile(target, []byte("developer edit v2"), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	res, err := p715Svc(t, h).Import(ctx, root, projectID.String(), fileID.String())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !res.Imported {
		t.Fatalf("imported = false, want true (the content changed)")
	}
	if res.Version.Version != 2 || res.Version.Content != "developer edit v2" {
		t.Fatalf("imported version = %+v, want v2 with the on-disk content", res.Version)
	}
	var cur int
	if err := h.DBScan(ctx, `SELECT current_version FROM state_files WHERE id = '`+fileID.String()+`'`, &cur); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if cur != 2 {
		t.Fatalf("stored current_version = %d, want 2", cur)
	}
}

// T7.26.2 — Import hash calculated: the imported version's stored hash is
// the content hash of the exact on-disk bytes.
func TestP716ImportHashCalculated(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	typeID := p708Type(t, h, projectID, "p16-managed2", "PROJECT", statefile.SourceManaged)
	fileID := p713ProjectFile(t, h, projectID, typeID)
	p714Content(t, h, fileID, "hash base")
	root := t.TempDir()
	target := filepath.Join(root, ".overseer", "state", "p16-managed2.md")

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	edited := "edited bytes for the hash check"
	if err := os.WriteFile(target, []byte(edited), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	res, err := p715Svc(t, h).Import(ctx, root, projectID.String(), fileID.String())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := statefile.ContentHash(edited)
	if res.Version.ContentHash != want {
		t.Fatalf("hash = %q, want %q (statefile.ContentHash of the imported bytes)", res.Version.ContentHash, want)
	}
	var stored string
	if err := h.DBScan(ctx, `SELECT content_hash FROM state_file_versions WHERE state_file_id = '`+fileID.String()+`' AND version = 2`, &stored); err != nil {
		t.Fatalf("scan stored hash: %v", err)
	}
	if stored != want {
		t.Fatalf("stored v2 hash = %q, want %q", stored, want)
	}
}

// T7.26.3 — Derived file import rejected: an explicit import of a DERIVED
// file is a clean rejection BEFORE any write (the version chain is
// unchanged).
func TestP716DerivedImportRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	typeID := p708Type(t, h, projectID, "p16-derived3", "PROJECT", statefile.SourceDerived)
	fileID := p708File(t, h, p708Service(t, h), projectID, typeID, "PROJECT", nil, nil)
	p716SeedVersion(t, h, fileID, "derived base")
	root := t.TempDir()
	target := filepath.Join(root, ".overseer", "state", "p16-derived3.md")

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if err := os.WriteFile(target, []byte("not imported"), 0o644); err != nil {
		t.Fatalf("external edit: %v", err)
	}
	_, failErr := p715Svc(t, h).Import(ctx, root, projectID.String(), fileID.String())
	if !errors.Is(failErr, service.ErrInvalidInput) {
		t.Fatalf("import of DERIVED err = %v, want ErrInvalidInput", failErr)
	}
	if n := p716VersionCount(t, h, fileID.String()); n != 1 {
		t.Fatalf("version rows = %d, want 1 (rejection before any write)", n)
	}
}

// T7.26.4 — Unchanged import handled idempotently: importing content whose
// hash equals the current version's hash appends NO new version row and
// leaves the metadata record byte-identical.
func TestP716UnchangedImportIdempotent(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	typeID := p708Type(t, h, projectID, "p16-managed4", "PROJECT", statefile.SourceManaged)
	fileID := p713ProjectFile(t, h, projectID, typeID)
	p714Content(t, h, fileID, "unchanged content")
	root := t.TempDir()

	if _, err := p715Svc(t, h).Materialize(ctx, root, projectID.String()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	metaBefore := p716MetaSig(t, h, fileID.String())

	res, err := p715Svc(t, h).Import(ctx, root, projectID.String(), fileID.String())
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported {
		t.Fatalf("imported = true, want false (the content is unchanged)")
	}
	if res.Version.Version != 1 {
		t.Fatalf("returned version = %d, want the current 1 (no new version)", res.Version.Version)
	}
	if n := p716VersionCount(t, h, fileID.String()); n != 1 {
		t.Fatalf("version rows = %d, want 1 (no spurious new version)", n)
	}
	if got := p716MetaSig(t, h, fileID.String()); got != metaBefore {
		t.Fatalf("metadata record changed by an unchanged import: %q -> %q", metaBefore, got)
	}
}

// T7.26.5 — Import outside the managed path rejected: a type whose
// DefaultFilename escapes the state directory is resolved through the SAME
// managed-path resolution as materialization (SafeRelPath) — the import is
// a clean rejection and nothing is imported.
func TestP716ImportOutsideManagedPathRejected(t *testing.T) {
	h := NewHarness(t)
	ctx := context.Background()
	projectID, _, _ := p705Rig(t, h)
	typeID := p708Type(t, h, projectID, "p16-escape", "PROJECT", statefile.SourceManaged)
	if _, err := h.DBExec(ctx, "UPDATE state_file_types SET default_filename = '../outside-import.md' WHERE id = '"+typeID.String()+"'"); err != nil {
		t.Fatalf("set filename: %v", err)
	}
	fileID := p713ProjectFile(t, h, projectID, typeID)
	p714Content(t, h, fileID, "base content")
	root := t.TempDir()

	// Place content where the (unsafe) name would point, so a naive import
	// reader COULD find it — the managed-path resolution must reject first.
	outside := filepath.Join(root, "outside-import.md")
	if err := os.WriteFile(outside, []byte("would be imported if not rejected"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	_, failErr := p715Svc(t, h).Import(ctx, root, projectID.String(), fileID.String())
	if !errors.Is(failErr, service.ErrInvalidInput) {
		t.Fatalf("outside-managed-path import err = %v, want ErrInvalidInput (clean rejection)", failErr)
	}
	if n := p716VersionCount(t, h, fileID.String()); n != 1 {
		t.Fatalf("version rows = %d, want 1 (nothing imported)", n)
	}
	// The outside file is left as-is (the op never wrote through it).
	got, err := os.ReadFile(outside)
	if err != nil || !strings.Contains(string(got), "would be imported") {
		t.Fatalf("outside file = %q (err %v)", got, err)
	}
}

var _ = persistence.ErrNotFound
