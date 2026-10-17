// P7-23 (WBS 7.37/7.40): the context-preview read op and the snapshot-read
// surface.
//
// The ContextPreviewService is the non-persisting, non-mutating half of the
// snapshot pipeline (WBS 7.37): it reports WHAT context WOULD be captured
// for the project's current runtime position — the current state, the
// resolved role, the role's required state files each at its CURRENT
// persisted version (version + hash + token estimate), the required files
// that are MISSING (no file at the resolved scope), and the total estimated
// context token count — WITHOUT writing an execution snapshot row and
// WITHOUT altering the runtime row (T7.37.4/T7.37.5).
//
// It composes the SAME read pipeline the P7-18 snapshot-creation service
// uses (role resolution, requirement + scope resolution, version capture) —
// no parallel assembly path — and reuses the P7-18 resolveRole + P7-13
// scope-resolution ops, the P7-19/20 per-file version/hash + token estimate,
// and the role-snapshot read.
//
// The snapshot-read ops (WBS 7.40) list the project's persisted historical
// snapshots and retrieve one by id (404 when unknown); the surface is
// read-only (no mutation endpoint).
package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/statefile"
)

// ContextPreviewService previews the execution context that WOULD be
// captured (WBS 7.37) and reads persisted snapshots (WBS 7.40).
type ContextPreviewService struct {
	// Preview composition (mirrors the P7-18 creation pipeline):
	runtime      persistence.RuntimeStateRepository
	states       persistence.WorkflowStateRepository
	assignments  persistence.StateRoleAssignmentRepository
	roles        persistence.RoleRepository
	roleSvc      *RoleService
	scopeRes     *StateFileScopeResolutionService
	fileVersions persistence.StateFileVersionRepository
	// Snapshot reads:
	snapshots persistence.ContextSnapshotRepository
	txPool    *pgxpool.Pool
}

// NewContextPreviewService wires the preview + snapshot-read surface.
func NewContextPreviewService(
	runtime persistence.RuntimeStateRepository,
	states persistence.WorkflowStateRepository,
	assignments persistence.StateRoleAssignmentRepository,
	roles persistence.RoleRepository,
	roleSvc *RoleService,
	scopeRes *StateFileScopeResolutionService,
) *ContextPreviewService {
	return &ContextPreviewService{
		runtime: runtime, states: states,
		assignments: assignments, roles: roles,
		roleSvc: roleSvc, scopeRes: scopeRes,
	}
}

// WithFileVersions wires the state-file version repository (required for
// the preview's version/hash + token-estimate capture).
func (s *ContextPreviewService) WithFileVersions(repo persistence.StateFileVersionRepository) *ContextPreviewService {
	s.fileVersions = repo
	return s
}

// WithSnapshots wires the snapshot repository (required for the snapshot
// read ops).
func (s *ContextPreviewService) WithSnapshots(repo persistence.ContextSnapshotRepository) *ContextPreviewService {
	s.snapshots = repo
	return s
}

// WithTxPool wires the transaction pool so the preview's reads run in one
// consistent view (the nil-pool path runs sequentially).
func (s *ContextPreviewService) WithTxPool(pool *pgxpool.Pool) *ContextPreviewService {
	s.txPool = pool
	return s
}

// ScopePreviewFile is one required state file as it would be captured (WBS
// 7.37): the type key + scope, and — when the file has a persisted version
// — its current version, content hash and approximate token estimate. A
// missing required file is identified by TypeKey + Scope with Version 0.
type ScopePreviewFile struct {
	TypeKey string
	Scope   statefile.StateFileScope

	// Present is false for a required type with no file at its resolved
	// scope (reported in the preview's MissingFiles set).
	Present bool

	Version       int
	Hash          string
	TokenEstimate int
}

// ScopePreview is the context-preview read model (WBS 7.37): the current
// state key, the resolved role key, the required state files with their
// current versions, the required files that are missing, and the total
// estimated context token count.
type ScopePreview struct {
	StateKey string
	RoleKey  string

	Files        []ScopePreviewFile
	MissingFiles []string

	EstimatedTokens int
}

// Preview reports the context that WOULD be captured for the project's
// current runtime position WITHOUT persisting a snapshot and WITHOUT
// altering the runtime (WBS 7.37). Failure classes: unknown project runtime
// (ErrNotFound) and no usable role for the current state (ErrNotFound). A
// missing required FILE does not fail the preview — it is reported in
// MissingFiles (T7.37.3), the preview's distinguishing property over the
// creation op (which the P7-13 semantics make fail).
func (s *ContextPreviewService) Preview(ctx context.Context, projectID string) (*ScopePreview, error) {
	if s.fileVersions == nil || s.roleSvc == nil || s.scopeRes == nil {
		return nil, ErrInfrastructure
	}
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	var out *ScopePreview
	var rerr error
	if s.txPool == nil {
		out, rerr = s.previewOnce(ctx, nil, pid)
	} else {
		rerr = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
			var e error
			out, e = s.previewOnce(ctx, tx, pid)
			return e
		})
	}
	if rerr != nil {
		return nil, rerr
	}
	return out, nil
}

// previewOnce performs one preview attempt on the given transaction (nil for
// the sequential path): read the runtime position, resolve state + role,
// resolve the required files (collecting the missing ones), and capture each
// present file's exact current version + hash + token estimate.
func (s *ContextPreviewService) previewOnce(ctx context.Context, tx persistence.Transaction, pid uuid.UUID) (*ScopePreview, error) {
	rt := s.runtime
	if tx != nil {
		rt = s.runtime.WithTx(tx)
	}
	rtRec, err := rt.Get(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}
	stRepo := s.states
	if tx != nil {
		stRepo = s.states.WithTx(tx)
	}
	st, err := stRepo.Get(ctx, rtRec.CurrentStateID)
	if err != nil {
		return nil, translate(err)
	}
	roleRec, err := s.resolveRole(ctx, tx, pid, st)
	if err != nil {
		return nil, err
	}
	roleSnap, err := s.roleSvc.assembleSnapshot(ctx, tx, roleRec)
	if err != nil {
		return nil, translate(err)
	}
	resolved, resErr := s.resolveRequiredScopedFiles(ctx, tx, pid)
	if resErr != nil {
		return nil, resErr
	}
	verRepo := s.fileVersions
	if tx != nil {
		verRepo = s.fileVersions.WithTx(tx)
	}
	preview := &ScopePreview{
		StateKey: st.Key,
		RoleKey:  roleSnap.RoleKey,
		Files:    make([]ScopePreviewFile, 0, len(resolved)),
	}
	for _, rsf := range resolved {
		entry := ScopePreviewFile{
			TypeKey: rsf.Type.Key,
			Scope:   statefile.StateFileScope(rsf.Type.Scope),
		}
		if rsf.File == nil {
			// A required type with no file at its scope: reported as
			// missing (T7.37.3) — the preview's distinguishing property.
			preview.MissingFiles = append(preview.MissingFiles, rsf.Type.Key)
			preview.Files = append(preview.Files, entry)
			continue
		}
		entry.Present = true
		if rsf.File.CurrentVersion > 0 {
			v, err := verRepo.GetVersion(ctx, rsf.File.ID, rsf.File.CurrentVersion)
			if err != nil {
				return nil, translate(err)
			}
			entry.Version = v.Version
			entry.Hash = v.ContentHash
			entry.TokenEstimate = statefile.TokenEstimate(v.Content)
			preview.EstimatedTokens += entry.TokenEstimate
		}
		preview.Files = append(preview.Files, entry)
	}
	// The role's own context contributes to the estimate the same way the
	// snapshot budget does (system prompt + assigned instructions).
	preview.EstimatedTokens += statefile.TokenEstimate(roleSnap.SystemPrompt.Content)
	for _, in := range roleSnap.Instructions {
		preview.EstimatedTokens += statefile.TokenEstimate(in.Content)
	}
	return preview, nil
}

// resolveRequiredScopedFiles runs the P7-13 scope-resolution op and, on a
// missing-required-file failure, re-resolves reporting the missing types as
// nil-file entries instead of failing. The other failure classes (a missing
// required context — no current phase/work unit) propagate.
func (s *ContextPreviewService) resolveRequiredScopedFiles(ctx context.Context, tx persistence.Transaction, pid uuid.UUID) ([]ResolvedStateFile, error) {
	var resolved []ResolvedStateFile
	var err error
	if tx != nil {
		resolved, err = s.scopeRes.ResolveStateFileScopesTx(ctx, tx, pid.String())
	} else {
		resolved, err = s.scopeRes.ResolveStateFileScopes(ctx, pid.String())
	}
	if err == nil {
		return resolved, nil
	}
	if errors.Is(err, ErrMissingStateFile) {
		return s.resolveReportingMissing(ctx, tx, pid)
	}
	return nil, translate(err)
}

// resolveReportingMissing resolves every required type individually, marking
// the missing ones as nil-file entries, so the preview's MissingFiles set is
// complete (not just the first missing type the strict op reports).
func (s *ContextPreviewService) resolveReportingMissing(ctx context.Context, tx persistence.Transaction, pid uuid.UUID) ([]ResolvedStateFile, error) {
	var categories []string
	var err error
	if tx != nil {
		categories, err = s.scopeRes.reqs.ResolveStateFileRequirementsTx(ctx, tx, pid.String())
	} else {
		categories, err = s.scopeRes.reqs.ResolveStateFileRequirements(ctx, pid.String())
	}
	if err != nil {
		return nil, translate(err)
	}
	byKey, err := s.scopeRes.TypeByRequirementKey(ctx, tx, pid)
	if err != nil {
		return nil, translate(err)
	}
	var out []ResolvedStateFile
	for _, category := range categories {
		rec, ok := byKey[category]
		if !ok || !rec.Enabled || !rec.Required {
			continue
		}
		file, lerr := s.scopeRes.LookupScopedFile(ctx, tx, pid, rec)
		if lerr != nil {
			if errors.Is(lerr, persistence.ErrNotFound) {
				out = append(out, ResolvedStateFile{Type: rec, File: nil})
				continue
			}
			return nil, translate(lerr)
		}
		out = append(out, ResolvedStateFile{Type: rec, File: file})
	}
	return out, nil
}

// resolveRole returns the role the preview carries — the SAME resolution
// rule as the P7-18 snapshot-creation op (stated there): the state's
// assigned role at the smallest ordinal, visiting in ordinal order and
// skipping disabled roles; a state with no assignments falls back to the
// project's smallest-key role; none usable is ErrNotFound.
func (s *ContextPreviewService) resolveRole(ctx context.Context, tx persistence.Transaction, pid uuid.UUID, st *persistence.WorkflowStateRecord) (*persistence.RoleRecord, error) {
	asgns := s.assignments
	if tx != nil {
		asgns = s.assignments.WithTx(tx)
	}
	roleRepo := s.roles
	if tx != nil {
		roleRepo = s.roles.WithTx(tx)
	}
	rows, err := asgns.ListByState(ctx, st.ID)
	if err != nil {
		return nil, translate(err)
	}
	for _, a := range rows {
		rec, err := roleRepo.Get(ctx, a.RoleID.String())
		if err != nil {
			if errors.Is(err, persistence.ErrNotFound) {
				continue
			}
			return nil, translate(err)
		}
		if rec.Enabled {
			return rec, nil
		}
	}
	list, err := roleRepo.ListByScope(ctx, &pid)
	if err != nil {
		return nil, translate(err)
	}
	var best *persistence.RoleRecord
	for _, r := range list {
		if !r.Enabled {
			continue
		}
		if best == nil || r.Key < best.Key {
			best = r
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

// ListSnapshots returns the project's persisted snapshots, oldest first (WBS
// 7.40). An unknown project returns an empty (non-nil) slice.
func (s *ContextPreviewService) ListSnapshots(ctx context.Context, projectID string) ([]*persistence.ContextSnapshotRecord, error) {
	if s.snapshots == nil {
		return nil, ErrInfrastructure
	}
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	recs, err := s.snapshots.ListByProject(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}
	return recs, nil
}

// GetSnapshot returns one persisted snapshot by id (WBS 7.40); a snapshot
// that belongs to a different project, or that is missing, is ErrNotFound
// (the project-scoped read contract).
func (s *ContextPreviewService) GetSnapshot(ctx context.Context, projectID, snapshotID string) (*persistence.ContextSnapshotRecord, error) {
	if s.snapshots == nil {
		return nil, ErrInfrastructure
	}
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	sid, err := uuid.Parse(snapshotID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	rec, err := s.snapshots.Get(ctx, sid)
	if err != nil {
		return nil, translate(err)
	}
	// Enforce scope in the service: a snapshot of another project is not
	// found under this project's URL (the established read contract).
	if rec.ProjectID != pid {
		return nil, ErrNotFound
	}
	return rec, nil
}
