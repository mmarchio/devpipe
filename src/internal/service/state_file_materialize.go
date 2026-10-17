// P7-14 (WBS 7.20/7.21) + P7-15 (WBS 7.22/7.23) + P7-16 (WBS 7.24/7.25/7.26):
// state-file materialization service, atomic materialized writes,
// materialized-file metadata, filesystem drift detection, the DERIVED
// drift policy, and the explicit managed-file import.
//
// The StateFileMaterializeService is the OPTIONAL write path from the
// authoritative persisted state (PostgreSQL) into the configured project
// working directory: a project's state files are materialized to
// <root>/.overseer/state/<type's DefaultFilename> (WBS 7.20 layout). The
// materialized files are projections — the persisted content version
// (P7-06) stays the source of truth, never the other way around.
//
// File-selection policy (stated per the task NOTES and pinned by the
// integration tests in internal/integration): the files materialized for
// a project are its PROJECT-scoped state files whose ENABLED type has a
// non-empty DefaultFilename and whose CURRENT persisted content version
// exists. A file of a type with no filename, or a file without content,
// is skipped (the op continues with the remaining files) — a file-level
// skip, not an op failure. An IO error (read/IO) is reported cleanly as
// an op error identifying the file (T7.20.5).
//
// Path safety (WBS 7.21) is delegated to statefile.SafeRelPath: a
// configured filename is a managed RELATIVE name under the state
// directory — .. traversal and absolute paths are rejected, and a symlink
// component is resolved with containment re-verified AFTER resolution, so
// a symlink pointing outside the state directory is rejected
// (T7.21.1-T7.21.5).
//
// Atomic writes (WBS 7.22, P7-15): each materialized file is written to a
// temporary file in the SAME directory as the target, fsynced and closed,
// then renamed over the target. A crash mid-write leaves the PREVIOUS
// file intact (the rename is the only mutation of the target); the
// temporary file is removed on failure where possible. The write step is
// a service field (writeFile) defaulting to atomicWriteFile — the
// failure-injection seam used by T7.22.2 (force an IO error on the temp
// write).
//
// Metadata (WBS 7.23, P7-15): when the optional metadata repository is
// wired (WithMaterializedMetadata), each successful write persists one
// CURRENT record per state file (state_file_id, version, content_hash,
// path, materialized_at) — updated in place on re-materialization, never
// duplicated. The record is a PROJECTION for drift detection (P7-16); it
// never overrides the persisted version.
//
// Drift detection (WBS 7.24, P7-16): DetectDrift is a READ op that
// compares the on-disk file's content hash against the PERSISTED current
// version's hash (PostgreSQL stays authoritative — the metadata record is
// only a location hint, never the comparison source). A matching hash
// reports SYNCHRONIZED, a differing hash DRIFTED, a missing file at the
// recorded path MISSING, and a file without a current version
// NOT_MATERIALIZED (a file-level status, not an error). Detection is
// strictly non-mutating: no metadata upsert, no version write, no file
// write (T7.24.4 pins this by byte-signature).
//
// Derived drift policy (WBS 7.25): for DERIVED files the persisted
// representation wins — an external edit of the materialized copy is
// detected as drift and is NEVER imported into the version chain (the
// only writer for a DERIVED class is re-derivation / the P7-10-style
// controlled append path); a later materialization restores the
// authoritative content. The drift/re-materialization cycle touches
// neither the file's associations (work-unit selection is unchanged) nor
// the version chain.
//
// Managed import (WBS 7.26): Import is the EXPLICIT import op for
// selected MANAGED/USER state files — the file's current on-disk content
// becomes a NEW persisted version (via the P7-06 optimistic
// UpdateContent path, so the version's hash/token estimate follow the
// existing recipe). "Selected MANAGED/USER" is consumed verbatim from
// P7-04: the type's SourceClass must pass
// statefile.ControlledUpdateAllowed (T7.16.4) — DERIVED is rejected
// BEFORE any write (T7.26.3). Importing content whose hash EQUALS the
// current version's hash is an idempotent no-op: no new version row, the
// current version is returned (T7.26.4). The file is resolved through the
// SAME managed-path resolution as materialization (SafeRelPath under the
// state directory + the type's DefaultFilename), so a path outside the
// managed state directory is a clean rejection (T7.26.5) — a file never
// gets imported merely because it changed.
package service

import (
	"context"
	"fmt"
	"os"
	"overseer/internal/bounds"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"overseer/internal/faults"
	"overseer/internal/persistence"
	"overseer/internal/statefile"
)

// MaterializeResult is the per-file outcome of a materialization op.
type MaterializeResult struct {
	StateFileID string
	// Written is the absolute path the file was written to.
	Written string
	// Skipped is the reason the file was skipped (no filename on the
	// type, no content version); empty when the file was written.
	Skipped string
}

// DriftStatus is the outcome of a drift detection for one materialized
// state file (WBS 7.24).
type DriftStatus string

const (
	// DriftSynchronized means the on-disk bytes' content hash equals the
	// persisted current version's hash.
	DriftSynchronized DriftStatus = "SYNCHRONIZED"
	// DriftDrifted means a file exists at the materialization path but its
	// content hash differs from the persisted current version (an
	// external edit — the persisted version stays authoritative).
	DriftDrifted DriftStatus = "DRIFTED"
	// DriftMissing means no file exists at the materialization path.
	DriftMissing DriftStatus = "MISSING"
	// DriftNotMaterialized means the file has no current content version
	// to compare against (a file-level status, not an error).
	DriftNotMaterialized DriftStatus = "NOT_MATERIALIZED"
)

// DriftResult is the per-file outcome of DetectDrift.
type DriftResult struct {
	// Path is the materialization path the file was checked at.
	Path string
	// Status is the drift status for the file.
	Status DriftStatus
	// CurrentVersion is the persisted current version number (0 for
	// NOT_MATERIALIZED).
	CurrentVersion int
	// ContentHash is the persisted current version's content hash ("" for
	// NOT_MATERIALIZED).
	ContentHash string
	// FSHash is the on-disk content hash ("" when no file was read).
	FSHash string
}

// ImportResult is the outcome of an explicit managed-file import (WBS 7.26).
type ImportResult struct {
	// Imported is false when the on-disk content already matched the
	// current version's hash (the idempotent no-op, T7.26.4); the
	// returned version is then the current version, not a new one.
	Imported bool
	// Version is the imported version (Imported=true) or the current
	// version (Imported=false).
	Version StateFileVersionView
}

// StateFileMaterializeService materializes a project's persisted state
// files into the configured working directory (WBS 7.20).
type StateFileMaterializeService struct {
	types    persistence.StateFileTypeRepository
	files    persistence.StateFileRepository
	versions persistence.StateFileVersionRepository
	// meta is the OPTIONAL materialized-file metadata repository (WBS
	// 7.23, P7-15). Nil = no metadata persistence (unit-scope
	// construction keeps its degraded path); when wired, every
	// successful write upserts the file's current record.
	meta persistence.MaterializedStateFileRepository
	// WriteFile is the atomic-write step (P7-15, WBS 7.22): default is
	// atomicWriteFile; tests may swap it to force a failure on the temp
	// write (the T7.22.2 failure-injection seam).
	WriteFile func(path string, data []byte) error
}

// NewStateFileMaterializeService wires the materialization service to the
// P7-05/P7-06 repositories (the persisted content source of truth).
func NewStateFileMaterializeService(
	types persistence.StateFileTypeRepository,
	files persistence.StateFileRepository,
	versions persistence.StateFileVersionRepository,
) *StateFileMaterializeService {
	return &StateFileMaterializeService{
		types:     types,
		files:     files,
		versions:  versions,
		WriteFile: atomicWriteFile,
	}
}

// WithMaterializedMetadata wires the OPTIONAL metadata repository (WBS
// 7.23, P7-15): after each successful file write the op upserts the
// file's current record (version, content hash, path, UTC time). Nil
// keeps the no-metadata behavior (additive seam — prior-phase
// construction is unchanged).
func (s *StateFileMaterializeService) WithMaterializedMetadata(repo persistence.MaterializedStateFileRepository) *StateFileMaterializeService {
	if repo == nil {
		return s
	}
	s.meta = repo
	return s
}

// atomicWriteFile writes data to path atomically (WBS 7.22 / 18.55): a
// temporary file in the same directory, written fully, fsynced, closed,
// renamed over the target, then the directory fsynced so the rename itself
// is durable. On any failure the temporary file is removed where possible
// and the target is left untouched. A process killed at any point leaves
// either the original or the complete replacement at path — never a
// partial file (the rename is the sole target mutation); at worst a stale
// temporary file remains, which nothing reads and cleanStaleTemps removes.
func atomicWriteFile(path string, data []byte) error {
	ctx := context.Background()
	if err := faults.Hit(ctx, faults.FSWrite); err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, materializeTempPattern)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := faults.Hit(ctx, faults.FSRename); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp file: %w", err)
	}
	if d, err := os.Open(dir); err == nil {
		// Best effort: some filesystems refuse fsync on directories.
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// materializeTempPattern names atomicWriteFile's temporary files.
const materializeTempPattern = ".materialize-*.tmp"

// staleTempAge is how old a leftover temporary file must be before it is
// removed: far longer than any in-progress write.
const staleTempAge = 10 * time.Minute

// cleanStaleTemps removes temporary files a killed writer left in dir
// that are older than maxAge (a younger one may belong to a live writer).
func cleanStaleTemps(dir string, maxAge time.Duration) int {
	matches, _ := filepath.Glob(filepath.Join(dir, materializeTempPattern))
	n := 0
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && time.Since(st.ModTime()) >= maxAge {
			if os.Remove(m) == nil {
				n++
			}
		}
	}
	return n
}

// Materialize writes the project's state files to
// <root>/.overseer/state/ (WBS 7.20): the state namespace is created
// where missing (T7.20.2), each materialized file carries the content of
// its CURRENT persisted version (T7.20.3), and existing unrelated files
// under the root are left byte-identical (T7.20.4 — the op only writes
// into the state namespace). A per-file skip (no filename / no content)
// is reported in the result, not as an error; an IO failure is a clean
// op error identifying the file (T7.20.5).
//
// P7-15: every write goes through the atomic path (temp + fsync +
// rename, WBS 7.22), and each successful write upserts the file's
// materialized-file metadata record (WBS 7.23) when the repository is
// wired.
func (s *StateFileMaterializeService) Materialize(ctx context.Context, root, projectID string) ([]MaterializeResult, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	records, err := s.files.ListByProject(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}
	stateDir := filepath.Join(root, ".overseer", "state")
	var out []MaterializeResult
	for _, rec := range records {
		if rec.Scope != string(statefile.ScopeProject) {
			continue // only PROJECT-scoped files live at the project root
		}
		result := MaterializeResult{StateFileID: rec.ID.String()}
		typeRec, err := s.types.Get(ctx, rec.TypeID)
		if err != nil {
			return nil, translate(err)
		}
		if !typeRec.Enabled || typeRec.DefaultFilename == "" {
			result.Skipped = "no default filename on type"
			out = append(out, result)
			continue
		}
		ver, err := s.versions.GetCurrentVersion(ctx, rec.ID)
		if err != nil {
			return nil, translate(err)
		}
		if ver == nil {
			result.Skipped = "no content version"
			out = append(out, result)
			continue
		}
		target, err := statefile.SafeRelPath(stateDir, typeRec.DefaultFilename)
		if err != nil {
			// Unsafe path (WBS 7.21): reported cleanly, nothing written.
			return nil, fmt.Errorf("materialize file %s: %w", typeRec.DefaultFilename, err)
		}
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			return nil, fmt.Errorf("materialize file %s: create state dir: %w", typeRec.DefaultFilename, err)
		}
		cleanStaleTemps(stateDir, staleTempAge)
		if err := s.WriteFile(target, []byte(ver.Content)); err != nil {
			return nil, fmt.Errorf("materialize file %s: %w", typeRec.DefaultFilename, err)
		}
		if s.meta != nil {
			// WBS 7.23: persist what was last materialized — the WRITTEN
			// version and the content hash of the written bytes. The
			// record is a projection for drift detection; it never
			// overrides the persisted version.
			if err := s.meta.Upsert(ctx, &persistence.MaterializedStateFileRecord{
				StateFileID:    rec.ID,
				Version:        ver.Version,
				ContentHash:    ver.ContentHash,
				Path:           target,
				MaterializedAt: time.Now().UTC(),
			}); err != nil {
				return nil, fmt.Errorf("materialize file %s: record metadata: %w", typeRec.DefaultFilename, translate(err))
			}
		}
		result.Written = target
		out = append(out, result)
	}
	return out, nil
}

// resolveMaterializedPath derives the managed materialization path for a
// file (the SAME resolution Materialize uses — WBS 7.21): the type's
// DefaultFilename under <root>/.overseer/state/, validated by
// statefile.SafeRelPath. It returns the path and the resolved type record.
// A type without a DefaultFilename yields ("", typeRec, nil) — the file
// is simply not materializable, not an error.
func (s *StateFileMaterializeService) resolveMaterializedPath(ctx context.Context, root string, rec *persistence.StateFileRecord) (string, *persistence.StateFileTypeRecord, error) {
	typeRec, err := s.types.Get(ctx, rec.TypeID)
	if err != nil {
		return "", nil, translate(err)
	}
	if typeRec.DefaultFilename == "" {
		return "", typeRec, nil
	}
	stateDir := filepath.Join(root, ".overseer", "state")
	target, err := statefile.SafeRelPath(stateDir, typeRec.DefaultFilename)
	if err != nil {
		// The type's configured filename is not a safe managed name (WBS
		// 7.21) — a clean input rejection, not an infrastructure failure.
		return "", typeRec, fmt.Errorf("%w: type %s filename %q is not a safe managed path: %v", ErrInvalidInput, typeRec.Key, typeRec.DefaultFilename, err)
	}
	return target, typeRec, nil
}

// DetectDrift is the filesystem drift-detection READ (WBS 7.24): it
// compares the on-disk file's content hash against the PERSISTED current
// version's hash and reports SYNCHRONIZED / DRIFTED / MISSING /
// NOT_MATERIALIZED. The persisted version (PostgreSQL) is the comparison
// source — the metadata record is only consulted for the recorded
// location (falling back to the derived managed path when absent), and is
// never a source of truth. The op is strictly non-mutating: it writes no
// metadata record, appends no version, and touches no file (T7.24.4).
func (s *StateFileMaterializeService) DetectDrift(ctx context.Context, root, projectID, stateFileID string) (*DriftResult, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	fileID, err := uuid.Parse(stateFileID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rec, err := s.files.Get(ctx, fileID)
	if err != nil {
		return nil, translate(err)
	}
	if rec.ProjectID != pid {
		return nil, ErrInvalidReference
	}
	// The PERSISTED current version is the comparison source
	// (PostgreSQL stays authoritative).
	ver, err := s.versions.GetCurrentVersion(ctx, fileID)
	if err != nil {
		return nil, translate(err)
	}
	if ver == nil {
		return &DriftResult{Status: DriftNotMaterialized}, nil
	}
	path, _, err := s.resolveMaterializedPath(ctx, root, rec)
	if err != nil {
		return nil, err
	}
	if path == "" {
		// No materialization path exists for this type: the file can be
		// neither synchronized nor missing — it was never materializable.
		return &DriftResult{Status: DriftNotMaterialized, CurrentVersion: ver.Version, ContentHash: ver.ContentHash}, nil
	}
	// Location preference: the recorded materialization path when the
	// metadata record is wired and present, else the derived managed
	// path. Both are the same managed resolution; the record is a
	// projection, never the comparison source.
	if s.meta != nil {
		if mrec, err := s.meta.Get(ctx, fileID); err == nil && mrec != nil && mrec.Path != "" {
			path = mrec.Path
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &DriftResult{Path: path, Status: DriftMissing, CurrentVersion: ver.Version, ContentHash: ver.ContentHash}, nil
		}
		return nil, fmt.Errorf("detect drift file %s: read: %w", filepath.Base(path), err)
	}
	status := DriftSynchronized
	fsHash := statefile.ContentHash(string(b))
	if fsHash != ver.ContentHash {
		status = DriftDrifted
	}
	return &DriftResult{Path: path, Status: status, CurrentVersion: ver.Version, ContentHash: ver.ContentHash, FSHash: fsHash}, nil
}

// Import is the EXPLICIT managed-file import op (WBS 7.26): the file's
// current on-disk content becomes a NEW persisted version. The policy is
// consumed verbatim from P7-04 (WBS 7.16 T7.16.4): only classes with a
// controlled update path (MANAGED/USER/EXTERNAL) are importable — a
// DERIVED type is rejected BEFORE any write (T7.26.3; its content is
// re-derived, never imported). Content whose hash equals the current
// version's hash is handled idempotently: no new version row, the
// current version is returned with Imported=false (T7.26.4). The file is
// resolved through the SAME managed-path resolution as materialization
// (SafeRelPath under the state directory + the type's DefaultFilename),
// so a path outside the managed state directory is a clean rejection
// (T7.26.5) — nothing is ever imported merely because a file changed.
//
// The new version is appended through the P7-06 optimistic
// UpdateContent path (expectedVersion = the just-read current version),
// so the stored hash/token estimate follow the existing recipe; when the
// metadata repository is wired the record is updated to the persisted
// current version afterward (the record is a projection and stays
// consistent without being a source of truth).
func (s *StateFileMaterializeService) Import(ctx context.Context, root, projectID, stateFileID string) (*ImportResult, error) {
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	fileID, err := uuid.Parse(stateFileID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rec, err := s.files.Get(ctx, fileID)
	if err != nil {
		return nil, translate(err)
	}
	if rec.ProjectID != pid {
		return nil, ErrInvalidReference
	}
	path, typeRec, err := s.resolveMaterializedPath(ctx, root, rec)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, fmt.Errorf("%w: type %s has no materialization path", ErrInvalidInput, typeRec.Key)
	}
	// "Selected MANAGED/USER" = the type's SourceClass passes the
	// P7-04 controlled-update policy (T7.16.4) — DERIVED is rejected
	// BEFORE any write (T7.26.3).
	if !statefile.ControlledUpdateAllowed(statefile.SourceClass(typeRec.Source)) {
		return nil, fmt.Errorf("%w: %s content requires a derivation re-run (import rejected)", ErrInvalidInput, typeRec.Key)
	}
	// Size is checked before reading: an oversized on-disk file is never
	// loaded into memory (WBS 18.96).
	if fi, err := os.Stat(path); err == nil && fi.Size() > int64(DefaultMaxContentBytes) {
		return nil, fmt.Errorf("%w: file %s (%d bytes > limit %d)", ErrContentLimitExceeded, rec.ID, fi.Size(), DefaultMaxContentBytes)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: file %s is not materialized at its managed path", ErrInvalidInput, filepath.Base(path))
		}
		return nil, fmt.Errorf("import file %s: read: %w", filepath.Base(path), err)
	}
	cur, err := s.versions.GetCurrentVersion(ctx, fileID)
	if err != nil {
		return nil, translate(err)
	}
	if cur == nil {
		return nil, fmt.Errorf("%w: file has no content version to import against", ErrInvalidInput)
	}
	// T7.26.4: unchanged content (same hash as the current version) is an
	// idempotent no-op — no spurious new version row.
	if statefile.ContentHash(string(b)) == cur.ContentHash {
		return &ImportResult{Imported: false, Version: toVersionView(cur)}, nil
	}
	if len(b) > DefaultMaxContentBytes {
		return nil, fmt.Errorf("%w: file %s (%d bytes > limit %d)", ErrContentLimitExceeded, rec.ID, len(b), DefaultMaxContentBytes)
	}
	if err := bounds.Check(bounds.StateFile, string(b)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	stored, err := s.versions.UpdateContent(ctx, fileID, string(b), cur.Version)
	if err != nil {
		return nil, translate(err)
	}
	if s.meta != nil {
		// Keep the projection consistent with the persisted current
		// version (the import advanced it).
		if err := s.meta.Upsert(ctx, &persistence.MaterializedStateFileRecord{
			StateFileID:    fileID,
			Version:        stored.Version,
			ContentHash:    stored.ContentHash,
			Path:           path,
			MaterializedAt: time.Now().UTC(),
		}); err != nil {
			return nil, fmt.Errorf("import file %s: record metadata: %w", filepath.Base(path), translate(err))
		}
	}
	return &ImportResult{Imported: true, Version: toVersionView(stored)}, nil
}
