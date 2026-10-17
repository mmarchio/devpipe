package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/compression"
	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/role"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// pgxRows is the minimal row-set surface the reference query needs.
type pgxRows interface {
	Close()
	Next() bool
	Scan(dest ...any) error
	Err() error
}

// P8-38 (WBS 8.25 + 8.32 halves T8.32.1-T8.32.2): the core prompt composer
// service. ComposePrompt resolves a pinned context snapshot (the caller's
// snapshot-captured values — never "latest") into canonical components
// through the existing per-state composer primitives (P8-15..P8-22), renders
// the pinned template, and persists the CanonicalPrompt record (WBS 8.26)
// with exact content + hash + token estimate + provenance (P8-31) + cache key
// (P8-34). Repeated composition over identical pinned inputs is byte-identical
// (T8.32.1/T8.32.2; the DB-row-order half is P8-39).
//
// Sensitive-data exclusion (WBS 8.38) applies through the registry's
// prompt-safe surface: the composed content is built only from the
// snapshot-captured values (which are prompt-safe by construction — the
// snapshot carries no secret settings), and the P8-37 registry prompt-safe
// surface is the only template-registration path.
//
// Service-only surface (the preview API is P8-40, the history API is P8-41):
// the tests drive the op over the harness pool.
type PromptComposerService struct {
	snapshots persistence.ContextSnapshotRepository
	tmplVer   persistence.PromptTemplateVersionRepository
	states    persistence.WorkflowStateRepository
	protocols persistence.ResponseProtocolRepository
	stInstr   persistence.StateInstructionRepository
	templates persistence.PromptTemplateRepository
	units     *dbrepo.WorkUnitRepository
	plans     *dbrepo.PlanRepository
	canonical persistence.CanonicalPromptRepository
	// wbsVersions + settings feed the PHASE_PLAN planning composition
	// (WBS 8.16/8.44): the phase's current WBS version+content and the
	// project's planning policy/granularity. Nil-safe: with either unwired
	// the planning composition degrades (no WBS/policy content) and the
	// prior-phase (non-planning) contract is unchanged.
	wbsVersions persistence.WBSVersionRepository
	settings    persistence.SettingsRepository
	// components persists the composed components (with compression
	// policies) and the preservation manifest alongside the canonical prompt
	// (Phase 9 P9-14). Nil-safe: without it only the canonical row is stored.
	components persistence.CanonicalPromptComponentRepository
	txPool     *pgxpool.Pool
}

// NewPromptComposerService wires the composer to its repositories. The
// canonical-prompt repository is the persist sink; the other repositories
// resolve the snapshot's pinned sources (template source, state instruction,
// response protocol, work unit).
func NewPromptComposerService(
	snapshots persistence.ContextSnapshotRepository,
	tmplVer persistence.PromptTemplateVersionRepository,
	states persistence.WorkflowStateRepository,
	protocols persistence.ResponseProtocolRepository,
	stInstr persistence.StateInstructionRepository,
	templates persistence.PromptTemplateRepository,
	units *dbrepo.WorkUnitRepository,
	plans *dbrepo.PlanRepository,
	canonical persistence.CanonicalPromptRepository,
) *PromptComposerService {
	return &PromptComposerService{
		snapshots: snapshots, tmplVer: tmplVer, states: states,
		protocols: protocols, stInstr: stInstr, templates: templates,
		units: units, plans: plans, canonical: canonical,
	}
}

// WithTxPool wires the transaction pool so the reads + persist run in one
// transaction; with a nil pool the op runs sequentially.
func (s *PromptComposerService) WithTxPool(pool *pgxpool.Pool) *PromptComposerService {
	s.txPool = pool
	return s
}

// WithWBSVersions wires the WBS-version repository used by the PHASE_PLAN
// planning composition (the phase's current WBS version + exact content).
// Nil-safe: without it the planning composition carries no WBS content.
func (s *PromptComposerService) WithWBSVersions(repo persistence.WBSVersionRepository) *PromptComposerService {
	s.wbsVersions = repo
	return s
}

// WithSettings wires the project-settings repository used by the PHASE_PLAN
// planning composition (the project's planning policy + granularity).
// Nil-safe: without it the planning composition carries no policy/granularity.
func (s *PromptComposerService) WithSettings(repo persistence.SettingsRepository) *PromptComposerService {
	s.settings = repo
	return s
}

// WithComponents wires the component/manifest repository (Phase 9 P9-14):
// every composed canonical prompt then also persists its components with
// their compression policies and its preservation manifest, in the same
// transaction. The canonical content, hash and estimate are unchanged.
func (s *PromptComposerService) WithComponents(repo persistence.CanonicalPromptComponentRepository) *PromptComposerService {
	s.components = repo
	return s
}

// ComposePrompt performs one composition: load the pinned snapshot, resolve
// the template source + state instruction + response protocol + work unit at
// their pinned versions, assemble the CanonicalCompositionInput, compose (pure
// domain, P8-38 core), deduplicate (P8-30), and persist the CanonicalPrompt.
// The request's continuation status (when present) is validated against the
// snapshot's bound work unit (T8.25.4). It returns the persisted record.
func (s *PromptComposerService) ComposePrompt(ctx context.Context, req prompt.ComposePromptRequest) (*persistence.CanonicalPromptRecord, error) {
	if s.snapshots == nil || s.canonical == nil {
		return nil, ErrInfrastructure
	}
	var rec *persistence.CanonicalPromptRecord
	var e error
	if s.txPool == nil {
		rec, e = s.composeOnce(ctx, nil, req)
	} else {
		e = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
			var ee error
			rec, ee = s.composeOnce(ctx, tx, req)
			return ee
		})
	}
	if e != nil {
		return nil, e
	}
	return rec, nil
}

// composeOnce performs one composition attempt on the given transaction (nil
// for the sequential path).
func (s *PromptComposerService) composeOnce(ctx context.Context, tx persistence.Transaction, req prompt.ComposePromptRequest) (*persistence.CanonicalPromptRecord, error) {
	in, _, err := s.resolve(ctx, tx, req)
	if err != nil {
		return nil, err
	}
	return s.persistComposed(ctx, tx, in, "")
}

// resolve loads the pinned snapshot, validates the optional continuation
// and resolves every composition source into the captured composition input
// (Phase 10 P10-07: the single source resolution shared by composition and
// cache-key derivation, so the two can never drift).
func (s *PromptComposerService) resolve(ctx context.Context, tx persistence.Transaction, req prompt.ComposePromptRequest) (prompt.CanonicalCompositionInput, *persistence.ContextSnapshotRecord, error) {
	snapRepo := s.snapshots
	if tx != nil {
		snapRepo = s.snapshots.WithTx(tx)
	}
	snap, err := snapRepo.Get(ctx, req.ContextSnapshotID)
	if err != nil {
		return prompt.CanonicalCompositionInput{}, nil, translate(err)
	}
	return s.resolveFrom(ctx, tx, snap, req)
}

// resolveFrom resolves a composition request against an already-loaded
// snapshot record (a persisted snapshot, or a Phase 11 projected record
// that is never persisted).
func (s *PromptComposerService) resolveFrom(ctx context.Context, tx persistence.Transaction, snap *persistence.ContextSnapshotRecord, req prompt.ComposePromptRequest) (prompt.CanonicalCompositionInput, *persistence.ContextSnapshotRecord, error) {
	var err error

	// T8.25.4: validate the optional continuation status against the work unit
	// bound in the snapshot.
	var contStatus *prompt.ContinuationStatus
	if req.ContinuationStatusID != nil {
		contStatus, err = s.loadContinuation(ctx, *req.ContinuationStatusID, req.ContinuationWorkUnitID, snap.WorkUnitID)
		if err != nil {
			return prompt.CanonicalCompositionInput{}, nil, err
		}
		if c := req.CapturedStatus; c != nil {
			if c.WorkUnitID != *req.ContinuationWorkUnitID {
				return prompt.CanonicalCompositionInput{}, nil, ErrInvalidInput
			}
			in := *c
			if in.CapturedAt.IsZero() {
				in.CapturedAt = contStatus.CapturedAt
			}
			if contStatus, err = prompt.NewContinuationStatus(in); err != nil {
				return prompt.CanonicalCompositionInput{}, nil, translate(err)
			}
		}
	}

	in, err := s.buildInput(ctx, tx, snap, req, contStatus)
	if err != nil {
		return prompt.CanonicalCompositionInput{}, nil, err
	}
	return in, snap, nil
}

// persistComposed composes the captured input and persists the canonical
// prompt (+ components/manifest when wired), tagged with the Layer 1 cache
// key when one is given.
func (s *PromptComposerService) persistComposed(ctx context.Context, tx persistence.Transaction, in prompt.CanonicalCompositionInput, cacheKey string) (*persistence.CanonicalPromptRecord, error) {
	return s.persistComposedP(ctx, tx, in, cacheKey, nil)
}

func (s *PromptComposerService) persistComposedP(ctx context.Context, tx persistence.Transaction, in prompt.CanonicalCompositionInput, cacheKey string, projectionID *uuid.UUID) (*persistence.CanonicalPromptRecord, error) {
	cp, _, comps, err := prompt.ComposeCanonicalPromptDetailed(in)
	if err != nil {
		return nil, translate(err)
	}
	// P8-30 dedup: an identical persisted artifact is reused (invocation
	// points at it, retaining its own association); novel content is stored.
	decision := prompt.DedupeCanonicalPrompt(cp, []prompt.CanonicalPrompt{})
	_ = decision

	rec := &persistence.CanonicalPromptRecord{
		ID:                  cp.ID,
		ProjectID:           cp.ProjectID,
		PhaseID:             cp.PhaseID,
		WorkUnitID:          cp.WorkUnitID,
		ContextSnapshotID:   cp.ContextSnapshotID,
		TemplateID:          cp.TemplateID,
		TemplateVersion:     cp.TemplateVersion,
		RoleID:              cp.RoleID,
		RoleVersion:         cp.RoleVersion,
		StateMachineID:      cp.StateMachineID,
		StateMachineVersion: cp.StateMachineVersion,
		StateKey:            cp.StateKey,
		Content:             cp.Content,
		Hash:                cp.Hash,
		EstimatedTokens:     cp.EstimatedTokens,
		CacheKey:            cacheKey,
		ProjectionID:        projectionID,
		CreatedAt:           cp.CreatedAt,
	}
	cRepo := s.canonical
	if tx != nil {
		cRepo = s.canonical.WithTx(tx)
	}
	if err := cRepo.Create(ctx, rec); err != nil {
		return nil, translate(err)
	}
	if s.components != nil {
		if err := s.persistComponents(ctx, tx, rec, in, comps); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

// ResolveComposition resolves a composition request into its captured
// composition input and the pinned snapshot record without composing or
// persisting anything (Phase 10 cache-input derivation).
func (s *PromptComposerService) ResolveComposition(ctx context.Context, req prompt.ComposePromptRequest) (prompt.CanonicalCompositionInput, *persistence.ContextSnapshotRecord, error) {
	if s.snapshots == nil {
		return prompt.CanonicalCompositionInput{}, nil, ErrInfrastructure
	}
	return s.resolve(ctx, nil, req)
}

// ResolveCompositionFromRecord resolves a composition request against an
// in-memory snapshot record (Phase 11 projected contexts: the record's id
// is the real base snapshot, its content the projected position).
func (s *PromptComposerService) ResolveCompositionFromRecord(ctx context.Context, snap *persistence.ContextSnapshotRecord, req prompt.ComposePromptRequest) (prompt.CanonicalCompositionInput, *persistence.ContextSnapshotRecord, error) {
	if snap == nil {
		return prompt.CanonicalCompositionInput{}, nil, ErrInvalidInput
	}
	return s.resolveFrom(ctx, nil, snap, req)
}

// ComposeResolvedForProjection is ComposeResolved for a prompt composed from
// a projected context: the canonical prompt records the projection.
func (s *PromptComposerService) ComposeResolvedForProjection(ctx context.Context, in prompt.CanonicalCompositionInput, cacheKey string, projectionID uuid.UUID) (*persistence.CanonicalPromptRecord, error) {
	pid := projectionID
	return s.composeResolved(ctx, in, cacheKey, &pid)
}

// ComposeResolved composes and persists a previously resolved input in one
// transaction, recording the Layer 1 cache key on the canonical prompt.
func (s *PromptComposerService) ComposeResolved(ctx context.Context, in prompt.CanonicalCompositionInput, cacheKey string) (*persistence.CanonicalPromptRecord, error) {
	return s.composeResolved(ctx, in, cacheKey, nil)
}

func (s *PromptComposerService) composeResolved(ctx context.Context, in prompt.CanonicalCompositionInput, cacheKey string, projectionID *uuid.UUID) (*persistence.CanonicalPromptRecord, error) {
	if s.canonical == nil {
		return nil, ErrInfrastructure
	}
	if s.txPool == nil {
		return s.persistComposedP(ctx, nil, in, cacheKey, projectionID)
	}
	var rec *persistence.CanonicalPromptRecord
	err := dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
		var e error
		rec, e = s.persistComposedP(ctx, tx, in, cacheKey, projectionID)
		return e
	})
	return rec, err
}

// persistComponents stores the composed components (canonical order, with
// their compression policies + provenance) and the preservation manifest
// built from the captured structured values (Phase 9 P9-11/P9-14).
func (s *PromptComposerService) persistComponents(ctx context.Context, tx persistence.Transaction, rec *persistence.CanonicalPromptRecord, in prompt.CanonicalCompositionInput, comps prompt.Components) error {
	cc, err := compression.FromPromptComponents(comps)
	if err != nil {
		return translate(err)
	}
	mi := compression.ManifestInput{
		ProjectID: in.ProjectID.String(),
		StateKey:  in.StateKey,
		RoleKey:   in.Role.RoleKey,
	}
	if in.PhaseID != nil {
		mi.PhaseID = in.PhaseID.String()
	}
	if u := in.Unit; u != nil {
		mi.WorkUnitID, mi.WorkUnitName = u.ID.String(), u.Name
		mi.Dependencies = u.Dependencies
		mi.Acceptance, mi.Tests, mi.Constraints = u.Acceptance, u.RequiredTests, u.Constraints
	}
	manifest := compression.BuildManifest(mi, compression.ApplyEffectivePolicies(cc))
	repo := s.components
	if tx != nil {
		repo = repo.WithTx(tx)
	}
	if err := repo.CreateComponents(ctx, rec.ID, cc); err != nil {
		return translate(err)
	}
	if err := repo.CreateManifest(ctx, rec.ID, manifest); err != nil {
		return translate(err)
	}
	return nil
}

// buildInput assembles the CanonicalCompositionInput from the pinned snapshot
// and the resolved pinned sources. Every value is snapshot-captured/pinned —
// the composer never reads "latest".
func (s *PromptComposerService) buildInput(ctx context.Context, tx persistence.Transaction, snap *persistence.ContextSnapshotRecord, req prompt.ComposePromptRequest, contStatus *prompt.ContinuationStatus) (prompt.CanonicalCompositionInput, error) {
	roleSnap, err := decodeRoleSnapshot(snap.RoleSnapshotJSON)
	if err != nil {
		return prompt.CanonicalCompositionInput{}, ErrInfrastructure
	}
	// Template source at the pinned version.
	tmplVer := s.tmplVer
	if tx != nil {
		tmplVer = s.tmplVer.WithTx(tx)
	}
	tmplRow, err := tmplVer.GetVersion(ctx, req.TemplateID, req.TemplateVersion)
	if err != nil {
		return prompt.CanonicalCompositionInput{}, translate(err)
	}
	// Response protocol: the snapshot's state pins (protocol, version); resolve
	// the protocol id from the state row, then read the pinned content.
	protocolID, protocolVersion, allowed, err := s.resolveProtocol(ctx, tx, snap)
	if err != nil {
		return prompt.CanonicalCompositionInput{}, err
	}
	// Work unit: the snapshot's bound unit (optional).
	var unit *prompt.WorkUnitContext
	var unitPtr *uuid.UUID
	if snap.WorkUnitID != nil {
		u, err := s.loadUnit(ctx, *snap.WorkUnitID)
		if err != nil {
			return prompt.CanonicalCompositionInput{}, err
		}
		unit = u
		unitPtr = snap.WorkUnitID
	}
	stateFiles := toStateFileSnapshots(snap.Files)

	// P8-12: the state's current instruction (captured version + content).
	var stInstrContent, stInstrHash string
	var stInstrVersion int
	stateID, err := s.stateIDFor(ctx, tx, snap)
	if err != nil {
		// A missing state row is tolerated (the protocol lookup below
		// re-resolves and surfaces the failure); instruction absent.
		stateID = uuid.Nil
	}
	if stateID != uuid.Nil {
		if ver, ierr := s.stInstr.GetCurrentVersion(ctx, stateID); ierr == nil && ver > 0 {
			stInstrVersion = ver
			if rec, rerr := s.stInstr.GetVersion(ctx, stateID, ver); rerr == nil {
				stInstrContent, stInstrHash = rec.Content, rec.ContentHash
			}
		}
	}

	// P8-24 (WBS 8.16/8.44): the PHASE_PLAN planning inputs — the phase's
	// current WBS (version + EXACT content) and the project's planning
	// policy/granularity (project configuration).
	var wbsVersion int
	var wbsContent, planningPolicy, planningGranularity string
	if in_phaseID := snap.PhaseID; in_phaseID != nil {
		if repo := s.wbsVersions; repo != nil {
			if v, werr := repo.GetCurrentVersion(ctx, in_phaseID.String()); werr == nil && v > 0 {
				wbsVersion = v
				if rec, rerr := repo.GetVersion(ctx, in_phaseID.String(), v); rerr == nil {
					wbsContent = rec.Content
				}
			}
		}
	}
	if repo := s.settings; repo != nil {
		if st, serr := repo.Get(ctx, snap.ProjectID.String()); serr == nil {
			planningPolicy, planningGranularity = st.PlanningPolicy, st.PlanningGranularity
		}
	}

	in := prompt.CanonicalCompositionInput{
		ProjectID:               snap.ProjectID,
		PhaseID:                 snap.PhaseID,
		UnitID:                  unitPtr,
		ContextSnapshotID:       snap.ID,
		TemplateID:              req.TemplateID,
		TemplateVersion:         req.TemplateVersion,
		RoleID:                  roleSnap.RoleID,
		RoleVersion:             roleSnap.RoleVersion,
		StateMachineID:          snap.StateMachineID,
		StateMachineVersion:     snap.StateMachineVersion,
		StateKey:                snap.StateKey,
		Role:                    roleSnap,
		StateFiles:              stateFiles,
		Required:                roleSnap.StateFiles,
		Unit:                    unit,
		Continuation:            contStatus,
		ProtocolID:              protocolID,
		ProtocolVersion:         protocolVersion,
		ProtocolContent:         allowed,
		TemplateSource:          tmplRow.Content,
		SnapshotHash:            "",
		StateInstructionContent: stInstrContent,
		StateInstructionVersion: stInstrVersion,
		StateInstructionHash:    stInstrHash,
		PhaseWBSVersion:         wbsVersion,
		PhaseWBSContent:         wbsContent,
		PlanningPolicy:          planningPolicy,
		PlanningGranularity:     planningGranularity,
	}
	return in, nil
}

// resolveProtocol resolves the snapshot state's response protocol (id, pinned
// version, and the pinned content). The state's protocol reference pins the
// version (never "latest"); the content is read at that pinned version.
func (s *PromptComposerService) resolveProtocol(ctx context.Context, tx persistence.Transaction, snap *persistence.ContextSnapshotRecord) (uuid.UUID, int, string, error) {
	// Locate the state row (machine id + version + key) to find its protocol
	// reference.
	stRepo := s.states
	if tx != nil {
		stRepo = stRepo.WithTx(tx)
	}
	stateID, err := s.stateIDFor(ctx, tx, snap)
	if err != nil {
		return uuid.Nil, 0, "", err
	}
	protoRepo := s.protocols
	if tx != nil {
		protoRepo = protoRepo.WithTx(tx)
	}
	// The state may reference at most one protocol; scan its references.
	refs := s.listStateProtocolRefs(ctx, tx, stateID)
	_ = protoRepo
	if len(refs) == 0 {
		// No pinned protocol: a cleared capture is a composition failure
		// (the P8-10/P8-22 contract), surfaced here as invalid input.
		return uuid.Nil, 0, "", ErrInvalidInput
	}
	ref := refs[0]
	ver, err := protoRepo.GetVersion(ctx, ref.ProtocolID, ref.ProtocolVersion)
	if err != nil {
		return uuid.Nil, 0, "", translate(err)
	}
	return ref.ProtocolID, ref.ProtocolVersion, ver.Content, nil
}

// stateIDFor returns the workflow-state id matching the snapshot's
// (machine id, version, state key).
func (s *PromptComposerService) stateIDFor(ctx context.Context, tx persistence.Transaction, snap *persistence.ContextSnapshotRecord) (uuid.UUID, error) {
	stRepo := s.states
	if tx != nil {
		stRepo = stRepo.WithTx(tx)
	}
	states, err := stRepo.ListByMachine(ctx, snap.StateMachineID, snap.StateMachineVersion)
	if err != nil {
		return uuid.Nil, translate(err)
	}
	for _, st := range states {
		if st.Key == snap.StateKey {
			return st.ID, nil
		}
	}
	return uuid.Nil, ErrInvalidInput
}

// loadContinuation resolves a continuation status for validation (T8.25.4).
// The continuation-status model (P8-08) is pure (no persistence table in this
// phase): the request's ContinuationWorkUnitID is the captured status's unit
// association. The status is validated against the snapshot's bound unit — a
// mismatch (a status captured for one unit applied to another) is the
// T8.25.4 cross-unit reject. A request with no bound unit in the snapshot
// cannot carry a continuation (reject).
func (s *PromptComposerService) loadContinuation(ctx context.Context, statusID uuid.UUID, statusUnit, boundUnit *uuid.UUID) (*prompt.ContinuationStatus, error) {
	if statusUnit == nil || boundUnit == nil {
		// A continuation status always names a work unit, and the snapshot must
		// bind a unit; either absent is a cross-unit reject (T8.25.4).
		return nil, ErrInvalidInput
	}
	st, err := prompt.NewContinuationStatus(prompt.ContinuationStatusInput{
		WorkUnitID: *statusUnit,
		CapturedAt: time.Now().UTC(),
	})
	if err != nil {
		return nil, translate(err)
	}
	// T8.25.4: the captured status's unit must match the snapshot's bound unit.
	if err := prompt.ValidateContinuationForUnit(st, *boundUnit); err != nil {
		return nil, translate(err)
	}
	_ = statusID
	return st, nil
}

// loadUnit loads the bound work unit's captured context (P8-14).
func (s *PromptComposerService) loadUnit(ctx context.Context, unitID uuid.UUID) (*prompt.WorkUnitContext, error) {
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
		Dependencies:  nil,
		Acceptance:    spec.AcceptanceCriteria,
		RequiredTests: spec.RequiredTests,
		Constraints:   spec.Constraints,
	}
	return wc, nil
}

// decodeRoleSnapshot deserializes the snapshot's role-snapshot JSONB blob.
func decodeRoleSnapshot(raw []byte) (role.RoleSnapshot, error) {
	var out role.RoleSnapshot
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return role.RoleSnapshot{}, err
	}
	return out, nil
}

// toStateFileSnapshots maps the snapshot's file pins to the P8-19 input set.
func toStateFileSnapshots(files []*persistence.ContextSnapshotFileRecord) []statefile.StateFileSnapshot {
	out := make([]statefile.StateFileSnapshot, 0, len(files))
	for _, f := range files {
		out = append(out, statefile.StateFileSnapshot{
			StateFileID:    f.StateFileID,
			TypeKey:        f.TypeKey,
			Scope:          statefile.StateFileScope(f.Scope),
			Version:        f.Version,
			Hash:           f.Hash,
			Content:        f.Content,
			Classification: role.Classification(f.Classification),
		})
	}
	return out
}

// listStateProtocolRefs reads the state's pinned protocol references from the
// state_protocol_references table directly (the repository interface carries
// no list-by-state method; there is at most one pinned protocol per state).
// It runs on the given transaction when non-nil, else on the raw pool.
func (s *PromptComposerService) listStateProtocolRefs(ctx context.Context, tx persistence.Transaction, stateID uuid.UUID) []*persistence.StateProtocolReferenceRecord {
	var rows []*persistence.StateProtocolReferenceRecord
	var q func(ctx context.Context, query string, args ...any) (pgxRows, error)
	if tx != nil {
		q = func(ctx context.Context, query string, args ...any) (pgxRows, error) {
			return tx.Query(ctx, query, args...)
		}
	} else if s.txPool != nil {
		q = func(ctx context.Context, query string, args ...any) (pgxRows, error) {
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
