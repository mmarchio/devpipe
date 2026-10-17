package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/cache"
	"overseer/internal/compression"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/statefile"
)

// Phase 10 (WBS 10.14-10.17, 10.29-10.34, 10.54-10.57, 10.69): the prompt
// cache service. PrepareForContext is the context-level entry point:
//
//	resolve composition inputs → semantic cache input → key
//	→ Layer 3 lookup (integrity-checked) ─HIT→ worker prompt (no composition, no provider)
//	→ MISS: BUILDING lease → Layer 1 canonical reuse | compose
//	        → Phase 9 PrepareWorkerPrompt (Layer 2 compression reuse)
//	        → VALID entry (or FAILED + negative window)
//
// PostgreSQL is authoritative; the in-process single-flight is acceleration
// only. Cache hits never mutate workflow / WBS state.

// Continuation modes (WBS 10.57).
const (
	ContinuationSameSession  = "SAME_SESSION"
	ContinuationResetSession = "RESET_SESSION"
)

// Prompt-cache service errors.
var (
	// ErrCacheSuppressed: a recent transient failure for the same key is
	// inside its negative-cache window (WBS 10.34).
	ErrCacheSuppressed = errors.New("service: prompt preparation suppressed by the negative cache")
	// ErrCacheUnavailable: the cache layer failed and bypass is disabled.
	ErrCacheUnavailable = errors.New("service: prompt cache unavailable")
)

// PrepareRequest is a context-level preparation request.
type PrepareRequest struct {
	SnapshotID      uuid.UUID
	TemplateID      uuid.UUID
	TemplateVersion int

	// Continuation optionally carries the captured worker status.
	Continuation *prompt.ContinuationStatusInput
	// ContinuationMode is SAME_SESSION or RESET_SESSION (continuations).
	ContinuationMode string

	// Source records how the entry is created (ON_DEMAND / PRECOMPUTED).
	Source cache.EntrySource

	// ResultReceivedAt, when set, is when the worker result that led to this
	// preparation was received; the transition latency (result received →
	// prompt ready) is recorded (WBS 11.53).
	ResultReceivedAt *time.Time
}

// PrepareResult is the outcome of a context-level preparation.
type PrepareResult struct {
	WorkerPrompt *compression.WorkerPrompt
	Entry        *cache.PromptCacheEntry

	CacheKey     string
	CanonicalKey string

	// Hit reports a Layer 3 (worker prompt) hit.
	Hit bool
	// Layers lists every layer reused during this preparation.
	Layers []cache.Layer

	CanonicalPromptID uuid.UUID
	Run               *compression.Run

	Bypassed     bool
	BypassReason string
}

// cacheInput is the derived cache identity of one request.
type cacheInput struct {
	input        cache.PromptCacheInput
	metadata     cache.KeySourceMetadata
	key          string
	canonicalKey string
	composition  prompt.CanonicalCompositionInput
	snapshot     *persistence.ContextSnapshotRecord
	mode         string

	// Phase 11: a projected preparation (precompute) — composed canonical
	// prompts record the projection; lookups do not count as uses.
	projectionID *uuid.UUID
	precompute   bool
}

// PromptCacheService implements the Phase 10 prompt cache.
type PromptCacheService struct {
	composer     *PromptComposerService
	compression  *CompressionService
	projSettings persistence.SettingsRepository
	entries      persistence.PromptCacheRepository
	events       persistence.PromptCacheEventRepository
	fragments    persistence.PromptFragmentRepository
	settings     persistence.PromptCacheSettingsRepository
	canonical    persistence.CanonicalPromptRepository
	components   persistence.CanonicalPromptComponentRepository
	artifacts    persistence.CompressionRepository

	owner        string
	now          func() time.Time
	pollInterval time.Duration
	flight       compression.SingleFlight
	// referenced reports execution-history references (WBS 10.35); no
	// execution history exists before Phase 12.
	referenced func(uuid.UUID) bool

	// Phase 11: placeholder templates (Decision 5) and latency records.
	templates persistence.PrecomputedTemplateRepository
	latencies persistence.TransitionLatencyRepository
}

// WithPrecompute wires the Phase 11 placeholder-template store and the
// transition-latency store.
func (s *PromptCacheService) WithPrecompute(templates persistence.PrecomputedTemplateRepository, latencies persistence.TransitionLatencyRepository) *PromptCacheService {
	s.templates, s.latencies = templates, latencies
	return s
}

// NewPromptCacheService wires the service. owner identifies this service
// instance for build leases.
func NewPromptCacheService(
	composer *PromptComposerService,
	compressionSvc *CompressionService,
	projSettings persistence.SettingsRepository,
	entries persistence.PromptCacheRepository,
	events persistence.PromptCacheEventRepository,
	fragments persistence.PromptFragmentRepository,
	settings persistence.PromptCacheSettingsRepository,
	canonical persistence.CanonicalPromptRepository,
	components persistence.CanonicalPromptComponentRepository,
	artifacts persistence.CompressionRepository,
) *PromptCacheService {
	return &PromptCacheService{
		composer: composer, compression: compressionSvc, projSettings: projSettings,
		entries: entries, events: events, fragments: fragments, settings: settings,
		canonical: canonical, components: components, artifacts: artifacts,
		owner:        "overseer-" + uuid.NewString(),
		now:          func() time.Time { return time.Now().UTC() },
		pollInterval: 50 * time.Millisecond,
		referenced:   func(uuid.UUID) bool { return false },
	}
}

// WithClock overrides the clock (tests).
func (s *PromptCacheService) WithClock(now func() time.Time) *PromptCacheService {
	s.now = now
	return s
}

// WithOwner overrides the lease owner id (tests).
func (s *PromptCacheService) WithOwner(owner string) *PromptCacheService {
	s.owner = owner
	return s
}

// WithEntries replaces the entry repository (tests: failure isolation).
func (s *PromptCacheService) WithEntries(r persistence.PromptCacheRepository) *PromptCacheService {
	s.entries = r
	return s
}

// Owner returns this instance's lease owner id.
func (s *PromptCacheService) Owner() string { return s.owner }

// ---------------------------------------------------------------------------
// Cache input (WBS 10.2-10.12, 10.19-10.25).

func (s *PromptCacheService) composeRequest(req PrepareRequest) prompt.ComposePromptRequest {
	cr := prompt.ComposePromptRequest{ContextSnapshotID: req.SnapshotID, TemplateID: req.TemplateID, TemplateVersion: req.TemplateVersion}
	if c := req.Continuation; c != nil {
		sid := uuid.NewSHA1(uuid.NameSpaceOID, []byte(cache.ContinuationStatusHash(*c)))
		unit := c.WorkUnitID
		cp := *c
		cr.ContinuationStatusID, cr.ContinuationWorkUnitID, cr.CapturedStatus = &sid, &unit, &cp
	}
	return cr
}

// buildCacheInput resolves the composition inputs (the composer's own
// resolution — no drift) and derives the semantic cache input + metadata.
func (s *PromptCacheService) buildCacheInput(ctx context.Context, projectID uuid.UUID, req PrepareRequest) (*cacheInput, error) {
	if req.SnapshotID == uuid.Nil || req.TemplateID == uuid.Nil || req.TemplateVersion <= 0 {
		return nil, ErrInvalidInput
	}
	in, snap, err := s.composer.ResolveComposition(ctx, s.composeRequest(req))
	if err != nil {
		return nil, err
	}
	if snap.ProjectID != projectID {
		return nil, ErrNotFound
	}
	return s.inputFromResolved(ctx, projectID, in, snap, req.ContinuationMode)
}

// inputFromResolved derives the semantic cache input + metadata + keys from
// an already resolved composition input and snapshot record (a persisted
// snapshot, or a Phase 11 projected record).
func (s *PromptCacheService) inputFromResolved(ctx context.Context, projectID uuid.UUID, in prompt.CanonicalCompositionInput, snap *persistence.ContextSnapshotRecord, mode string) (*cacheInput, error) {
	req := PrepareRequest{ContinuationMode: mode}
	ps, err := s.projSettings.Get(ctx, projectID.String())
	if err != nil {
		return nil, translate(err)
	}
	cst, instr, compHash, err := s.compression.ConfigIdentity(ctx, projectID)
	if err != nil {
		return nil, err
	}
	roleSnap, err := decodeRoleSnapshot(snap.RoleSnapshotJSON)
	if err != nil {
		return nil, ErrInfrastructure
	}
	files := toStateFileSnapshots(snap.Files)
	es := statefile.ExecutionContextSnapshot{
		ID: snap.ID, ProjectID: snap.ProjectID, PhaseID: snap.PhaseID, WorkUnitID: snap.WorkUnitID,
		RuntimeStateID: snap.RuntimeStateID, RuntimeStateVersion: snap.RuntimeStateVersion,
		StateMachineID: snap.StateMachineID, StateMachineVersion: snap.StateMachineVersion,
		StateKey: snap.StateKey, RoleSnapshot: roleSnap, StateFiles: files,
	}
	ci := cache.PromptCacheInput{
		ProjectID:             projectID,
		ProjectConfigHash:     cache.ProjectConfigHash(*ps),
		ContextSnapshotHash:   es.SemanticHash(), // schema v2: runtime-independent
		StateMachineID:        snap.StateMachineID,
		StateMachineVersion:   snap.StateMachineVersion,
		StateKey:              snap.StateKey,
		RoleHash:              cache.RoleHash(in.Role),
		TemplateHash:          cache.VersionedHash("template", in.TemplateID, in.TemplateVersion, in.TemplateSource),
		ResponseProtocolHash:  cache.VersionedHash("response_protocol", in.ProtocolID, in.ProtocolVersion, in.ProtocolContent),
		CompressionConfigHash: compHash,
	}
	if in.StateInstructionContent != "" {
		ci.StateInstructionHash = cache.VersionedHash("state_instruction", in.StateMachineID, in.StateInstructionVersion, in.StateInstructionContent)
	}
	if in.Unit != nil {
		ci.WorkUnitSpecHash = cache.WorkUnitSpecHash(*in.Unit)
	}
	wbsDetail := ""
	if in.StateKey == "PHASE_PLAN" {
		phase := uuid.Nil
		if in.PhaseID != nil {
			phase = *in.PhaseID
		}
		ci.PhaseWBSHash = cache.VersionedHash("phase_wbs", phase, in.PhaseWBSVersion, in.PhaseWBSContent)
		ci.PlanningConfigHash = cache.PlanningConfigHash(in.PlanningPolicy, in.PlanningGranularity)
		wbsDetail = fmt.Sprintf("WBS v%d", in.PhaseWBSVersion)
	}
	if c := in.Continuation; c != nil {
		h := cache.ContinuationStatusHash(prompt.ContinuationStatusInput{
			WorkUnitID: c.WorkUnitID, Summary: c.Summary, Completed: c.Completed,
			Remaining: c.Remaining, Blocking: c.Blocking, NextAction: c.NextAction,
		})
		ci.ContinuationStatusHash = &h
		ci.PreparationPolicy = req.ContinuationMode
	}
	ci, err = cache.NewPromptCacheInput(ci)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	var fv []cache.FileVersion
	for _, f := range files {
		fv = append(fv, cache.FileVersion{TypeKey: f.TypeKey, Version: f.Version, Hash: f.Hash})
	}
	md := cache.BuildMetadata(ci, cache.Details{
		Role:             fmt.Sprintf("%s v%d", roleSnap.RoleKey, roleSnap.RoleVersion),
		Template:         fmt.Sprintf("template v%d", in.TemplateVersion),
		Protocol:         fmt.Sprintf("response protocol v%d", in.ProtocolVersion),
		Compression:      fmt.Sprintf("%s/%s %s target %d", cst.Provider, cst.Model, cst.Mode, cst.EffectiveTarget()),
		StateMachine:     fmt.Sprintf("v%d %s", snap.StateMachineVersion, snap.StateKey),
		StateInstruction: fmt.Sprintf("v%d", in.StateInstructionVersion),
		WBS:              wbsDetail,
		Policy:           ci.PreparationPolicy,
		Files:            fv,
	})
	_ = instr
	key, err := cache.CacheKey(ci, cache.CurrentSchema)
	if err != nil {
		return nil, err
	}
	ckey, err := cache.CanonicalKey(ci, cache.CurrentSchema)
	if err != nil {
		return nil, err
	}
	return &cacheInput{input: ci, metadata: md, key: key, canonicalKey: ckey, composition: in, snapshot: snap, mode: req.ContinuationMode}, nil
}

// ---------------------------------------------------------------------------
// Events (best effort: an event failure never breaks preparation).

func (s *PromptCacheService) emit(ctx context.Context, b *cacheInput, typ cache.EventType, layer cache.Layer, entryID *uuid.UUID, reason string, mut func(*cache.Event)) {
	if s.events == nil {
		return
	}
	ev := cache.NewEvent(b.input.ProjectID, typ, layer, b.key, s.now())
	if layer == cache.LayerCanonical {
		ev.CacheKey = b.canonicalKey
	}
	ev.EntryID, ev.StateKey, ev.WorkUnitID, ev.Reason = entryID, b.input.StateKey, b.snapshot.WorkUnitID, reason
	if mut != nil {
		mut(&ev)
	}
	_ = s.events.Append(context.WithoutCancel(ctx), ev)
}

func (s *PromptCacheService) emitPlain(ctx context.Context, projectID uuid.UUID, typ cache.EventType, e *cache.PromptCacheEntry, reason string) {
	if s.events == nil {
		return
	}
	ev := cache.NewEvent(projectID, typ, cache.LayerWorker, "", s.now())
	if e != nil {
		id := e.ID
		ev.EntryID, ev.CacheKey, ev.Layer, ev.StateKey, ev.WorkUnitID = &id, e.CacheKey, e.Layer, e.StateKey, e.WorkUnitID
	}
	ev.Reason = reason
	_ = s.events.Append(context.WithoutCancel(ctx), ev)
}

// ---------------------------------------------------------------------------
// Integrity (WBS 10.17/10.28/10.58).

// verify checks an entry's referential integrity (and, when current is
// given, that its stored semantic inputs equal the current ones). It
// returns the worker prompt or a corruption reason.
func (s *PromptCacheService) verify(ctx context.Context, projectID uuid.UUID, e *cache.PromptCacheEntry, current *cache.KeySourceMetadata) (*compression.WorkerPrompt, string, error) {
	if !e.Dispatchable() || e.CanonicalPromptID == nil {
		return nil, "entry is not a valid worker-prompt entry", nil
	}
	if e.ProjectID != projectID {
		return nil, "entry belongs to another project", nil
	}
	wp, err := s.artifacts.GetWorkerPrompt(ctx, *e.WorkerPromptID)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, "worker prompt missing", nil
	} else if err != nil {
		return nil, "", err
	}
	cp, err := s.canonical.Get(ctx, *e.CanonicalPromptID)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, "canonical prompt missing", nil
	} else if err != nil {
		return nil, "", err
	}
	if cp.ProjectID != projectID || wp.CanonicalPromptID != cp.ID {
		return nil, "artifact references another project or canonical prompt", nil
	}
	if wp.Hash != compression.WorkerPromptHash(wp.Content, wp.Source) {
		return nil, "worker prompt hash mismatch", nil
	}
	if e.CompressedPromptID != nil {
		c, err := s.artifacts.GetCompressedPrompt(ctx, *e.CompressedPromptID)
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, "compressed prompt missing", nil
		} else if err != nil {
			return nil, "", err
		}
		if c.ValidationStatus != compression.ValidationPassed || wp.CompressedPromptID == nil || *wp.CompressedPromptID != c.ID ||
			c.CanonicalPromptID != cp.ID || c.Hash != compression.CompressedPromptHash(c.Content) || c.Content != wp.Content {
			return nil, "compressed prompt does not match the worker prompt", nil
		}
	} else if wp.Source != compression.SourceCanonical || wp.Content != cp.Content {
		return nil, "canonical worker prompt does not match the canonical prompt", nil
	}
	if current != nil {
		if d := cache.Diff(e.Metadata, *current); !d.Equal() {
			return nil, "stored semantic inputs do not match the key (collision)", nil
		}
	}
	return wp, "", nil
}

// quarantine invalidates a corrupt entry and records the event.
func (s *PromptCacheService) quarantine(ctx context.Context, projectID uuid.UUID, e *cache.PromptCacheEntry, reason string) {
	if ok, err := s.entries.MarkInvalid(context.WithoutCancel(ctx), e.ID, cache.ReasonCorrupt+": "+reason); err == nil && ok {
		s.emitPlain(ctx, projectID, cache.EventCorrupt, e, reason)
	}
}

// ---------------------------------------------------------------------------
// Lookup (WBS 10.14).

// lookup returns a verified Layer 3 hit, or (nil, nil) on a miss. Errors
// are cache-layer failures.
func (s *PromptCacheService) lookup(ctx context.Context, b *cacheInput) (*PrepareResult, error) {
	if !b.precompute {
		s.emit(ctx, b, cache.EventLookup, cache.LayerWorker, nil, "", nil)
	}
	e, err := s.entries.GetValid(ctx, b.input.ProjectID, b.key, cache.LayerWorker)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.hit(ctx, b, e)
}

// hit verifies and records a Layer 3 hit; a corrupt entry is quarantined
// and reported as a miss (never dispatched).
func (s *PromptCacheService) hit(ctx context.Context, b *cacheInput, e *cache.PromptCacheEntry) (*PrepareResult, error) {
	wp, reason, err := s.verify(ctx, b.input.ProjectID, e, &b.metadata)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		s.quarantine(ctx, b.input.ProjectID, e, reason)
		return nil, nil
	}
	if b.precompute {
		// A precompute cache-first check is not a use of the entry.
		return &PrepareResult{WorkerPrompt: wp, Entry: e, CacheKey: b.key, CanonicalKey: b.canonicalKey, Hit: true,
			Layers: []cache.Layer{cache.LayerWorker}, CanonicalPromptID: *e.CanonicalPromptID}, nil
	}
	if err := s.entries.Touch(ctx, e.ID, s.now()); err != nil {
		return nil, err
	}
	id := e.ID
	s.emit(ctx, b, cache.EventHit, cache.LayerWorker, &id, "", func(ev *cache.Event) {
		ev.AvoidedCompressionCalls, ev.AvoidedTokens, ev.AvoidedLatency = e.CompressionCalls, e.CompressionInputTokens, e.BuildDuration
	})
	e.HitCount++
	return &PrepareResult{
		WorkerPrompt: wp, Entry: e, CacheKey: b.key, CanonicalKey: b.canonicalKey, Hit: true,
		Layers: []cache.Layer{cache.LayerWorker}, CanonicalPromptID: *e.CanonicalPromptID,
	}, nil
}

// missReason explains a miss from the newest entry of the same slot.
func (s *PromptCacheService) missReason(ctx context.Context, b *cacheInput) string {
	ref, err := s.entries.LatestInSlot(ctx, b.input.ProjectID, b.input.StateKey, b.snapshot.WorkUnitID, cache.LayerWorker)
	if err != nil {
		return "no previous entry"
	}
	return summarize(cache.Diff(ref.Metadata, b.metadata))
}

func summarize(d cache.MetadataDiff) string {
	if d.Equal() {
		return "previous entry not reusable"
	}
	var parts []string
	for _, c := range d.Changed {
		if strings.HasPrefix(c.Name, "file:") {
			parts = append(parts, c.Before+" → "+c.After)
		} else {
			parts = append(parts, c.Name+" changed")
		}
	}
	return strings.Join(parts, "; ")
}

// ---------------------------------------------------------------------------
// PrepareForContext (WBS 10.16).

// PrepareForContext prepares (or reuses) the worker-ready prompt for a
// context snapshot + template.
func (s *PromptCacheService) PrepareForContext(ctx context.Context, projectID uuid.UUID, req PrepareRequest) (*PrepareResult, error) {
	b, err := s.buildCacheInput(ctx, projectID, req)
	if err != nil {
		return nil, err
	}
	res, err := s.prepareResolved(ctx, b, req)
	if err == nil && res != nil && req.ResultReceivedAt != nil {
		s.recordLatency(ctx, b, res, *req.ResultReceivedAt)
	}
	return res, err
}

// PrepareForProjection prepares (or finds) the worker prompt of a Phase 11
// projected context through the exact same pipeline as an actual
// preparation — same keys, same composition, compression, validation and
// cache — with the entry marked PRECOMPUTED. A cache-first hit is reported
// without counting as a use. The runtime state never advances.
func (s *PromptCacheService) PrepareForProjection(ctx context.Context, projectID uuid.UUID, in prompt.CanonicalCompositionInput, rec *persistence.ContextSnapshotRecord, projectionID uuid.UUID) (*PrepareResult, error) {
	b, err := s.inputFromResolved(ctx, projectID, in, rec, "")
	if err != nil {
		return nil, err
	}
	pid := projectionID
	b.projectionID, b.precompute = &pid, true
	return s.prepareResolved(ctx, b, PrepareRequest{Source: cache.SourcePrecomputed})
}

// KeysForResolved returns the Layer 3 and Layer 1 keys of a resolved input
// (Phase 11 candidate identity / dedup / placeholder normalization).
func (s *PromptCacheService) KeysForResolved(ctx context.Context, projectID uuid.UUID, in prompt.CanonicalCompositionInput, rec *persistence.ContextSnapshotRecord) (string, string, error) {
	b, err := s.inputFromResolved(ctx, projectID, in, rec, "")
	if err != nil {
		return "", "", err
	}
	return b.key, b.canonicalKey, nil
}

func (s *PromptCacheService) prepareResolved(ctx context.Context, b *cacheInput, req PrepareRequest) (*PrepareResult, error) {
	projectID := b.input.ProjectID
	cs, _, err := s.settings.Get(ctx, projectID)
	if err != nil {
		return s.cacheFailure(ctx, b, cache.DefaultSettings(), err)
	}
	if b.precompute && !cs.Enabled {
		return nil, fmt.Errorf("%w: prompt cache disabled for the project", ErrCacheUnavailable)
	}
	if !cs.Enabled {
		return s.bypass(ctx, b, "prompt cache disabled for the project")
	}
	if b.mode == ContinuationSameSession && !cs.CacheSameSessionContinuation {
		return s.bypass(ctx, b, "same-session continuation caching disabled")
	}
	res, err := s.lookup(ctx, b)
	if err != nil {
		return s.cacheFailure(ctx, b, cs, err)
	}
	if res != nil {
		return res, nil
	}
	if f, err := s.entries.LatestFailed(ctx, projectID, b.key, cache.LayerWorker); err == nil && f.Suppresses(s.now()) {
		id := f.ID
		s.emit(ctx, b, cache.EventSuppressed, cache.LayerWorker, &id, f.ErrorKind, nil)
		return nil, fmt.Errorf("%w until %s: %s", ErrCacheSuppressed, f.RetryAfter.Format(time.RFC3339), f.ErrorMessage)
	}
	if !b.precompute {
		s.emit(ctx, b, cache.EventMiss, cache.LayerWorker, nil, s.missReason(ctx, b), nil)
	}
	// Precompute builds never share a flight with active preparation: a
	// speculative build deferred or preempted under pressure (WBS 18.95)
	// must not fail the active caller; the BUILDING lease still prevents
	// conflicting authoritative entries.
	flightKey := projectID.String() + "|" + b.key
	if b.precompute {
		flightKey += "|precompute"
	}
	v, err, _ := s.flight.Do(flightKey, func() (any, error) {
		return s.build(ctx, b, cs, req.Source)
	})
	if v == nil {
		return nil, err
	}
	return v.(*PrepareResult), err
}

// cacheFailure applies the WBS 10.69 isolation policy.
func (s *PromptCacheService) cacheFailure(ctx context.Context, b *cacheInput, cs cache.Settings, cause error) (*PrepareResult, error) {
	if !cs.BypassOnCacheError || b.precompute {
		return nil, fmt.Errorf("%w: %v", ErrCacheUnavailable, cause)
	}
	return s.bypass(ctx, b, "cache layer error: "+cause.Error())
}

// bypass prepares without reading or writing cache entries (recorded).
func (s *PromptCacheService) bypass(ctx context.Context, b *cacheInput, reason string) (*PrepareResult, error) {
	s.emit(ctx, b, cache.EventBypass, cache.LayerWorker, nil, reason, nil)
	cp, err := s.compose(ctx, b)
	if err != nil {
		return nil, err
	}
	// Fragment completion is recorded on every composition (Phase 11).
	s.recordFragments(ctx, b, cp.ID)
	pr, err := s.compression.PrepareWorkerPrompt(ctx, b.input.ProjectID, cp.ID)
	res := &PrepareResult{CacheKey: b.key, CanonicalKey: b.canonicalKey, CanonicalPromptID: cp.ID, Bypassed: true, BypassReason: reason}
	if pr != nil {
		res.WorkerPrompt, res.Run = pr.WorkerPrompt, pr.Run
	}
	return res, err
}

// build runs the miss path under a persistent BUILDING lease (WBS
// 10.30-10.32).
func (s *PromptCacheService) build(ctx context.Context, b *cacheInput, cs cache.Settings, source cache.EntrySource) (*PrepareResult, error) {
	entry, err := s.claim(ctx, b, cs)
	if err != nil {
		return s.cacheFailure(ctx, b, cs, err)
	}
	if entry.Status == cache.StatusValid {
		// Another builder finished first.
		res, err := s.hit(ctx, b, entry)
		if err != nil || res != nil {
			return res, err
		}
		return s.build(ctx, b, cs, source)
	}
	id := entry.ID
	s.emit(ctx, b, cache.EventBuildStarted, cache.LayerWorker, &id, "", nil)
	start := time.Now()
	res := &PrepareResult{CacheKey: b.key, CanonicalKey: b.canonicalKey}

	// Phase 11 Decision 5: a pending placeholder template for exactly this
	// context (all inputs equal except the ACTIVE_TASK, which was unknown
	// at precompute time) is resolved with the real value and re-validated.
	if done, err := s.resolvePlaceholder(ctx, b, entry, res, start, source); done || err != nil {
		return res, err
	}

	// Layer 1: reuse the canonical prompt when its semantic inputs are
	// unchanged (e.g. only the compression configuration changed).
	cp, err := s.canonical.FindByCacheKey(ctx, b.input.ProjectID, b.canonicalKey)
	switch {
	case err == nil:
		res.Layers = append(res.Layers, cache.LayerCanonical)
		cid := cp.ID
		s.emit(ctx, b, cache.EventHit, cache.LayerCanonical, &cid, "canonical prompt reused", nil)
	case errors.Is(err, persistence.ErrNotFound):
		if cp, err = s.compose(ctx, b); err != nil {
			s.fail(ctx, b, entry, err, nil)
			return nil, err
		}
		s.recordFragments(ctx, b, cp.ID)
	default:
		s.fail(ctx, b, entry, err, nil)
		return nil, translate(err)
	}
	res.CanonicalPromptID = cp.ID

	// Layer 2 + worker prompt (Phase 9).
	revoked := func(wp uuid.UUID) bool {
		r, err := s.entries.WorkerPromptRevoked(ctx, b.input.ProjectID, wp)
		return err != nil || r
	}
	pr, perr := s.compression.PrepareWorkerPromptExcluding(ctx, b.input.ProjectID, cp.ID, revoked)
	if pr != nil {
		res.Run = pr.Run
		if pr.Reused {
			res.Layers = append(res.Layers, cache.LayerCompression)
			s.emit(ctx, b, cache.EventHit, cache.LayerCompression, nil, "accepted compression reused", nil)
		}
	}
	if perr != nil {
		s.fail(ctx, b, entry, perr, pr)
		return nil, perr
	}
	wp := pr.WorkerPrompt
	// Every attempt was one provider call; a hit later avoids them all.
	calls, tokens := len(pr.Attempts), 0
	if wp.Source == compression.SourceCompressed {
		tokens = pr.Run.CanonicalTokens
	}
	src := source
	if src == "" {
		src = cache.SourceOnDemand
	}
	ok, err := s.entries.FinalizeValid(ctx, entry.ID, s.owner, s.now(), persistence.BuildResult{
		CanonicalPromptID: cp.ID, CompressedPromptID: wp.CompressedPromptID, WorkerPromptID: wp.ID,
		Metadata: b.metadata, Source: src, CompressionCalls: calls, CompressionInputTokens: tokens,
		BuildDuration: time.Since(start),
	})
	if err != nil {
		return s.cacheFailureAfterBuild(ctx, b, cs, res, wp, err)
	}
	res.WorkerPrompt = wp
	if !ok {
		// The lease was lost (expired and reclaimed); the prepared worker
		// prompt is still valid, but this instance does not own the entry.
		res.BypassReason = "build lease lost; entry owned by another builder"
		return res, nil
	}
	stored, err := s.entries.Get(ctx, entry.ID)
	if err == nil {
		res.Entry = stored
	}
	s.emit(ctx, b, cache.EventStore, cache.LayerWorker, &id, "", nil)
	s.emit(ctx, b, cache.EventBuildCompleted, cache.LayerWorker, &id, "", nil)
	return res, nil
}

func (s *PromptCacheService) cacheFailureAfterBuild(ctx context.Context, b *cacheInput, cs cache.Settings, res *PrepareResult, wp *compression.WorkerPrompt, cause error) (*PrepareResult, error) {
	if !cs.BypassOnCacheError {
		return nil, fmt.Errorf("%w: %v", ErrCacheUnavailable, cause)
	}
	s.emit(ctx, b, cache.EventBypass, cache.LayerWorker, nil, "cache store failed: "+cause.Error(), nil)
	res.WorkerPrompt, res.Bypassed, res.BypassReason = wp, true, "cache store failed"
	return res, nil
}

// claim inserts a BUILDING entry, or resolves the contention: a VALID
// winner is returned for the hit path; an active foreign lease is waited
// on (bounded by the lease); an expired lease is reclaimed.
func (s *PromptCacheService) claim(ctx context.Context, b *cacheInput, cs cache.Settings) (*cache.PromptCacheEntry, error) {
	for {
		be, err := cache.NewBuildingEntry(b.input.ProjectID, b.key, cache.LayerWorker, s.owner, cs.Lease, s.now())
		if err != nil {
			return nil, err
		}
		be.StateKey, be.PhaseID, be.WorkUnitID, be.Metadata = b.input.StateKey, b.snapshot.PhaseID, b.snapshot.WorkUnitID, b.metadata
		w, created, err := s.entries.Insert(ctx, &be)
		if errors.Is(err, persistence.ErrConflict) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if created || w.Status == cache.StatusValid {
			return w, nil
		}
		if w.BuildOwner == s.owner {
			return w, nil
		}
		if !w.LeaseActive(s.now()) {
			exp := s.now().Add(cs.Lease)
			ok, err := s.entries.ReclaimLease(ctx, w.ID, s.owner, exp, s.now())
			if err != nil {
				return nil, err
			}
			if ok {
				w.BuildOwner, w.LeaseExpiresAt = s.owner, &exp
				return w, nil
			}
			continue
		}
		// Another builder holds an active lease: wait for its outcome.
		t := time.NewTimer(s.pollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// fail finalizes the BUILDING entry as FAILED; a transient provider failure
// opens the negative-cache window (WBS 10.33/10.34).
func (s *PromptCacheService) fail(ctx context.Context, b *cacheInput, entry *cache.PromptCacheEntry, cause error, pr *PreparationResult) {
	kind := ""
	if pr != nil && len(pr.Attempts) > 0 {
		kind = string(pr.Attempts[len(pr.Attempts)-1].ErrorKind)
	}
	cs, _, _ := s.settings.Get(ctx, b.input.ProjectID)
	var retry *time.Time
	// A speculative (precompute) failure never suppresses a later on-demand
	// preparation (WBS 11.49).
	if cache.TransientErrorKinds[kind] && cs.NegativeWindow > 0 && !b.precompute {
		t := s.now().Add(cs.NegativeWindow)
		retry = &t
	}
	msg := cause.Error()
	if len(msg) > 500 {
		msg = msg[:500]
	}
	if ok, err := s.entries.FinalizeFailed(context.WithoutCancel(ctx), entry.ID, s.owner, s.now(), kind, msg, retry, b.metadata); err == nil && ok {
		id := entry.ID
		s.emit(ctx, b, cache.EventBuildFailed, cache.LayerWorker, &id, kind, nil)
	}
}

// recordFragments stores the static fragments of a newly composed prompt
// (WBS 10.45-10.48); an existing fragment is a fragment hit. Best effort.
func (s *PromptCacheService) recordFragments(ctx context.Context, b *cacheInput, canonicalID uuid.UUID) {
	if s.fragments == nil || s.components == nil {
		return
	}
	comps, err := s.components.ListComponents(ctx, canonicalID)
	if err != nil || len(comps) == 0 {
		return
	}
	pcs := make(prompt.Components, 0, len(comps))
	for _, c := range comps {
		pcs = append(pcs, prompt.PromptComponent{Type: c.Type, Source: c.Source, Order: c.Ordinal, Content: c.Content,
			SourceID: c.SourceID, SourceVersion: c.SourceVersion, SourceHash: c.SourceHash, Policy: c.Policy})
	}
	for _, f := range cache.BuildFragments(b.input.ProjectID, pcs, s.now()) {
		f := f
		stored, created, err := s.fragments.StoreIfAbsent(ctx, &f)
		if err != nil || created {
			continue
		}
		_ = s.fragments.IncrementHit(ctx, stored.ID)
		fid := stored.ID
		s.emit(ctx, b, cache.EventHit, cache.LayerFragment, &fid, string(stored.Kind), func(ev *cache.Event) { ev.CacheKey = stored.FragmentKey })
	}
}
