// P7-18 (WBS 7.31/7.32): snapshot creation service + consistency.
//
// The ExecutionSnapshotService composes the creation pipeline (WBS 7.31)
// against ONE consistent view of persisted state:
//
//	read runtime state -> resolve state -> resolve role snapshot ->
//	resolve role + state state-file requirements -> resolve correctly
//	scoped files -> verify required files -> capture exact versions ->
//	persist.
//
// With a tx pool wired, the whole read+persist runs inside ONE dbrepo.Run
// transaction (WBS 7.31 "one consistent view"): every repository read
// joins the transaction, including the P7-13 scope-resolution op through
// its WithTx-joined variant. With a nil pool the reads run sequentially
// (the version guard is still the authoritative consistency check).
//
// Consistency (WBS 7.32): the runtime row's optimistic-concurrency version
// (WBS 6.24 — the value a transition bumps atomically) is read at the top
// of each attempt and re-read before persist; a concurrent transition in
// between is DETECTED (T7.32.2) and the attempt is RETRIED with a fresh
// view (T7.32.3) — the op succeeds against the fresh position or fails,
// never persisting a mixed context. A bounded retry budget
// (executionSnapshotRetries) ends in ErrConflict. Every failure path
// persists nothing (T7.32.4 — the persist is one atomic write of the
// snapshot row plus its file pins, rolled back as a unit on failure).
//
// Role resolution (task-level choice, stated so TESTER/PM can verify; the
// WBS says "Resolve Role Snapshot" without fixing the source): the role
// ASSIGNED to the runtime's current state at the SMALLEST ordinal of
// state_role_assignments (the ordered participating-role list, WBS 6.4),
// visiting roles in ordinal order and skipping disabled roles; a state
// with no assignments falls back to the project's role with the smallest
// key; no usable role at all is ErrNotFound.
//
// Missing required files use the P7-13 semantics: the scope-resolution op
// fails with ErrMissingStateFile identifying the type, the snapshot is
// prevented, and nothing is persisted (T7.31.5).
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/statefile"
)

// executionSnapshotRetries bounds the concurrent-transition retry budget
// (WBS 7.32.3): after this many detected moves the op fails with
// ErrConflict rather than persist a mixed context.
const executionSnapshotRetries = 3

// ExecutionSnapshotService creates execution-context snapshots (WBS 7.31/
// 7.32). No REST surface (the read API is P7-23).
type ExecutionSnapshotService struct {
	runtime      persistence.RuntimeStateRepository
	states       persistence.WorkflowStateRepository
	assignments  persistence.StateRoleAssignmentRepository
	roles        persistence.RoleRepository
	roleSvc      *RoleService
	scopeRes     *StateFileScopeResolutionService
	fileVersions persistence.StateFileVersionRepository
	snapshots    persistence.ContextSnapshotRepository
	txPool       *pgxpool.Pool

	// Probes (T7.32.x forced paths, used ONLY by the integration tests):
	// runtimeProbe runs between the read and the persist of one attempt
	// (an in-flight concurrent transition); persistProbe replaces the
	// persist step (a forced persistence failure).
	runtimeProbe func(projectID string) bool
	persistProbe func() error
}

// NewExecutionSnapshotService wires the snapshot-creation op: the
// runtime, state, assignment and role repositories, the Phase 5
// role-snapshot service (same package — the role read joins the
// attempt's transaction), the P7-13 scope-resolution op, and the
// state-file version repository (the exact version capture).
func NewExecutionSnapshotService(
	runtime persistence.RuntimeStateRepository,
	states persistence.WorkflowStateRepository,
	assignments persistence.StateRoleAssignmentRepository,
	roles persistence.RoleRepository,
	roleSvc *RoleService,
	scopeRes *StateFileScopeResolutionService,
) *ExecutionSnapshotService {
	return &ExecutionSnapshotService{
		runtime: runtime, states: states,
		assignments: assignments, roles: roles,
		roleSvc: roleSvc, scopeRes: scopeRes,
	}
}

// WithSnapshots wires the snapshot repository (required for Create).
func (s *ExecutionSnapshotService) WithSnapshots(repo persistence.ContextSnapshotRepository) *ExecutionSnapshotService {
	s.snapshots = repo
	return s
}

// WithFileVersions wires the state-file version repository (the exact
// version capture; required for Create).
func (s *ExecutionSnapshotService) WithFileVersions(repo persistence.StateFileVersionRepository) *ExecutionSnapshotService {
	s.fileVersions = repo
	return s
}

// WithTxPool wires the transaction pool so the read+persist is one
// transaction (WBS 7.31); with a nil pool the op runs sequentially.
func (s *ExecutionSnapshotService) WithTxPool(pool *pgxpool.Pool) *ExecutionSnapshotService {
	s.txPool = pool
	return s
}

// WithRuntimeProbe wires the in-flight transition probe (T7.32.2/T7.32.3,
// test seam): the probe runs between the read and the persist of one
// attempt.
func (s *ExecutionSnapshotService) WithRuntimeProbe(probe func(projectID string) bool) *ExecutionSnapshotService {
	s.runtimeProbe = probe
	return s
}

// WithPersistProbe wires the forced persistence failure (T7.32.4, test
// seam): the persist step returns its error.
func (s *ExecutionSnapshotService) WithPersistProbe(probe func() error) *ExecutionSnapshotService {
	s.persistProbe = probe
	return s
}

// CreateExecutionSnapshot performs the WBS 7.31 pipeline against one
// consistent view of persisted state and persists the resulting
// ExecutionContextSnapshot. It returns the created snapshot. Failure
// classes: unknown project runtime (ErrNotFound), missing required state
// file (ErrMissingStateFile, nothing persisted), and a concurrent runtime
// transition that outlasts the retry budget (ErrConflict, nothing
// persisted).
func (s *ExecutionSnapshotService) CreateExecutionSnapshot(ctx context.Context, projectID string) (*statefile.ExecutionContextSnapshot, error) {
	return s.CreateExecutionSnapshotIncluding(ctx, projectID, nil)
}

// CreateExecutionSnapshotIncluding is CreateExecutionSnapshot that also
// pins the listed state-file types when a file exists at the runtime's
// scope (optional — absent files are skipped). Phase 16 repair dispatches
// pin TEST_STATE so the failure evidence reaches the FIX prompt (WBS
// 16.49) without making it a project-wide requirement.
func (s *ExecutionSnapshotService) CreateExecutionSnapshotIncluding(ctx context.Context, projectID string, extra []string) (*statefile.ExecutionContextSnapshot, error) {
	if s.snapshots == nil || s.fileVersions == nil {
		return nil, ErrInfrastructure
	}
	if s.roleSvc == nil || s.scopeRes == nil {
		return nil, ErrInfrastructure
	}
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	for attempt := 0; ; attempt++ {
		var out *statefile.ExecutionContextSnapshot
		var e error
		if s.txPool == nil {
			out, e = s.createOnce(ctx, nil, pid, projectID, extra)
		} else {
			e = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
				var ee error
				out, ee = s.createOnce(ctx, tx, pid, projectID, extra)
				return ee
			})
		}
		if e == nil {
			return out, nil
		}
		if !errors.Is(e, ErrConflict) || attempt+1 >= executionSnapshotRetries {
			return nil, e
		}
		// Detected a concurrent runtime move: retry with a fresh view
		// of the (new) position (WBS 7.32.3).
	}
}

// createOnce performs one creation attempt: the full read pipeline, the
// in-flight probe, the consistency re-check, and the persist — all on the
// given transaction (nil for the sequential path).
func (s *ExecutionSnapshotService) createOnce(ctx context.Context, tx persistence.Transaction, pid uuid.UUID, projectID string, extra []string) (*statefile.ExecutionContextSnapshot, error) {
	rt := s.runtime
	if tx != nil {
		rt = s.runtime.WithTx(tx)
	}
	rtRec, err := rt.Get(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}
	baseVersion := rtRec.Version
	if s.runtimeProbe != nil {
		s.runtimeProbe(projectID)
	}
	// Consistency re-check (WBS 7.32.2/T7.32.3): the runtime version
	// read at the top must still match immediately before persist — a
	// concurrent transition in between moves the position and this
	// attempt's context is stale.
	fresh, err := rt.Get(ctx, pid)
	if err != nil {
		return nil, translate(err)
	}
	if fresh.Version != baseVersion {
		return nil, ErrConflict
	}
	snap, err := s.assemble(ctx, tx, pid, rtRec, extra...)
	if err != nil {
		return nil, err
	}
	if err := s.persist(ctx, tx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// assemble runs the read pipeline (WBS 7.31) on the given transaction:
// state -> role snapshot -> requirements + scoped files -> verification ->
// exact version capture. The runtime record is authoritative for the
// position fields.
func (s *ExecutionSnapshotService) assemble(ctx context.Context, tx persistence.Transaction, pid uuid.UUID, rtRec *persistence.RuntimeStateRecord, extra ...string) (*statefile.ExecutionContextSnapshot, error) {
	// Resolve the state (the runtime's current state id).
	stRepo := s.states
	if tx != nil {
		stRepo = s.states.WithTx(tx)
	}
	st, err := stRepo.Get(ctx, rtRec.CurrentStateID)
	if err != nil {
		return nil, translate(err)
	}
	// Resolve the role snapshot (the state's assigned role, smallest
	// ordinal, disabled skipped; fallback the project's smallest-key
	// role; none usable is ErrNotFound). The Phase 5 snapshot assembly
	// joins the same transaction (one consistent view).
	roleRec, err := s.resolveRole(ctx, tx, pid, st)
	if err != nil {
		return nil, err
	}
	roleSnap, err := s.roleSvc.assembleSnapshot(ctx, tx, roleRec)
	if err != nil {
		return nil, translate(err)
	}
	// Resolve the correctly scoped files for every required type. A
	// missing required file fails the whole op (WBS 7.19/T7.31.5) —
	// the scope-resolution op is read-only, so a failure leaves the
	// persisted state untouched.
	var resolved []ResolvedStateFile
	var resErr error
	if tx != nil {
		resolved, resErr = s.scopeRes.ResolveStateFileScopesTx(ctx, tx, pid.String())
	} else {
		resolved, resErr = s.scopeRes.ResolveStateFileScopes(ctx, pid.String())
	}
	if resErr != nil {
		return nil, resErr
	}
	if len(extra) > 0 {
		more, err := s.optionalFiles(ctx, tx, pid, rtRec, resolved, extra)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, more...)
	}
	verRepo := s.fileVersions
	if tx != nil {
		verRepo = s.fileVersions.WithTx(tx)
	}
	files := make([]statefile.StateFileSnapshot, 0, len(resolved))
	for _, rsf := range resolved {
		file := rsf.File
		hash, content := "", ""
		if file.CurrentVersion > 0 {
			// Capture the EXACT version's hash + content (WBS 7.30);
			// the scope op verified presence, a missing version row
			// here is a data invariant violation.
			v, err := verRepo.GetVersion(ctx, file.ID, file.CurrentVersion)
			if err != nil {
				return nil, translate(err)
			}
			hash, content = v.ContentHash, v.Content
		}
		files = append(files, statefile.StateFileSnapshot{
			StateFileID: file.ID,
			TypeKey:     rsf.Type.Key,
			Scope:       statefile.StateFileScope(rsf.Type.Scope),
			Version:     file.CurrentVersion,
			Hash:        hash,
			Content:     content,
			// WBS 7.36: the pin's source classification, persisted with
			// the snapshot (read back from the snapshot, never
			// re-derived from live state — T7.36.5).
			Classification: statefile.ClassifySnapshotFile(rsf.Type.Key, statefile.StateFileScope(rsf.Type.Scope)),
		})
	}
	sortFileSnapshots(files)
	return &statefile.ExecutionContextSnapshot{
		ID:                  uuid.New(),
		ProjectID:           pid,
		PhaseID:             rtRec.PhaseID,
		WorkUnitID:          rtRec.WorkUnitID,
		RuntimeStateID:      rtRec.ID,
		RuntimeStateVersion: rtRec.Version,
		StateMachineID:      rtRec.StateMachineID,
		StateMachineVersion: rtRec.StateMachineVersion,
		StateKey:            st.Key,
		RoleSnapshot:        *roleSnap,
		StateFiles:          files,
	}, nil
}

// resolveRole returns the role the snapshot carries (the stated
// task-level choice): the state's assigned role at the smallest ordinal,
// visiting in ordinal order and skipping disabled roles; a state with no
// assignments falls back to the project's smallest-key role; none usable
// is ErrNotFound.
func (s *ExecutionSnapshotService) resolveRole(ctx context.Context, tx persistence.Transaction, pid uuid.UUID, st *persistence.WorkflowStateRecord) (*persistence.RoleRecord, error) {
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
	// No usable assignment: the project's smallest-key role.
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

// persist writes the snapshot + its file pins through the repository (one
// atomic write — the T7.32.4 no-partial-persist guarantee; the
// surrounding transaction rolls the unit back on failure). The
// persistProbe seam (T7.32.4) replaces it.
func (s *ExecutionSnapshotService) persist(ctx context.Context, tx persistence.Transaction, snap *statefile.ExecutionContextSnapshot) error {
	if s.persistProbe != nil {
		return translate(s.persistProbe())
	}
	roleJSON, err := json.Marshal(snap.RoleSnapshot)
	if err != nil {
		return ErrInfrastructure
	}
	now := time.Now().UTC()
	// WBS 7.35: the token budget is computed from the CAPTURED content and
	// stored with the snapshot (not re-derived at read, so it survives
	// reconstruction). The role estimate is the system-prompt + assigned
	// instructions where available (empty content estimates 0).
	size := snap.Size()
	rec := &persistence.ContextSnapshotRecord{
		ID:                  snap.ID,
		ProjectID:           snap.ProjectID,
		PhaseID:             snap.PhaseID,
		WorkUnitID:          snap.WorkUnitID,
		RuntimeStateID:      snap.RuntimeStateID,
		RuntimeStateVersion: snap.RuntimeStateVersion,
		StateMachineID:      snap.StateMachineID,
		StateMachineVersion: snap.StateMachineVersion,
		StateKey:            snap.StateKey,
		RoleSnapshotJSON:    roleJSON,
		StateFileTokens:     size.StateFileTokens,
		RoleTokens:          size.RoleTokens,
		EstimatedTotal:      size.EstimatedTotal,
		CreatedAt:           now,
	}
	for _, f := range snap.StateFiles {
		rec.Files = append(rec.Files, &persistence.ContextSnapshotFileRecord{
			ID:          uuid.New(),
			SnapshotID:  snap.ID,
			StateFileID: f.StateFileID,
			TypeKey:     f.TypeKey,
			Scope:       string(f.Scope),
			Version:     f.Version,
			Hash:        f.Hash,
			Content:     f.Content,
			// Per-pin component of the WBS 7.35 budget (empty optional
			// files estimate 0) + the WBS 7.36 classification.
			TokenEstimate:  statefile.TokenEstimate(f.Content),
			Classification: string(f.Classification),
			CreatedAt:      now,
		})
	}
	repo := s.snapshots
	if tx != nil {
		repo = s.snapshots.WithTx(tx)
	}
	if err := repo.Create(ctx, rec); err != nil {
		return translate(err)
	}
	return nil
}

// sortFileSnapshots orders the snapshot's file pins by type key
// (deterministic, WBS 7.31).
func sortFileSnapshots(files []statefile.StateFileSnapshot) {
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && files[j-1].TypeKey > files[j].TypeKey; j-- {
			files[j-1], files[j] = files[j], files[j-1]
		}
	}
}

// ProjectedSnapshot is an UNPERSISTED execution-context snapshot assembled
// for a projected position (Phase 11, WBS 11.19/11.20): the role, state
// files and identity a prompt WOULD see after a valid future transition.
// It is never written as a snapshot row.
type ProjectedSnapshot struct {
	Snapshot *statefile.ExecutionContextSnapshot
	// MissingRequired lists required state-file types with no file at the
	// projected position (e.g. a future unit's ACTIVE_TASK).
	MissingRequired []string
	RoleKey         string
	RoleEnabled     bool
}

// AssembleProjected assembles the context of a projected position: the
// target state (of the runtime's pinned machine version), the projected
// phase / work unit, and the role the target state resolves to — through
// the same Phase 5/7 assembly as a real snapshot, without persisting
// anything and without touching the runtime row.
func (s *ExecutionSnapshotService) AssembleProjected(ctx context.Context, projectID uuid.UUID, rt *persistence.RuntimeStateRecord, targetStateID uuid.UUID, phaseID, unitID *uuid.UUID) (*ProjectedSnapshot, error) {
	st, err := s.states.Get(ctx, targetStateID)
	if err != nil {
		return nil, translate(err)
	}
	roleRec, err := s.resolveRole(ctx, nil, projectID, st)
	if err != nil {
		return nil, err
	}
	roleSnap, err := s.roleSvc.assembleSnapshot(ctx, nil, roleRec)
	if err != nil {
		return nil, translate(err)
	}
	resolved, missing, err := s.scopeRes.ResolveStateFileScopesAt(ctx, projectID.String(), phaseID, unitID)
	if err != nil {
		return nil, err
	}
	files := make([]statefile.StateFileSnapshot, 0, len(resolved))
	for _, rsf := range resolved {
		file := rsf.File
		hash, content := "", ""
		if file.CurrentVersion > 0 {
			v, err := s.fileVersions.GetVersion(ctx, file.ID, file.CurrentVersion)
			if err != nil {
				return nil, translate(err)
			}
			hash, content = v.ContentHash, v.Content
		}
		files = append(files, statefile.StateFileSnapshot{
			StateFileID: file.ID, TypeKey: rsf.Type.Key, Scope: statefile.StateFileScope(rsf.Type.Scope),
			Version: file.CurrentVersion, Hash: hash, Content: content,
			Classification: statefile.ClassifySnapshotFile(rsf.Type.Key, statefile.StateFileScope(rsf.Type.Scope)),
		})
	}
	out := &ProjectedSnapshot{RoleKey: roleRec.Key, RoleEnabled: roleRec.Enabled}
	for _, m := range missing {
		out.MissingRequired = append(out.MissingRequired, m.Key)
	}
	sortFileSnapshots(files)
	out.Snapshot = &statefile.ExecutionContextSnapshot{
		ID: uuid.Nil, ProjectID: projectID, PhaseID: phaseID, WorkUnitID: unitID,
		RuntimeStateID: rt.ID, RuntimeStateVersion: rt.Version,
		StateMachineID: rt.StateMachineID, StateMachineVersion: rt.StateMachineVersion,
		StateKey: st.Key, RoleSnapshot: *roleSnap, StateFiles: files,
	}
	return out, nil
}

// SnapshotToRecord converts an assembled snapshot into the (unpersisted)
// record shape the composer resolves from, with the given record id (the
// real base snapshot for projections).
func SnapshotToRecord(snap *statefile.ExecutionContextSnapshot, id uuid.UUID) (*persistence.ContextSnapshotRecord, error) {
	roleJSON, err := json.Marshal(snap.RoleSnapshot)
	if err != nil {
		return nil, err
	}
	rec := &persistence.ContextSnapshotRecord{
		ID: id, ProjectID: snap.ProjectID, PhaseID: snap.PhaseID, WorkUnitID: snap.WorkUnitID,
		RuntimeStateID: snap.RuntimeStateID, RuntimeStateVersion: snap.RuntimeStateVersion,
		StateMachineID: snap.StateMachineID, StateMachineVersion: snap.StateMachineVersion,
		StateKey: snap.StateKey, RoleSnapshotJSON: roleJSON, CreatedAt: snap.CreatedAt,
	}
	for _, f := range snap.StateFiles {
		rec.Files = append(rec.Files, &persistence.ContextSnapshotFileRecord{
			ID: uuid.New(), SnapshotID: id, StateFileID: f.StateFileID, TypeKey: f.TypeKey, Scope: string(f.Scope),
			Version: f.Version, Hash: f.Hash, Content: f.Content, Classification: string(f.Classification),
		})
	}
	return rec, nil
}

// optionalFiles resolves extra state-file types at the runtime's scope,
// skipping types already pinned and files that do not exist.
func (s *ExecutionSnapshotService) optionalFiles(ctx context.Context, tx persistence.Transaction, pid uuid.UUID, rt *persistence.RuntimeStateRecord,
	have []ResolvedStateFile, extra []string) ([]ResolvedStateFile, error) {
	types, err := s.scopeRes.TypeByRequirementKey(ctx, tx, pid)
	if err != nil {
		return nil, err
	}
	pinned := map[string]bool{}
	for _, r := range have {
		pinned[r.Type.Key] = true
	}
	repos := s.scopeRes.bind(tx)
	var out []ResolvedStateFile
	for _, key := range extra {
		rec := types[key]
		if rec == nil || pinned[key] {
			continue
		}
		var phaseID, unitID *uuid.UUID
		switch statefile.StateFileScope(rec.Scope) {
		case statefile.ScopePhase:
			phaseID = rt.PhaseID
		case statefile.ScopeWorkUnit:
			unitID = rt.WorkUnitID
		}
		f, err := repos.files.GetByTypeAndScope(ctx, pid, rec.ID, rec.Scope, phaseID, unitID)
		if errors.Is(err, persistence.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, translate(err)
		}
		pinned[key] = true
		out = append(out, ResolvedStateFile{Type: rec, File: f})
	}
	return out, nil
}

// ErrSnapshotCorrupt: a stored context snapshot no longer matches its
// recorded hashes / relationships (WBS 18.199).
var ErrSnapshotCorrupt = errors.New("context snapshot is corrupt")

// VerifySnapshot re-checks a stored snapshot before prompt composition
// (WBS 18.199): it belongs to the project, every file belongs to it, and
// every file's content still hashes to its recorded content hash.
func (s *ExecutionSnapshotService) VerifySnapshot(ctx context.Context, projectID, snapshotID uuid.UUID) error {
	snap, err := s.snapshots.Get(ctx, snapshotID)
	if err != nil {
		return translate(err)
	}
	if snap.ProjectID != projectID {
		return fmt.Errorf("%w: snapshot %s belongs to another project", ErrSnapshotCorrupt, snapshotID)
	}
	for _, f := range snap.Files {
		if f.SnapshotID != snapshotID {
			return fmt.Errorf("%w: file %s is not part of snapshot %s", ErrSnapshotCorrupt, f.ID, snapshotID)
		}
		if f.Hash != "" && statefile.ContentHash(f.Content) != f.Hash {
			return fmt.Errorf("%w: %s v%d content does not match its hash", ErrSnapshotCorrupt, f.TypeKey, f.Version)
		}
	}
	return nil
}
