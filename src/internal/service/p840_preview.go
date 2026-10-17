// P8-40 (WBS 8.35): the prompt-preview read op.
//
// The PromptPreviewService renders the canonical prompt for the project's
// CURRENT runtime position WITHOUT persisting a CanonicalPrompt row and
// WITHOUT calling an LLM (WBS 8.35). It reuses the SAME read pipeline the
// P7-18 snapshot-creation op uses (role resolution, P7-13 scope resolution,
// exact-version file capture) and the SAME composition core the P8-38
// composer uses (prompt.ComposeCanonicalPromptDetailed) — no parallel
// assembly path. It returns the rendered content plus the WBS 8.35 metadata
// (state, role, template, estimated tokens, per-component sizes, snapshot
// hash, prompt hash) and identifies any required content that is absent at
// the pinned position (a non-fatal report, T8.35.4).
//
// Determinism (T8.35.5): the preview over the same live position is
// byte-identical in content + hash + metadata. The snapshot hash is derived
// deterministically from the resolved file pins (sorted by type key), so it
// is stable over an unchanged position and moves when a file changes.
//
// Service-only surface (the REST endpoint is registered in internal/web).
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// PromptPreviewRequest is the preview request: the pinned template identity
// (optional — when absent the state's referenced template is resolved) and an
// optional continuation status (its work-unit association, validated against
// the position's bound unit, T8.25.4).
type PromptPreviewRequest struct {
	TemplateID      *uuid.UUID
	TemplateVersion *int

	ContinuationStatusID   *uuid.UUID
	ContinuationWorkUnitID *uuid.UUID
}

// NewPromptPreviewRequest validates a preview request: the template identity
// must be present together (both template_id + template_version, or neither —
// a partial pin is invalid), and the continuation identity likewise (both
// status id + work-unit id). It returns the typed service request.
// PromptPreviewRequestInput is the raw (string-typed) preview request the web
// layer decodes; NewPromptPreviewRequest validates and converts it.
type PromptPreviewRequestInput struct {
	TemplateID             string
	TemplateVersion        int
	ContinuationStatusID   string
	ContinuationWorkUnitID string
}

func NewPromptPreviewRequest(body PromptPreviewRequestInput) (PromptPreviewRequest, error) {
	var req PromptPreviewRequest
	if body.TemplateID != "" || body.TemplateVersion > 0 {
		if body.TemplateID == "" || body.TemplateVersion <= 0 {
			return req, ErrInvalidInput
		}
		id, err := uuid.Parse(body.TemplateID)
		if err != nil {
			return req, ErrInvalidInput
		}
		req.TemplateID = &id
		ver := body.TemplateVersion
		req.TemplateVersion = &ver
	}
	if body.ContinuationStatusID != "" || body.ContinuationWorkUnitID != "" {
		if body.ContinuationStatusID == "" || body.ContinuationWorkUnitID == "" {
			return req, ErrInvalidInput
		}
		sid, err := uuid.Parse(body.ContinuationStatusID)
		if err != nil {
			return req, ErrInvalidInput
		}
		wid, err := uuid.Parse(body.ContinuationWorkUnitID)
		if err != nil {
			return req, ErrInvalidInput
		}
		req.ContinuationStatusID = &sid
		req.ContinuationWorkUnitID = &wid
	}
	return req, nil
}

// PreviewComponentSize is one composed component's size metadata (WBS 8.35
// "component sizes"): its type, source provenance, and approximate token
// estimate.
type PreviewComponentSize struct {
	Type          prompt.PromptComponentType
	SourceID      uuid.UUID
	SourceVersion int
	TokenEstimate int
}

// PromptPreview is the preview read model (WBS 8.35): the rendered content,
// the prompt hash, the estimated tokens, the per-component sizes, the
// snapshot hash, and the metadata envelope (state, role, template).
// MissingRequired lists the required items absent at the pinned position
// (T8.35.4) — a non-fatal report.
type PromptPreview struct {
	StateKey        string
	RoleKey         string
	TemplateID      uuid.UUID
	TemplateVersion int

	Content         string
	Hash            string
	EstimatedTokens int
	SnapshotHash    string

	Components      []PreviewComponentSize
	MissingRequired []string
}

// PromptPreviewService previews the canonical prompt (WBS 8.35) and reports
// missing requirements.
type PromptPreviewService struct {
	runtime      persistence.RuntimeStateRepository
	states       persistence.WorkflowStateRepository
	assignments  persistence.StateRoleAssignmentRepository
	roles        persistence.RoleRepository
	roleSvc      *RoleService
	scopeRes     *StateFileScopeResolutionService
	fileVersions persistence.StateFileVersionRepository
	tmplVer      persistence.PromptTemplateVersionRepository
	templates    persistence.PromptTemplateRepository
	protocols    persistence.ResponseProtocolRepository
	units        *dbrepo.WorkUnitRepository
	plans        *dbrepo.PlanRepository
	txPool       *pgxpool.Pool
}

// NewPromptPreviewService wires the preview read op.
func NewPromptPreviewService(
	runtime persistence.RuntimeStateRepository,
	states persistence.WorkflowStateRepository,
	assignments persistence.StateRoleAssignmentRepository,
	roles persistence.RoleRepository,
	roleSvc *RoleService,
	scopeRes *StateFileScopeResolutionService,
	fileVersions persistence.StateFileVersionRepository,
	tmplVer persistence.PromptTemplateVersionRepository,
	templates persistence.PromptTemplateRepository,
	protocols persistence.ResponseProtocolRepository,
	units *dbrepo.WorkUnitRepository,
	plans *dbrepo.PlanRepository,
) *PromptPreviewService {
	return &PromptPreviewService{
		runtime: runtime, states: states,
		assignments: assignments, roles: roles,
		roleSvc: roleSvc, scopeRes: scopeRes,
		fileVersions: fileVersions, tmplVer: tmplVer,
		templates: templates, protocols: protocols,
		units: units, plans: plans,
	}
}

// WithTxPool wires the transaction pool so the reads run in one consistent
// view (the nil-pool path runs sequentially).
func (s *PromptPreviewService) WithTxPool(pool *pgxpool.Pool) *PromptPreviewService {
	s.txPool = pool
	return s
}

// Preview renders the canonical prompt for the project's current runtime
// position (WBS 8.35) WITHOUT persisting a CanonicalPrompt row and WITHOUT
// altering the runtime (T8.35.2/T8.35.3). It reports any required content
// absent at the position in MissingRequired (T8.35.4, non-fatal). Failure
// classes: unknown project runtime (ErrNotFound), no usable role (ErrNotFound),
// a missing pinned protocol (ErrNotFound — the position is not composable),
// and a missing template (ErrNotFound).
func (s *PromptPreviewService) Preview(ctx context.Context, projectID string, req PromptPreviewRequest) (*PromptPreview, error) {
	if s.roleSvc == nil || s.scopeRes == nil || s.tmplVer == nil {
		return nil, ErrInfrastructure
	}
	pid, err := uuid.Parse(projectID)
	if err != nil {
		return nil, ErrInvalidInput
	}
	var out *PromptPreview
	var rerr error
	if s.txPool == nil {
		out, rerr = s.previewOnce(ctx, nil, pid, req)
	} else {
		rerr = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
			var e error
			out, e = s.previewOnce(ctx, tx, pid, req)
			return e
		})
	}
	if rerr != nil {
		return nil, rerr
	}
	return out, nil
}

// previewOnce performs one preview over the given transaction (nil for the
// sequential path).
func (s *PromptPreviewService) previewOnce(ctx context.Context, tx persistence.Transaction, pid uuid.UUID, req PromptPreviewRequest) (*PromptPreview, error) {
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
		stRepo = stRepo.WithTx(tx)
	}
	st, err := stRepo.Get(ctx, rtRec.CurrentStateID)
	if err != nil {
		return nil, translate(err)
	}
	// Resolve the role snapshot (the P7-18 resolution rule).
	roleRec, err := s.resolveRole(ctx, tx, pid, st)
	if err != nil {
		return nil, err
	}
	roleSnap, err := s.roleSvc.assembleSnapshot(ctx, tx, roleRec)
	if err != nil {
		return nil, translate(err)
	}
	// Resolve the required files, reporting the missing ones (T8.35.4).
	resolved, resErr := s.resolveRequiredScopedFiles(ctx, tx, pid)
	if resErr != nil {
		return nil, resErr
	}
	verRepo := s.fileVersions
	if tx != nil {
		verRepo = verRepo.WithTx(tx)
	}
	files := make([]statefile.StateFileSnapshot, 0, len(resolved))
	var missing []string
	for _, rsf := range resolved {
		if rsf.File == nil || rsf.File.CurrentVersion <= 0 {
			// A required type with no file at its scope: reported, not fatal.
			missing = append(missing, rsf.Type.Key)
			continue
		}
		v, verr := verRepo.GetVersion(ctx, rsf.File.ID, rsf.File.CurrentVersion)
		if verr != nil {
			return nil, translate(verr)
		}
		files = append(files, statefile.StateFileSnapshot{
			StateFileID:    rsf.File.ID,
			TypeKey:        rsf.Type.Key,
			Scope:          statefile.StateFileScope(rsf.Type.Scope),
			Version:        v.Version,
			Hash:           v.ContentHash,
			Content:        v.Content,
			Classification: statefile.ClassifySnapshotFile(rsf.Type.Key, statefile.StateFileScope(rsf.Type.Scope)),
		})
	}
	sortStateFilePins(files)
	// Deterministic snapshot hash over the resolved pins (stable over an
	// unchanged position; moves when a file changes).
	snapshotHash := previewSnapshotHash(files)

	// Work unit: the position's bound unit (optional).
	var unit *prompt.WorkUnitContext
	var unitPtr *uuid.UUID
	if rtRec.WorkUnitID != nil {
		u, uerr := s.loadUnit(ctx, *rtRec.WorkUnitID)
		if uerr != nil {
			return nil, uerr
		}
		unit = u
		unitPtr = rtRec.WorkUnitID
	}

	// Continuation (T8.25.4): validated against the bound unit.
	var contStatus *prompt.ContinuationStatus
	if req.ContinuationStatusID != nil {
		if req.ContinuationWorkUnitID == nil || rtRec.WorkUnitID == nil {
			return nil, ErrInvalidInput
		}
		contStatus, _ = prompt.NewContinuationStatus(prompt.ContinuationStatusInput{
			WorkUnitID: *req.ContinuationWorkUnitID,
			CapturedAt: time.Now().UTC(),
		})
		if err := prompt.ValidateContinuationForUnit(contStatus, *rtRec.WorkUnitID); err != nil {
			return nil, translate(err)
		}
	}

	// Response protocol: the state's pinned protocol (a missing pin means the
	// position is not composable — ErrNotFound).
	protocolID, protocolVersion, protocolContent, perr := s.resolveProtocol(ctx, tx, st)
	if perr != nil {
		return nil, perr
	}

	// Template: the request's pinned identity, or the state's referenced
	// template at its current version.
	tmplID, tmplVerNo, terr := s.resolveTemplate(ctx, tx, pid, st, req)
	if terr != nil {
		return nil, terr
	}
	tmplRepo := s.tmplVer
	if tx != nil {
		tmplRepo = tmplRepo.WithTx(tx)
	}
	tmplRow, terr := tmplRepo.GetVersion(ctx, tmplID, tmplVerNo)
	if terr != nil {
		return nil, translate(terr)
	}

	in := prompt.CanonicalCompositionInput{
		ProjectID:           pid,
		PhaseID:             rtRec.PhaseID,
		UnitID:              unitPtr,
		ContextSnapshotID:   rtRec.ID,
		TemplateID:          tmplID,
		TemplateVersion:     tmplVerNo,
		RoleID:              roleSnap.RoleID,
		RoleVersion:         roleSnap.RoleVersion,
		StateMachineID:      rtRec.StateMachineID,
		StateMachineVersion: rtRec.StateMachineVersion,
		StateKey:            st.Key,
		Role:                *roleSnap,
		StateFiles:          files,
		Required:            roleSnap.StateFiles,
		Unit:                unit,
		Continuation:        contStatus,
		ProtocolID:          protocolID,
		ProtocolVersion:     protocolVersion,
		ProtocolContent:     protocolContent,
		TemplateSource:      tmplRow.Content,
		SnapshotHash:        snapshotHash,
	}
	cp, _, comps, cerr := prompt.ComposeCanonicalPromptDetailed(in)
	if cerr != nil {
		// The position is not composable (a domain condition, not an
		// infrastructure failure): a conflict carrying the reason.
		return nil, fmt.Errorf("%w: prompt not composable at %s: %v", ErrConflict, st.Key, cerr)
	}
	sizes := make([]PreviewComponentSize, 0, len(comps))
	for _, c := range comps {
		sizes = append(sizes, PreviewComponentSize{
			Type:          c.Type,
			SourceID:      c.SourceID,
			SourceVersion: c.SourceVersion,
			TokenEstimate: (len(c.Content) + 3) / 4,
		})
	}
	return &PromptPreview{
		StateKey:        st.Key,
		RoleKey:         roleSnap.RoleKey,
		TemplateID:      tmplID,
		TemplateVersion: tmplVerNo,
		Content:         cp.Content,
		Hash:            cp.Hash,
		EstimatedTokens: cp.EstimatedTokens,
		SnapshotHash:    snapshotHash,
		Components:      sizes,
		MissingRequired: missing,
	}, nil
}

// resolveRole is the P7-18/P7-23 role resolution rule (the state's assigned
// role at the smallest ordinal, disabled skipped; fallback the project's
// smallest-key role; none usable is ErrNotFound).
func (s *PromptPreviewService) resolveRole(ctx context.Context, tx persistence.Transaction, pid uuid.UUID, st *persistence.WorkflowStateRecord) (*persistence.RoleRecord, error) {
	asgns := s.assignments
	if tx != nil {
		asgns = asgns.WithTx(tx)
	}
	roleRepo := s.roles
	if tx != nil {
		roleRepo = roleRepo.WithTx(tx)
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

// resolveRequiredScopedFiles runs the P7-13 scope-resolution op and, on a
// missing-required-file failure, re-resolves reporting the missing types as
// nil-file entries (T8.35.4 — reported, not fatal).
func (s *PromptPreviewService) resolveRequiredScopedFiles(ctx context.Context, tx persistence.Transaction, pid uuid.UUID) ([]ResolvedStateFile, error) {
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
	if !errors.Is(err, ErrMissingStateFile) {
		return nil, translate(err)
	}
	var categories []string
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

// resolveProtocol resolves the state's pinned response protocol (id, pinned
// version, pinned content). A state with no pinned protocol is not
// composable (ErrNotFound — the P8-10/P8-22 cleared-capture contract).
func (s *PromptPreviewService) resolveProtocol(ctx context.Context, tx persistence.Transaction, st *persistence.WorkflowStateRecord) (uuid.UUID, int, string, error) {
	refs := s.listStateProtocolRefs(ctx, tx, st.ID)
	if len(refs) == 0 {
		return uuid.Nil, 0, "", ErrNotFound
	}
	ref := refs[0]
	protoRepo := s.protocols
	if tx != nil {
		protoRepo = protoRepo.WithTx(tx)
	}
	ver, err := protoRepo.GetVersion(ctx, ref.ProtocolID, ref.ProtocolVersion)
	if err != nil {
		return uuid.Nil, 0, "", translate(err)
	}
	return ref.ProtocolID, ref.ProtocolVersion, ver.Content, nil
}

// resolveTemplate returns the preview's pinned (template id, version): the
// request's explicit identity when supplied, else the state's referenced
// template key resolved (project scope, then global) at its current version.
func (s *PromptPreviewService) resolveTemplate(ctx context.Context, tx persistence.Transaction, pid uuid.UUID, st *persistence.WorkflowStateRecord, req PromptPreviewRequest) (uuid.UUID, int, error) {
	if req.TemplateID != nil && req.TemplateVersion != nil {
		return *req.TemplateID, *req.TemplateVersion, nil
	}
	tmplRepo := s.templates
	if tx != nil {
		tmplRepo = tmplRepo.WithTx(tx)
	}
	rec, err := tmplRepo.GetByKey(ctx, &pid, st.PromptTemplate)
	if errors.Is(err, persistence.ErrNotFound) {
		rec, err = tmplRepo.GetByKey(ctx, nil, st.PromptTemplate)
	}
	if err != nil {
		return uuid.Nil, 0, translate(err)
	}
	verRepo := s.tmplVer
	if tx != nil {
		verRepo = verRepo.WithTx(tx)
	}
	cur, err := verRepo.GetCurrentVersion(ctx, rec.ID)
	if err != nil {
		return uuid.Nil, 0, translate(err)
	}
	if cur == nil {
		return uuid.Nil, 0, ErrNotFound
	}
	return rec.ID, cur.Version, nil
}

// loadUnit loads the bound work unit's captured context (P8-14).
func (s *PromptPreviewService) loadUnit(ctx context.Context, unitID uuid.UUID) (*prompt.WorkUnitContext, error) {
	if s.units == nil {
		return nil, nil
	}
	u, err := s.units.Get(ctx, unitID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, nil
		}
		return nil, translate(err)
	}
	spec := u.Specification
	if spec == nil {
		spec = &wbs.WorkUnitSpecification{}
	}
	wc := &prompt.WorkUnitContext{
		ID:            u.ID,
		Name:          u.Name,
		Kind:          string(u.Kind),
		Objective:     spec.Objective,
		Acceptance:    spec.AcceptanceCriteria,
		RequiredTests: spec.RequiredTests,
		Constraints:   spec.Constraints,
	}
	return wc, nil
}

// previewRows is the minimal row-set surface the protocol-reference query
// needs.
type previewRows interface {
	Close()
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// listStateProtocolRefs reads the state's pinned protocol references (at most
// one per state), on the transaction when non-nil, else the raw pool.
func (s *PromptPreviewService) listStateProtocolRefs(ctx context.Context, tx persistence.Transaction, stateID uuid.UUID) []*persistence.StateProtocolReferenceRecord {
	var rows []*persistence.StateProtocolReferenceRecord
	var q func(ctx context.Context, query string, args ...any) (previewRows, error)
	if tx != nil {
		q = func(ctx context.Context, query string, args ...any) (previewRows, error) {
			return tx.Query(ctx, query, args...)
		}
	} else if s.txPool != nil {
		q = func(ctx context.Context, query string, args ...any) (previewRows, error) {
			return s.txPool.Query(ctx, query, args...)
		}
	} else {
		return nil
	}
	rs, err := q(ctx, `SELECT protocol_id, protocol_version FROM state_protocol_references WHERE state_id = $1`, stateID)
	if err != nil {
		return nil
	}
	defer rs.Close()
	for rs.Next() {
		var rec persistence.StateProtocolReferenceRecord
		if err := rs.Scan(&rec.ProtocolID, &rec.ProtocolVersion); err != nil {
			return nil
		}
		rec.StateID = stateID
		rows = append(rows, &rec)
	}
	return rows
}

// sortStateFilePins orders the pins by type key (deterministic, WBS 7.31).
func sortStateFilePins(files []statefile.StateFileSnapshot) {
	for i := 1; i < len(files); i++ {
		for j := i; j > 0 && files[j-1].TypeKey > files[j].TypeKey; j-- {
			files[j-1], files[j] = files[j], files[j-1]
		}
	}
}

// previewSnapshotHash derives a deterministic snapshot hash over the resolved
// file pins (sorted by type key): the hex SHA-256 of the "key|version|hash"
// lines. Stable over an unchanged position (T8.35.5) and moves when a file
// changes.
func previewSnapshotHash(files []statefile.StateFileSnapshot) string {
	var b []byte
	for _, f := range files {
		b = append(b, f.TypeKey...)
		b = append(b, '|')
		b = append(b, []byte(itoaInt(f.Version))...)
		b = append(b, '|')
		b = append(b, f.Hash...)
		b = append(b, '\n')
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// itoaInt renders an int as a decimal string (avoid a strconv import for one
// small helper).
func itoaInt(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
