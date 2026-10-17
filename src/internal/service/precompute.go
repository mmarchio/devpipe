package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"overseer/internal/cache"
	"overseer/internal/compression"
	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/precompute"
	"overseer/internal/project"
	"overseer/internal/prompt"
	"overseer/internal/resilience"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// Phase 11: future-state prompt precomputation. The service coordinates the
// Phase 6 graph, Phase 7 snapshot assembly, Phase 8 composer, Phase 9
// compression and Phase 10 cache — it never composes, compresses,
// validates or keys prompts itself (WBS 11.1), and never performs workflow
// actions: no worker call, no state transition, no work-unit status, no
// ACTIVE_TASK / state-file / runtime write (WBS 11.35).

// Precompute errors.
var (
	ErrPrecomputeDisabled = errors.New("service: precomputation disabled for the project")
	ErrProjectNotActive   = errors.New("service: project is paused, cancelled or finished")
)

// PrecomputeDeps wires the precompute service.
type PrecomputeDeps struct {
	Settings     persistence.PrecomputeSettingsRepository
	Jobs         persistence.PrecomputeJobRepository
	Projections  persistence.ProjectionRepository
	Manifests    persistence.FragmentManifestRepository
	Templates    persistence.PrecomputedTemplateRepository
	Latencies    persistence.TransitionLatencyRepository
	Events       persistence.PromptCacheEventRepository
	Fragments    persistence.PromptFragmentRepository
	CacheEntries persistence.PromptCacheRepository

	Projects    persistence.ProjectRepository
	Runtime     persistence.RuntimeStateRepository
	States      persistence.WorkflowStateRepository
	Transitions persistence.StateTransitionRepository
	PromptTmpl  persistence.PromptTemplateRepository
	TmplVer     persistence.PromptTemplateVersionRepository
	Snapshots   persistence.ContextSnapshotRepository
	FileVersion persistence.StateFileVersionRepository
	WBSVersions persistence.WBSVersionRepository
	Phases      *dbrepo.PhaseRepository
	Plans       *dbrepo.PlanRepository
	Units       *dbrepo.WorkUnitRepository

	SnapshotSvc   *ExecutionSnapshotService
	ScopeRes      *StateFileScopeResolutionService
	ActiveTask    *ActiveTaskService
	Composer      *PromptComposerService
	Cache         *PromptCacheService
	Compression   *CompressionService
	ProviderLocal func(provider string) bool
}

// PrecomputeService implements WarmUpcomingPrompts and the Phase 11 hooks.
type PrecomputeService struct {
	d        PrecomputeDeps
	registry precompute.Registry
	tracker  *precompute.ActivityTracker
	owner    string
	now      func() time.Time
	// staleAfter: a non-terminal job whose heartbeat is older is
	// reclassified on recovery (restart / dead instance).
	staleAfter time.Duration

	// beforeCandidate is a test synchronization hook (nil in production).
	beforeCandidate func(ctx context.Context, rec *precompute.CandidateRecord)

	mu      sync.Mutex
	running map[uuid.UUID]runningJob
	baseCtx context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

type runningJob struct {
	project uuid.UUID
	cancel  context.CancelFunc
}

// NewPrecomputeService builds the service (owner identifies this instance).
func NewPrecomputeService(d PrecomputeDeps) *PrecomputeService {
	ctx, cancel := context.WithCancel(context.Background())
	if d.ProviderLocal == nil {
		d.ProviderLocal = func(string) bool { return false }
	}
	return &PrecomputeService{
		d: d, registry: precompute.DefaultRegistry(), tracker: &precompute.ActivityTracker{},
		owner: "precompute-" + uuid.NewString(), now: func() time.Time { return time.Now().UTC() },
		staleAfter: time.Minute, running: map[uuid.UUID]runningJob{}, baseCtx: ctx, stop: cancel,
	}
}

// WithStaleAfter sets the recovery staleness threshold (tests).
func (s *PrecomputeService) WithStaleAfter(d time.Duration) *PrecomputeService {
	s.staleAfter = d
	return s
}

// WithBeforeCandidate installs a hook run before each candidate (tests).
func (s *PrecomputeService) WithBeforeCandidate(fn func(ctx context.Context, rec *precompute.CandidateRecord)) *PrecomputeService {
	s.beforeCandidate = fn
	return s
}

// WithOwner overrides the instance owner id (tests).
func (s *PrecomputeService) WithOwner(owner string) *PrecomputeService {
	s.owner = owner
	return s
}

// Owner returns the instance owner id.
func (s *PrecomputeService) Owner() string { return s.owner }

// Shutdown cancels running jobs and waits for them (bounded by ctx).
func (s *PrecomputeService) Shutdown(ctx context.Context) {
	s.stop()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// ---------------------------------------------------------------------------
// Settings (WBS 11.3).

// PrecomputeSettings returns the project's settings (defaults if unstored).
func (s *PrecomputeService) PrecomputeSettings(ctx context.Context, projectID uuid.UUID) (precompute.Settings, bool, error) {
	if _, err := s.project(ctx, projectID); err != nil {
		return precompute.Settings{}, false, err
	}
	st, stored, err := s.d.Settings.Get(ctx, projectID)
	if err != nil {
		return precompute.Settings{}, false, translate(err)
	}
	return st, stored, nil
}

// UpdatePrecomputeSettings validates and stores the settings.
func (s *PrecomputeService) UpdatePrecomputeSettings(ctx context.Context, projectID uuid.UUID, st precompute.Settings) (precompute.Settings, error) {
	if _, err := s.project(ctx, projectID); err != nil {
		return precompute.Settings{}, err
	}
	if err := st.Validate(); err != nil {
		return precompute.Settings{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if err := s.d.Settings.Upsert(ctx, projectID, st); err != nil {
		return precompute.Settings{}, translate(err)
	}
	return st, nil
}

func (s *PrecomputeService) project(ctx context.Context, projectID uuid.UUID) (*project.Project, error) {
	p, err := s.d.Projects.Get(ctx, projectID.String())
	if err != nil {
		return nil, translate(err)
	}
	return p, nil
}

func activeStatus(st project.ProjectStatus) bool {
	switch st {
	case project.StatusPAUSED, project.StatusCANCELLED, project.StatusCOMPLETED, project.StatusFAILED:
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Template resolution (P11-23): a state's prompt_template key → template
// (project scope, else global) → current version. Shared by actual
// (PrepareNext) and projected preparation.

type stateTemplate struct {
	ID      uuid.UUID
	Version int
	Key     string
	Purpose string
}

func (s *PrecomputeService) resolveStateTemplate(ctx context.Context, projectID uuid.UUID, key string) (*stateTemplate, error) {
	if strings.TrimSpace(key) == "" {
		return nil, persistence.ErrNotFound
	}
	pid := projectID
	rec, err := s.d.PromptTmpl.GetByKey(ctx, &pid, key)
	if errors.Is(err, persistence.ErrNotFound) {
		rec, err = s.d.PromptTmpl.GetByKey(ctx, nil, key)
	}
	if err != nil {
		return nil, err
	}
	if !rec.Enabled {
		return nil, persistence.ErrNotFound
	}
	ver, err := s.d.TmplVer.GetCurrentVersion(ctx, rec.ID)
	if err != nil {
		return nil, err
	}
	return &stateTemplate{ID: rec.ID, Version: ver.Version, Key: rec.Key, Purpose: rec.Purpose}, nil
}

// ---------------------------------------------------------------------------
// Discovery (WBS 11.5-11.22).

// plannedCandidate is a candidate plus its projection material.
type plannedCandidate struct {
	precompute.Candidate
	state      *persistence.WorkflowStateRecord
	template   *stateTemplate
	record     *persistence.ContextSnapshotRecord
	projection *precompute.ProjectedContext
	reason     string
}

type discovery struct {
	runtime      *persistence.RuntimeStateRecord
	current      *persistence.WorkflowStateRecord
	base         *persistence.ContextSnapshotRecord
	baseSemantic string
	graph        precompute.Graph
	candidates   []plannedCandidate
}

func (s *PrecomputeService) discover(ctx context.Context, projectID uuid.UUID, depth int, baseID *uuid.UUID, createBase bool) (*discovery, error) {
	rt, err := s.d.Runtime.Get(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	states, err := s.d.States.ListByMachine(ctx, rt.StateMachineID, rt.StateMachineVersion)
	if err != nil {
		return nil, translate(err)
	}
	edges, err := s.d.Transitions.ListByMachine(ctx, rt.StateMachineID, rt.StateMachineVersion)
	if err != nil {
		return nil, translate(err)
	}
	g := precompute.Graph{MachineVersion: rt.StateMachineVersion, States: map[string]precompute.StateNode{}}
	byID := map[uuid.UUID]*persistence.WorkflowStateRecord{}
	byKey := map[string]*persistence.WorkflowStateRecord{}
	for _, st := range states {
		byID[st.ID], byKey[st.Key] = st, st
		g.States[st.Key] = precompute.StateNode{Key: st.Key, Kind: st.Kind, Enabled: st.Enabled, Terminal: st.Terminal, TemplateKey: st.PromptTemplate}
	}
	minPrio := map[string]int{}
	for _, e := range edges {
		from, to := byID[e.FromStateID], byID[e.ToStateID]
		if from == nil || to == nil {
			continue
		}
		g.Edges = append(g.Edges, precompute.Edge{From: from.Key, To: to.Key, Outcome: e.Outcome, Priority: e.Priority, Enabled: e.Enabled})
		if p, ok := minPrio[from.Key]; !ok || e.Priority < p {
			minPrio[from.Key] = e.Priority
		}
	}
	cur := byID[rt.CurrentStateID]
	if cur == nil {
		return nil, ErrInvalidInput
	}
	base, err := s.baseSnapshot(ctx, projectID, rt, cur, baseID, createBase)
	if err != nil {
		return nil, err
	}
	baseSnap, err := recordToSnapshot(base)
	if err != nil {
		return nil, ErrInfrastructure
	}
	dsc := &discovery{runtime: rt, current: cur, base: base, baseSemantic: baseSnap.SemanticHash(), graph: g}

	// Reachable states of the same work unit (or phase-level states).
	firstEdge := map[string]bool{}
	for _, e := range g.Edges {
		if e.From == cur.Key && e.Enabled && e.Priority == minPrio[cur.Key] {
			firstEdge[e.To] = true
		}
	}
	for _, r := range g.Reachable(cur.Key, depth) {
		st := byKey[r.StateKey]
		prio := precompute.PrioritySameUnit
		switch {
		case r.Depth == 1 && firstEdge[r.StateKey]:
			prio = precompute.PriorityImmediate
		case r.Depth >= 3:
			prio = precompute.PriorityDistant
		}
		pc := s.plan(ctx, dsc, projectID, st, rt.PhaseID, rt.WorkUnitID, [][]string{r.Path}, r.Depth, prio, false, false)
		dsc.candidates = append(dsc.candidates, pc)
	}

	// Next executable work unit at the unit-entry state (WBS 11.17/11.31).
	if entry := unitEntryState(states); entry != nil && rt.PhaseID != nil {
		if next := s.nextUnit(ctx, projectID, rt.WorkUnitID); next != nil {
			id := next.ID
			pc := s.plan(ctx, dsc, projectID, entry, rt.PhaseID, &id, [][]string{{"NEXT_UNIT>" + entry.Key}}, 1, precompute.PriorityNextUnit, true, false)
			dsc.candidates = append(dsc.candidates, pc)
		}
	}

	// Next phase planning prompt (WBS 11.32-11.34), when enabled.
	if st, _, _ := s.d.Settings.Get(ctx, projectID); st.PlanNextPhase {
		if planState := stateWithTemplate(states, "PLAN_PHASE"); planState != nil {
			if nextPhase := s.nextPhaseWithWBS(ctx, projectID, rt.PhaseID); nextPhase != nil {
				pc := s.plan(ctx, dsc, projectID, planState, nextPhase, nil, [][]string{{"NEXT_PHASE>" + planState.Key}}, 1, precompute.PriorityPhaseBoundry, false, true)
				dsc.candidates = append(dsc.candidates, pc)
			}
		}
	}
	return dsc, nil
}

// unitEntryState is the state where a work unit starts: the state whose
// prompt template is IMPLEMENT_WORK_UNIT (lowest key), enabled.
func unitEntryState(states []*persistence.WorkflowStateRecord) *persistence.WorkflowStateRecord {
	return stateWithTemplate(states, "IMPLEMENT_WORK_UNIT")
}

func stateWithTemplate(states []*persistence.WorkflowStateRecord, key string) *persistence.WorkflowStateRecord {
	var out *persistence.WorkflowStateRecord
	for _, st := range states {
		if st.Enabled && st.PromptTemplate == key && (out == nil || st.Key < out.Key) {
			out = st
		}
	}
	return out
}

// nextUnit selects the next deterministically executable unit of the
// current phase's active plan (dependency-ready, lowest sequence),
// excluding the current unit. Read-only (T11.17.3).
func (s *PrecomputeService) nextUnit(ctx context.Context, projectID uuid.UUID, current *uuid.UUID) *precompute.EligibleUnit {
	phases, err := s.d.Phases.ListByProject(ctx, projectID)
	if err != nil {
		return nil
	}
	ph := wbs.CurrentPhase(phases)
	if ph == nil {
		return nil
	}
	plans, err := s.d.Plans.ListByPhase(ctx, ph.ID)
	if err != nil {
		return nil
	}
	plan := wbs.ActivePlan(ph.ID, plans)
	if plan == nil {
		return nil
	}
	units, err := s.d.Units.ListEligible(ctx, plan.ID)
	if err != nil {
		return nil
	}
	var el []precompute.EligibleUnit
	for _, u := range units {
		if current != nil && u.ID == *current {
			continue
		}
		el = append(el, precompute.EligibleUnit{ID: u.ID, Sequence: u.Sequence, Name: u.Name})
	}
	return precompute.SelectNextUnit(el)
}

// nextPhaseWithWBS returns the phase after the current one when it already
// has a WBS (planning prompt fully known, WBS 11.34).
func (s *PrecomputeService) nextPhaseWithWBS(ctx context.Context, projectID uuid.UUID, current *uuid.UUID) *uuid.UUID {
	if s.d.WBSVersions == nil {
		return nil
	}
	phases, err := s.d.Phases.ListByProject(ctx, projectID)
	if err != nil {
		return nil
	}
	sort.Slice(phases, func(i, j int) bool { return phases[i].Order < phases[j].Order })
	curOrder := -1
	for _, p := range phases {
		if current != nil && p.ID == *current {
			curOrder = p.Order
		}
	}
	if curOrder < 0 {
		return nil
	}
	for _, p := range phases {
		if p.Order > curOrder {
			if v, err := s.d.WBSVersions.GetCurrentVersion(ctx, p.ID.String()); err == nil && v > 0 {
				id := p.ID
				return &id
			}
			return nil
		}
	}
	return nil
}

func (s *PrecomputeService) baseSnapshot(ctx context.Context, projectID uuid.UUID, rt *persistence.RuntimeStateRecord, cur *persistence.WorkflowStateRecord, baseID *uuid.UUID, create bool) (*persistence.ContextSnapshotRecord, error) {
	if baseID != nil {
		rec, err := s.d.Snapshots.Get(ctx, *baseID)
		if err != nil {
			return nil, translate(err)
		}
		if rec.ProjectID != projectID {
			return nil, ErrNotFound
		}
		return rec, nil
	}
	snaps, err := s.d.Snapshots.ListByProject(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	var best *persistence.ContextSnapshotRecord
	for _, sn := range snaps {
		if sn.RuntimeStateVersion == rt.Version && sn.StateKey == cur.Key && sameID(sn.WorkUnitID, rt.WorkUnitID) &&
			(best == nil || sn.CreatedAt.After(best.CreatedAt)) {
			best = sn
		}
	}
	if best != nil {
		return s.d.Snapshots.Get(ctx, best.ID)
	}
	if !create {
		// Read-only callers (preview) assemble the current position in
		// memory instead of creating a snapshot.
		ps, err := s.d.SnapshotSvc.AssembleProjected(ctx, projectID, rt, cur.ID, rt.PhaseID, rt.WorkUnitID)
		if err != nil {
			return nil, err
		}
		// A deterministic synthetic id: preview never persists a projection,
		// so nothing can ever reference it.
		return SnapshotToRecord(ps.Snapshot, uuid.NewSHA1(uuid.NameSpaceOID, []byte("precompute-preview:"+projectID.String())))
	}
	snap, err := s.d.SnapshotSvc.CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		return nil, err
	}
	return s.d.Snapshots.Get(ctx, snap.ID)
}

func sameID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func recordToSnapshot(rec *persistence.ContextSnapshotRecord) (*statefile.ExecutionContextSnapshot, error) {
	roleSnap, err := decodeRoleSnapshot(rec.RoleSnapshotJSON)
	if err != nil {
		return nil, err
	}
	return &statefile.ExecutionContextSnapshot{
		ID: rec.ID, ProjectID: rec.ProjectID, PhaseID: rec.PhaseID, WorkUnitID: rec.WorkUnitID,
		RuntimeStateID: rec.RuntimeStateID, RuntimeStateVersion: rec.RuntimeStateVersion,
		StateMachineID: rec.StateMachineID, StateMachineVersion: rec.StateMachineVersion,
		StateKey: rec.StateKey, RoleSnapshot: roleSnap, StateFiles: toStateFileSnapshots(rec.Files),
	}, nil
}

// plan analyses one candidate: template, role, projected files, readiness,
// projection (validated) and identity. It reads only; nothing is written.
func (s *PrecomputeService) plan(ctx context.Context, d *discovery, projectID uuid.UUID, st *persistence.WorkflowStateRecord, phaseID, unitID *uuid.UUID, paths [][]string, depth, prio int, nextUnit, nextPhase bool) plannedCandidate {
	pc := plannedCandidate{state: st}
	pc.ProjectID, pc.StateKey, pc.Paths, pc.Depth, pc.Priority, pc.NextUnit = projectID, st.Key, paths, depth, prio, nextUnit
	pc.PhaseID, pc.WorkUnitID = phaseID, unitID
	unknown := func(reason string) plannedCandidate {
		pc.Readiness, pc.Structural, pc.reason = precompute.Unknown, append(pc.Structural, reason), reason
		c, _ := precompute.NewCandidate(pc.Candidate)
		pc.Candidate = c
		return pc
	}
	if execution.IsCommandTemplate(st.PromptTemplate) {
		return unknown("command state: the command is sent as is, nothing to precompute")
	}
	tmpl, terr := s.resolveStateTemplate(ctx, projectID, st.PromptTemplate)
	a := precompute.Analysis{HasTemplate: terr == nil, StaticFilesOK: true, UnitEligible: true}
	if terr == nil {
		pc.template, pc.TemplateKey, pc.TemplatePurpose = tmpl, tmpl.Key, tmpl.Purpose
		a.TemplateKey, a.Purpose = tmpl.Key, tmpl.Purpose
		a.UnitBound = s.registry.UnitBound[tmpl.Purpose]
		if tmpl.Purpose == "PHASE_PLANNING" {
			unitID, pc.WorkUnitID = nil, nil
		}
	}
	if pc.Priority != precompute.PriorityNextUnit && !a.UnitBound && terr == nil && pc.Priority != precompute.PriorityImmediate {
		pc.Priority = precompute.PriorityPhaseBoundry
	}
	a.HasUnit = unitID != nil
	ps, err := s.d.SnapshotSvc.AssembleProjected(ctx, projectID, d.runtime, st.ID, phaseID, unitID)
	if err != nil {
		a.HasRole = false
		r, _, _, structural, _ := precompute.Classify(a, s.registry)
		if r == precompute.Unknown && len(structural) > 0 {
			return unknown(strings.Join(structural, "; "))
		}
		return unknown("projected context not assemblable: " + err.Error())
	}
	pc.RoleKey = ps.RoleKey
	a.HasRole = ps.RoleEnabled
	a.ActiveTaskExists = true
	for _, m := range ps.MissingRequired {
		if m == "ACTIVE_TASK" && unitID != nil {
			a.ActiveTaskExists = false
			continue
		}
		a.StaticFilesOK = false
	}
	// The role may require ACTIVE_TASK even when its file type is not
	// flagged required (it is then silently omitted by scope resolution):
	// a projected unit without its ACTIVE_TASK must never look complete.
	if unitID != nil && a.ActiveTaskExists && containsKey(ps.Snapshot.RoleSnapshot.StateFiles, "ACTIVE_TASK") && !hasFile(ps.Snapshot.StateFiles, "ACTIVE_TASK") {
		a.ActiveTaskExists = false
	}
	readiness, known, missing, structural, placeholder := precompute.Classify(a, s.registry)
	pc.Readiness, pc.Known, pc.Missing, pc.Structural, pc.NeedsPlaceholder = readiness, known, missing, structural, placeholder
	if readiness == precompute.Unknown {
		return unknown(strings.Join(structural, "; "))
	}
	rec, err := SnapshotToRecord(ps.Snapshot, d.base.ID)
	if err != nil {
		return unknown("projection record: " + err.Error())
	}
	if !a.ActiveTaskExists {
		var ok bool
		if rec, ok = WithPlaceholderActiveTask(rec, true); !ok {
			return unknown("placeholder requires a work unit")
		}
	}
	pc.record = rec
	var pins []precompute.FilePin
	for _, f := range rec.Files {
		pins = append(pins, precompute.FilePin{TypeKey: f.TypeKey, Version: f.Version, Hash: f.Hash, Placeholder: compression.ContainsPlaceholder(f.Content)})
	}
	path := []string{}
	if len(paths) > 0 {
		path = paths[0]
	}
	vin := precompute.ValidationInput{
		Graph: d.graph, StartState: d.current.Key, Path: path, TargetState: st.Key,
		RoleAssigned: true, RoleEnabled: ps.RoleEnabled, BasePhaseID: d.runtime.PhaseID, ProjectedPhaseID: phaseID,
		UnitBound: a.UnitBound, ProjectedUnit: unitID, UnitEligible: true, UnitIsCurrent: sameID(unitID, d.runtime.WorkUnitID),
		StaticFilesOK: a.StaticFilesOK, NextUnitTarget: nextUnit, NextPhaseTarget: nextPhase,
	}
	if err := precompute.ValidateProjection(vin); err != nil {
		return unknown(err.Error())
	}
	proj, err := precompute.NewProjectedContext(precompute.ProjectedContext{
		BaseSnapshotID: d.base.ID, BaseSemanticHash: d.baseSemantic, ProjectID: projectID, PhaseID: phaseID,
		ProjectedWorkUnitID: unitID, ProjectedStateKey: st.Key, ProjectedRoleKey: ps.RoleKey, TransitionPath: path,
		Pins: pins, CreatedAt: s.now(),
	})
	if err != nil {
		return unknown(err.Error())
	}
	pc.projection = &proj
	pc.IdentityHash = proj.ProjectionHash
	c, _ := precompute.NewCandidate(pc.Candidate)
	pc.Candidate = c
	return pc
}

// ---------------------------------------------------------------------------
// Jobs (WBS 11.36-11.49).

// WarmUpcomingPrompts runs one precompute job synchronously (WBS 11.1).
func (s *PrecomputeService) WarmUpcomingPrompts(ctx context.Context, req precompute.Request) (precompute.Result, error) {
	job, st, err := s.newJob(ctx, req)
	if err != nil {
		return precompute.Result{}, err
	}
	return s.runJob(ctx, job, st, req), nil
}

// Trigger enqueues an asynchronous job and returns immediately (WBS 11.4):
// failures never propagate to the caller beyond refusal reasons.
func (s *PrecomputeService) Trigger(ctx context.Context, projectID uuid.UUID, trigger precompute.Trigger) (*precompute.Job, error) {
	rt, err := s.d.Runtime.Get(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	req := precompute.Request{ProjectID: projectID, RuntimeStateID: rt.ID, Trigger: trigger}
	job, st, err := s.newJob(ctx, req)
	if err != nil {
		return nil, err
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer containPanic(nil, "precompute job", "job_id", job.ID.String())
		s.runJob(s.baseCtx, job, st, req)
	}()
	return job, nil
}

func (s *PrecomputeService) newJob(ctx context.Context, req precompute.Request) (*precompute.Job, precompute.Settings, error) {
	if err := req.Validate(); err != nil {
		return nil, precompute.Settings{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	p, err := s.project(ctx, req.ProjectID)
	if err != nil {
		return nil, precompute.Settings{}, err
	}
	if !activeStatus(p.Status) {
		return nil, precompute.Settings{}, ErrProjectNotActive
	}
	st, _, err := s.d.Settings.Get(ctx, req.ProjectID)
	if err != nil {
		return nil, precompute.Settings{}, translate(err)
	}
	if !st.Enabled {
		return nil, st, ErrPrecomputeDisabled
	}
	if req.Trigger == precompute.TriggerWorkerDispatch && !st.DuringWorkerExecution {
		return nil, st, ErrPrecomputeDisabled
	}
	depth, limit := req.MaxDepth, req.MaxCandidates
	if depth == 0 {
		depth = st.Depth
	}
	if limit == 0 {
		limit = st.MaxCandidates
	}
	now := s.now()
	job := &precompute.Job{ID: uuid.New(), ProjectID: req.ProjectID, Trigger: req.Trigger, Status: precompute.JobPending,
		Owner: s.owner, Depth: depth, MaxCandidates: limit, CreatedAt: now, HeartbeatAt: now}
	if err := s.d.Jobs.CreateJob(ctx, job); err != nil {
		return nil, st, translate(err)
	}
	return job, st, nil
}

func (s *PrecomputeService) event(ctx context.Context, projectID uuid.UUID, typ string, key string, entry *uuid.UUID, c *precompute.Candidate, reason string) {
	ev := cache.NewEvent(projectID, cache.EventType(typ), "PRECOMPUTE", key, s.now())
	ev.EntryID, ev.Reason = entry, reason
	if c != nil {
		ev.StateKey, ev.WorkUnitID = c.StateKey, c.WorkUnitID
	}
	_ = s.d.Events.Append(context.WithoutCancel(ctx), ev)
}

// runJob executes a persisted job (never returns an error: everything is
// recorded on the job).
func (s *PrecomputeService) runJob(parent context.Context, job *precompute.Job, st precompute.Settings, req precompute.Request) precompute.Result {
	// Precompute is speculative: it yields shared capacity (compression)
	// to active execution and is cancelled first (WBS 18.94 / 18.95).
	parent = resilience.WithSpeculative(parent)
	ctx, cancel := context.WithCancel(parent)
	if st.MaxDuration > 0 {
		ctx, cancel = context.WithTimeout(parent, st.MaxDuration)
	}
	defer cancel()
	s.mu.Lock()
	s.running[job.ID] = runningJob{project: job.ProjectID, cancel: cancel}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.running, job.ID)
		s.mu.Unlock()
	}()
	started := s.now()
	if ok, err := s.d.Jobs.StartJob(ctx, job.ID, s.owner, started); err != nil || !ok {
		return s.finish(job, nil, precompute.JobFailed, "could not start job")
	}
	job.Status, job.StartedAt = precompute.JobRunning, &started
	s.event(ctx, job.ProjectID, "PRECOMPUTE_STARTED", "", &job.ID, nil, string(job.Trigger))

	d, err := s.discover(ctx, job.ProjectID, job.Depth, req.BaseSnapshotID, true)
	if err != nil {
		return s.finish(job, nil, precompute.JobFailed, "discovery failed: "+err.Error())
	}
	bid := d.base.ID
	job.BaseSnapshotID, job.StateKey, job.WorkUnitID = &bid, d.current.Key, d.runtime.WorkUnitID
	var cands []precompute.Candidate
	byID := map[uuid.UUID]plannedCandidate{}
	for _, pc := range d.candidates {
		cands = append(cands, pc.Candidate)
		byID[pc.ID] = pc
	}
	ranked := precompute.Rank(precompute.Dedup(cands), job.MaxCandidates)
	recs := make([]*precompute.CandidateRecord, len(ranked))
	for i, c := range ranked {
		pc := byID[c.ID]
		pc.Candidate = c
		byID[c.ID] = pc
		rec := &precompute.CandidateRecord{ID: c.ID, JobID: job.ID, ProjectID: job.ProjectID, StateKey: c.StateKey, RoleKey: c.RoleKey,
			PhaseID: c.PhaseID, WorkUnitID: c.WorkUnitID, Paths: c.Paths, Depth: c.Depth, NextUnit: c.NextUnit, Readiness: c.Readiness,
			Priority: c.Priority, Missing: c.Missing, Reason: pc.reason, Result: precompute.ResultPending, CreatedAt: s.now()}
		recs[i] = rec
		_ = s.d.Jobs.UpsertCandidate(ctx, rec)
		s.event(ctx, job.ProjectID, "PRECOMPUTE_CANDIDATE_DISCOVERED", "", &rec.ID, &c, string(c.Readiness))
	}
	budget := precompute.NewBudget(job.MaxCandidates, st.MaxTokens, 0, s.now)
	sem := make(chan struct{}, st.Concurrency)
	var wg sync.WaitGroup
	cancelled := false
	for i := range ranked {
		if s.beforeCandidate != nil {
			s.beforeCandidate(ctx, recs[i])
		}
		if ctx.Err() != nil || !s.projectActive(ctx, job.ProjectID) {
			cancelled = true
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			cancelled = true
		}
		if cancelled {
			break
		}
		wg.Add(1)
		go func(pc plannedCandidate, rec *precompute.CandidateRecord) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					rec.Result, rec.Reason = precompute.ResultFailed, fmt.Sprintf("candidate panicked: %v", r)
				}
				_ = s.d.Jobs.UpsertCandidate(context.WithoutCancel(ctx), rec)
				_ = s.d.Jobs.Heartbeat(context.WithoutCancel(ctx), job.ID, s.now())
			}()
			s.process(ctx, job, st, budget, pc, rec)
		}(byID[ranked[i].ID], recs[i])
	}
	wg.Wait()
	if ctx.Err() != nil {
		cancelled = true
	}
	final := make([]precompute.CandidateRecord, 0, len(recs))
	for _, r := range recs {
		if r.Result == precompute.ResultPending {
			r.Result, r.Reason = precompute.ResultSkipped, "job cancelled before the candidate started"
			if ctx.Err() == context.DeadlineExceeded {
				r.Reason = precompute.BudgetDuration
			}
			_ = s.d.Jobs.UpsertCandidate(context.WithoutCancel(ctx), r)
		}
		final = append(final, *r)
	}
	sum := precompute.Summarize(job.ID, final)
	status, reason := sum.Status, ""
	if cancelled && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		status, reason = precompute.JobCancelled, "cancelled (project paused/cancelled or service stopping)"
	} else if cancelled {
		reason = precompute.BudgetDuration
	}
	return s.finish(job, final, status, reason)
}

func (s *PrecomputeService) projectActive(ctx context.Context, projectID uuid.UUID) bool {
	p, err := s.project(ctx, projectID)
	return err == nil && activeStatus(p.Status)
}

func (s *PrecomputeService) finish(job *precompute.Job, recs []precompute.CandidateRecord, status precompute.JobStatus, reason string) precompute.Result {
	sum := precompute.Summarize(job.ID, recs)
	sum.Status = status
	at := s.now()
	job.Status, job.Reason, job.CompletedAt = status, reason, &at
	job.CandidateCount, job.HitCount, job.BuiltCount, job.FragmentCount = sum.Candidates, sum.ExistingHits, sum.FullPrompts, sum.Fragments
	job.TemplateCount, job.SkippedCount, job.FailedCount = sum.Templates, sum.Skipped, sum.Failed
	_, _ = s.d.Jobs.FinishJob(context.Background(), job)
	typ := "PRECOMPUTE_COMPLETED"
	if status == precompute.JobCancelled {
		typ = "PRECOMPUTE_CANCELLED"
	}
	s.event(context.Background(), job.ProjectID, typ, "", &job.ID, nil, fmt.Sprintf("%s candidates=%d hits=%d built=%d fragments=%d templates=%d skipped=%d failed=%d",
		status, sum.Candidates, sum.ExistingHits, sum.FullPrompts, sum.Fragments, sum.Templates, sum.Skipped, sum.Failed))
	return sum
}

// process prepares one candidate (cache-first; isolated failures).
func (s *PrecomputeService) process(ctx context.Context, job *precompute.Job, st precompute.Settings, budget *precompute.Budget, pc plannedCandidate, rec *precompute.CandidateRecord) {
	c := pc.Candidate
	skip := func(reason string) {
		rec.Result, rec.Reason = precompute.ResultSkipped, reason
		s.event(ctx, job.ProjectID, "PRECOMPUTE_SKIPPED", "", &rec.ID, &c, reason)
	}
	fail := func(err error) {
		rec.Result, rec.Reason = precompute.ResultFailed, err.Error()
		s.event(ctx, job.ProjectID, "PRECOMPUTE_FAILED", "", &rec.ID, &c, truncateReason(err.Error()))
	}
	if c.Readiness == precompute.Unknown {
		skip("UNKNOWN: " + pc.reason)
		return
	}
	compressionWork := st.CompressionEnabled && (c.Readiness == precompute.FullyKnown || c.NeedsPlaceholder)
	cst, _, _ := s.d.Compression.EffectiveSettings(ctx, job.ProjectID)
	if dec, why := precompute.Evaluate(st.ResourcePolicy, compressionWork, s.d.ProviderLocal(cst.Provider), s.tracker.Active(job.ProjectID.String()), st.WorkerIsLocal); dec == precompute.DecisionSkip {
		skip(why)
		return
	}
	if err := s.d.Projections.Create(ctx, pc.projection); err != nil {
		fail(err)
		return
	}
	pid := pc.projection.ID
	rec.ProjectionID = &pid
	in, _, err := s.d.Composer.ResolveCompositionFromRecord(ctx, pc.record, prompt.ComposePromptRequest{
		ContextSnapshotID: pc.record.ID, TemplateID: pc.template.ID, TemplateVersion: pc.template.Version,
	})
	if err != nil {
		fail(fmt.Errorf("resolve projected composition: %w", err))
		return
	}
	switch {
	case c.Readiness == precompute.FullyKnown:
		s.processFull(ctx, job, st, budget, pc, rec, in)
	case c.NeedsPlaceholder && st.CompressionEnabled:
		s.processTemplate(ctx, job, budget, pc, rec, in)
	default:
		s.processFragments(ctx, job, st, pc, rec, in)
	}
}

func truncateReason(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

func (s *PrecomputeService) processFull(ctx context.Context, job *precompute.Job, st precompute.Settings, budget *precompute.Budget, pc plannedCandidate, rec *precompute.CandidateRecord, in prompt.CanonicalCompositionInput) {
	c := pc.Candidate
	key, ckey, err := s.d.Cache.KeysForResolved(ctx, job.ProjectID, in, pc.record)
	if err != nil {
		rec.Result, rec.Reason = precompute.ResultFailed, err.Error()
		return
	}
	rec.CacheKey = key
	if !st.CompressionEnabled {
		if _, err := s.d.Cache.canonical.FindByCacheKey(ctx, job.ProjectID, ckey); err == nil {
			rec.Result = precompute.ResultHit
			s.event(ctx, job.ProjectID, "PRECOMPUTE_CACHE_HIT", ckey, &rec.ID, &c, "canonical")
			return
		}
		if _, err := s.d.Composer.ComposeResolvedForProjection(ctx, in, ckey, pc.projection.ID); err != nil {
			rec.Result, rec.Reason = precompute.ResultFailed, err.Error()
			return
		}
		rec.Result = precompute.ResultCanonical
		s.event(ctx, job.ProjectID, "PRECOMPUTE_PROMPT_CREATED", ckey, &rec.ID, &c, "canonical only (compression disabled)")
		return
	}
	cp, _, _, cerr := prompt.ComposeCanonicalPromptDetailed(in)
	tokens := 0
	if cerr == nil {
		tokens = cp.EstimatedTokens
	}
	if e, err := s.d.CacheEntries.GetValid(ctx, job.ProjectID, key, cache.LayerWorker); err == nil {
		id := e.ID
		rec.Result, rec.CacheEntryID = precompute.ResultHit, &id
		s.event(ctx, job.ProjectID, "PRECOMPUTE_CACHE_HIT", key, &id, &c, "")
		return
	}
	if why := budget.Take(tokens); why != "" {
		rec.Result, rec.Reason = precompute.ResultSkipped, why
		s.event(ctx, job.ProjectID, "PRECOMPUTE_SKIPPED", key, &rec.ID, &c, why)
		return
	}
	res, err := s.d.Cache.PrepareForProjection(ctx, job.ProjectID, in, pc.record, pc.projection.ID)
	if errors.Is(err, ErrCompressionDeferred) {
		rec.Result, rec.Reason = precompute.ResultSkipped, truncateReason(err.Error())
		s.event(ctx, job.ProjectID, "PRECOMPUTE_SKIPPED", key, &rec.ID, &c, rec.Reason)
		return
	}
	if err != nil {
		rec.Result, rec.Reason = precompute.ResultFailed, truncateReason(err.Error())
		s.event(ctx, job.ProjectID, "PRECOMPUTE_FAILED", key, &rec.ID, &c, truncateReason(err.Error()))
		return
	}
	if res.Entry != nil {
		id := res.Entry.ID
		rec.CacheEntryID = &id
	}
	if res.Hit {
		rec.Result = precompute.ResultHit
		s.event(ctx, job.ProjectID, "PRECOMPUTE_CACHE_HIT", key, rec.CacheEntryID, &c, "")
		return
	}
	rec.Result = precompute.ResultBuilt
	s.event(ctx, job.ProjectID, "PRECOMPUTE_PROMPT_CREATED", key, rec.CacheEntryID, &c, "")
}

func (s *PrecomputeService) processTemplate(ctx context.Context, job *precompute.Job, budget *precompute.Budget, pc plannedCandidate, rec *precompute.CandidateRecord, in prompt.CanonicalCompositionInput) {
	c := pc.Candidate
	nkey, ckey, err := s.d.Cache.KeysForResolved(ctx, job.ProjectID, in, pc.record)
	if err != nil {
		rec.Result, rec.Reason = precompute.ResultFailed, err.Error()
		return
	}
	rec.CacheKey = nkey
	if t, err := s.d.Templates.GetPending(ctx, job.ProjectID, nkey); err == nil {
		id := t.ID
		rec.Result, rec.TemplateID = precompute.ResultHit, &id
		s.event(ctx, job.ProjectID, "PRECOMPUTE_CACHE_HIT", nkey, &id, &c, "placeholder template")
		return
	}
	cpd, _, _, cerr := prompt.ComposeCanonicalPromptDetailed(in)
	if cerr == nil {
		if why := budget.Take(cpd.EstimatedTokens); why != "" {
			rec.Result, rec.Reason = precompute.ResultSkipped, why
			return
		}
	}
	canonical, err := s.d.Cache.canonical.FindByCacheKey(ctx, job.ProjectID, ckey)
	if errors.Is(err, persistence.ErrNotFound) {
		canonical, err = s.d.Composer.ComposeResolvedForProjection(ctx, in, ckey, pc.projection.ID)
	}
	if err != nil {
		rec.Result, rec.Reason = precompute.ResultFailed, truncateReason("compose placeholder canonical: "+err.Error())
		return
	}
	tc, err := s.d.Compression.CompressTemplate(ctx, job.ProjectID, canonical.ID)
	if errors.Is(err, ErrCompressionDeferred) {
		rec.Result, rec.Reason = precompute.ResultSkipped, truncateReason(err.Error())
		s.event(ctx, job.ProjectID, "PRECOMPUTE_SKIPPED", nkey, &rec.ID, &c, rec.Reason)
		return
	}
	if err != nil {
		rec.Result, rec.Reason = precompute.ResultFailed, truncateReason("compress template: "+err.Error())
		s.event(ctx, job.ProjectID, "PRECOMPUTE_FAILED", nkey, &rec.ID, &c, truncateReason(err.Error()))
		return
	}
	t := &persistence.PrecomputedTemplate{
		ID: uuid.New(), ProjectID: job.ProjectID, ProjectionID: pc.projection.ID, WorkUnitID: *c.WorkUnitID, StateKey: c.StateKey,
		NormalizedKey: nkey, PlaceholderToken: precompute.PlaceholderToken(*c.WorkUnitID), CanonicalPromptID: canonical.ID,
		ContentSource: tc.ContentSource, Status: persistence.TemplatePending, CreatedAt: s.now(),
	}
	if tc.Compressed != nil {
		id := tc.Compressed.ID
		t.CompressedPromptID = &id
	}
	stored, created, err := s.d.Templates.Create(ctx, t)
	if err != nil {
		rec.Result, rec.Reason = precompute.ResultFailed, "store template: "+err.Error()
		return
	}
	id := stored.ID
	rec.TemplateID = &id
	if !created {
		rec.Result = precompute.ResultHit
		return
	}
	rec.Result = precompute.ResultTemplate
	s.event(ctx, job.ProjectID, "PRECOMPUTE_PROMPT_CREATED", nkey, &id, &c, "placeholder template (ACTIVE_TASK pending)")
}

func (s *PrecomputeService) processFragments(ctx context.Context, job *precompute.Job, st precompute.Settings, pc plannedCandidate, rec *precompute.CandidateRecord, in prompt.CanonicalCompositionInput) {
	c := pc.Candidate
	if !st.StaticFragments {
		rec.Result, rec.Reason = precompute.ResultSkipped, "static fragments disabled"
		return
	}
	// Static material only: the projection carries no worker status, test
	// output, failures or generated plan — nothing is fabricated (WBS
	// 11.25); same-session continuation material is never compressed.
	_, _, comps, err := prompt.ComposeCanonicalPromptDetailed(in)
	if err != nil {
		rec.Result, rec.Reason = precompute.ResultFailed, err.Error()
		return
	}
	frags := cache.BuildFragments(job.ProjectID, comps, s.now())
	created := 0
	for _, f := range frags {
		f := f
		stored, isNew, err := s.d.Fragments.StoreIfAbsent(ctx, &f)
		if err != nil {
			rec.Result, rec.Reason = precompute.ResultFailed, err.Error()
			return
		}
		rec.FragmentIDs = append(rec.FragmentIDs, stored.ID)
		m, err := precompute.NewFragmentManifest(precompute.FragmentManifest{
			ProjectID: job.ProjectID, FragmentID: stored.ID, FragmentKey: stored.FragmentKey,
			RequiredDynamicInputs: c.Missing, SourceSnapshotHash: pc.projection.BaseSemanticHash,
			ProjectionHash: pc.projection.ProjectionHash, CreatedAt: s.now(),
		})
		if err == nil {
			_, _, _ = s.d.Manifests.CreateIfAbsent(ctx, &m)
		}
		if isNew {
			created++
		}
	}
	if len(rec.FragmentIDs) == 0 {
		rec.Result, rec.Reason = precompute.ResultSkipped, "no static fragment material"
		return
	}
	rec.Result = precompute.ResultFragment
	if created == 0 {
		rec.Reason = "fragments already cached"
	}
	s.event(ctx, job.ProjectID, "PRECOMPUTE_FRAGMENT_CREATED", "", &rec.ID, &c, fmt.Sprintf("%d fragment(s), %d new; awaiting %v", len(rec.FragmentIDs), created, c.Missing))
}

// ---------------------------------------------------------------------------
// Hooks (WBS 11.4, 11.41, 11.42, 11.58-11.61).

// NotifyWorkerDispatched is called right after a worker request is
// dispatched (Phase 12): the worker is marked active and a warm-up job is
// enqueued without blocking the dispatch. Errors are swallowed (recorded on
// the job when one exists).
func (s *PrecomputeService) NotifyWorkerDispatched(ctx context.Context, projectID uuid.UUID) *precompute.Job {
	s.tracker.SetActive(projectID.String(), true)
	job, _ := s.Trigger(ctx, projectID, precompute.TriggerWorkerDispatch)
	return job
}

// NotifyWorkerFinished marks the worker idle.
func (s *PrecomputeService) NotifyWorkerFinished(projectID uuid.UUID) {
	s.tracker.SetActive(projectID.String(), false)
}

// OnTrigger enqueues a job for a lifecycle trigger (plan accepted, work
// unit started, state transition, phase start); errors are swallowed.
func (s *PrecomputeService) OnTrigger(ctx context.Context, projectID uuid.UUID, trigger precompute.Trigger) *precompute.Job {
	job, _ := s.Trigger(ctx, projectID, trigger)
	return job
}

// OnProjectStatus reacts to committed project status changes: PAUSED /
// CANCELLED / finished cancel running jobs (no new work starts; completed
// artifacts stay).
func (s *PrecomputeService) OnProjectStatus(ctx context.Context, projectID uuid.UUID, status project.ProjectStatus) {
	if !activeStatus(status) {
		s.CancelProject(projectID)
	}
}

// CancelProject cancels the project's running jobs in this instance.
func (s *PrecomputeService) CancelProject(projectID uuid.UUID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.running {
		if r.project == projectID {
			r.cancel()
			n++
		}
	}
	return n
}

// Wait blocks until every asynchronous job of this instance finished.
func (s *PrecomputeService) Wait() { s.wg.Wait() }

// Recover reclassifies stale non-terminal jobs (dead owner / restart, WBS
// 11.73): pending candidates are marked FAILED (interrupted) and the job
// PARTIAL (some work done) or FAILED. Completed artifacts are untouched;
// stale cache build leases are reclaimed by the Phase 10 lease logic.
func (s *PrecomputeService) Recover(ctx context.Context) (int, error) {
	stale, err := s.d.Jobs.ListStale(ctx, s.now().Add(-s.staleAfter), s.owner)
	if err != nil {
		return 0, translate(err)
	}
	for i := range stale {
		j := stale[i]
		_, _ = s.d.Jobs.FailPendingCandidates(ctx, j.ID, "interrupted (instance stopped)", s.now())
		recs, _ := s.d.Jobs.ListCandidates(ctx, j.ID)
		sum := precompute.Summarize(j.ID, recs)
		status := precompute.JobPartial
		if sum.ExistingHits+sum.FullPrompts+sum.Fragments+sum.Templates == 0 {
			status = precompute.JobFailed
		}
		s.finish(&j, recs, status, "recovered after interruption")
	}
	return len(stale), nil
}

// ---------------------------------------------------------------------------
// Actual preparation entry point (WBS 11.62-11.66).

// NextRequest prepares the worker prompt for the CURRENT authoritative
// position after a real transition.
type NextRequest struct {
	ResultReceivedAt *time.Time
	Continuation     *prompt.ContinuationStatusInput
	ContinuationMode string
}

// PrepareNext prepares the worker prompt of the project's current
// (authoritative) position: it ensures the current unit's real ACTIVE_TASK
// value is calculated (Phase 7 derivation when missing or stale — Decision
// 5), creates the authoritative snapshot, resolves the state's template and
// runs the normal Phase 10 preparation, which serves a matching
// precomputed entry or resolves a pending placeholder template — never a
// projection directly (WBS 11.23).
func (s *PrecomputeService) PrepareNext(ctx context.Context, projectID uuid.UUID, req NextRequest) (*PrepareResult, error) {
	rt, err := s.d.Runtime.Get(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	st, err := s.d.States.Get(ctx, rt.CurrentStateID)
	if err != nil {
		return nil, translate(err)
	}
	tmpl, err := s.resolveStateTemplate(ctx, projectID, st.PromptTemplate)
	if err != nil {
		return nil, fmt.Errorf("%w: state %s has no resolvable prompt template", ErrInvalidInput, st.Key)
	}
	if rt.WorkUnitID != nil && s.registry.UnitBound[tmpl.Purpose] {
		if err := s.ensureActiveTask(ctx, projectID, rt); err != nil {
			return nil, err
		}
	}
	snap, err := s.d.SnapshotSvc.CreateExecutionSnapshot(ctx, projectID.String())
	if err != nil {
		return nil, err
	}
	return s.d.Cache.PrepareForContext(ctx, projectID, PrepareRequest{
		SnapshotID: snap.ID, TemplateID: tmpl.ID, TemplateVersion: tmpl.Version,
		Continuation: req.Continuation, ContinuationMode: req.ContinuationMode, ResultReceivedAt: req.ResultReceivedAt,
	})
}

// ensureActiveTask derives the current unit's ACTIVE_TASK when it is
// missing or differs from the Phase 7 rendering of the unit.
func (s *PrecomputeService) ensureActiveTask(ctx context.Context, projectID uuid.UUID, rt *persistence.RuntimeStateRecord) error {
	unit, err := s.d.Units.Get(ctx, *rt.WorkUnitID)
	if err != nil {
		return translate(err)
	}
	want := statefile.RenderActiveTask(unit)
	resolved, _, err := s.d.ScopeRes.ResolveStateFileScopesAt(ctx, projectID.String(), rt.PhaseID, rt.WorkUnitID)
	if err != nil {
		return err
	}
	for _, r := range resolved {
		if r.Type.Key != "ACTIVE_TASK" || r.File.CurrentVersion == 0 {
			continue
		}
		v, err := s.d.FileVersion.GetVersion(ctx, r.File.ID, r.File.CurrentVersion)
		if err == nil && v.Content == want {
			return nil
		}
	}
	if _, err := s.d.ActiveTask.DeriveActiveTask(ctx, projectID.String()); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Read surfaces (WBS 11.51-11.57, 11.77).

// CandidateView is a non-executing candidate preview row.
type CandidateView struct {
	StateKey   string
	RoleKey    string
	WorkUnitID *uuid.UUID
	PhaseID    *uuid.UUID
	Readiness  precompute.Readiness
	Priority   int
	Depth      int
	NextUnit   bool
	Paths      [][]string
	Missing    []precompute.DynamicInputType
	Reason     string
	Template   string
}

// PreviewCandidates discovers and classifies candidates without executing
// anything — no composition, compression, cache write or projection row
// (T11.56.1-T11.56.4). It reads the latest snapshot of the current
// position (never creates one).
func (s *PrecomputeService) PreviewCandidates(ctx context.Context, projectID uuid.UUID) ([]CandidateView, error) {
	if _, err := s.project(ctx, projectID); err != nil {
		return nil, err
	}
	st, _, err := s.d.Settings.Get(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	d, err := s.discover(ctx, projectID, st.Depth, nil, false)
	if err != nil {
		return nil, err
	}
	var cands []precompute.Candidate
	byID := map[uuid.UUID]plannedCandidate{}
	for _, pc := range d.candidates {
		cands = append(cands, pc.Candidate)
		byID[pc.ID] = pc
	}
	var out []CandidateView
	for _, c := range precompute.Rank(precompute.Dedup(cands), st.MaxCandidates) {
		pc := byID[c.ID]
		out = append(out, CandidateView{StateKey: c.StateKey, RoleKey: c.RoleKey, WorkUnitID: c.WorkUnitID, PhaseID: c.PhaseID,
			Readiness: c.Readiness, Priority: c.Priority, Depth: c.Depth, NextUnit: c.NextUnit, Paths: c.Paths,
			Missing: c.Missing, Reason: pc.reason, Template: c.TemplateKey})
	}
	return out, nil
}

// ListJobs pages the project's jobs.
func (s *PrecomputeService) ListJobs(ctx context.Context, projectID uuid.UUID, limit, offset int) ([]precompute.Job, int, error) {
	if _, err := s.project(ctx, projectID); err != nil {
		return nil, 0, err
	}
	jobs, total, err := s.d.Jobs.ListJobs(ctx, persistence.JobFilter{ProjectID: projectID, Limit: limit, Offset: offset})
	if err != nil {
		return nil, 0, translate(err)
	}
	return jobs, total, nil
}

// GetJob returns a job with its candidate records (project-scoped).
func (s *PrecomputeService) GetJob(ctx context.Context, projectID, jobID uuid.UUID) (*precompute.Job, []precompute.CandidateRecord, error) {
	j, err := s.d.Jobs.GetJob(ctx, jobID)
	if err != nil {
		return nil, nil, translate(err)
	}
	if j.ProjectID != projectID {
		return nil, nil, ErrNotFound
	}
	recs, err := s.d.Jobs.ListCandidates(ctx, jobID)
	if err != nil {
		return nil, nil, translate(err)
	}
	return j, recs, nil
}

// Metrics computes precompute effectiveness: utilization by depth and
// priority (used = the produced artifact was later used by an actual
// preparation) and measured transition latency by source.
func (s *PrecomputeService) Metrics(ctx context.Context, projectID uuid.UUID) (precompute.Metrics, error) {
	if _, err := s.project(ctx, projectID); err != nil {
		return precompute.Metrics{}, err
	}
	_, jobs, err := s.d.Jobs.ListJobs(ctx, persistence.JobFilter{ProjectID: projectID, Limit: 1})
	if err != nil {
		return precompute.Metrics{}, translate(err)
	}
	cands, err := s.d.Jobs.ListProjectCandidates(ctx, projectID)
	if err != nil {
		return precompute.Metrics{}, translate(err)
	}
	events, err := s.d.Events.ListByProject(ctx, projectID, 0)
	if err != nil {
		return precompute.Metrics{}, translate(err)
	}
	fragUsed := map[uuid.UUID]bool{}
	for _, e := range events {
		if e.Type == cache.EventHit && e.Layer == cache.LayerFragment && e.EntryID != nil {
			fragUsed[*e.EntryID] = true
		}
	}
	var usage []precompute.UsageRecord
	for _, c := range cands {
		u := precompute.UsageRecord{Depth: c.Depth, Priority: c.Priority, Result: c.Result}
		switch c.Result {
		case precompute.ResultBuilt:
			if c.CacheEntryID != nil {
				if e, err := s.d.CacheEntries.Get(ctx, *c.CacheEntryID); err == nil {
					u.Used = e.HitCount > 0
				}
			}
		case precompute.ResultTemplate:
			if c.TemplateID != nil {
				if t, err := s.d.Templates.Get(ctx, *c.TemplateID); err == nil {
					u.Used = t.Status == persistence.TemplateResolved
				}
			}
		case precompute.ResultFragment:
			for _, f := range c.FragmentIDs {
				u.Used = u.Used || fragUsed[f]
			}
		}
		usage = append(usage, u)
	}
	lats, err := s.d.Latencies.ListByProject(ctx, projectID)
	if err != nil {
		return precompute.Metrics{}, translate(err)
	}
	var samples []precompute.LatencySample
	for _, l := range lats {
		samples = append(samples, precompute.LatencySample{Source: l.Source, Duration: l.Duration})
	}
	return precompute.Aggregate(jobs, usage, samples), nil
}

// Summary is the Phase 17 read model (WBS 11.57): the latest job, its
// counts, and the current candidates with their readiness and the dynamic
// inputs they await — no probability language.
type Summary struct {
	Latest     *precompute.Job
	Candidates []CandidateView
}

// ReadModel returns the summary.
func (s *PrecomputeService) ReadModel(ctx context.Context, projectID uuid.UUID) (*Summary, error) {
	jobs, _, err := s.ListJobs(ctx, projectID, 1, 0)
	if err != nil {
		return nil, err
	}
	out := &Summary{}
	if len(jobs) > 0 {
		out.Latest = &jobs[0]
	}
	if cands, err := s.PreviewCandidates(ctx, projectID); err == nil {
		out.Candidates = cands
	}
	return out, nil
}

// RuntimeID returns the project's runtime-state id (manual job requests).
func (s *PrecomputeService) RuntimeID(ctx context.Context, projectID uuid.UUID) (uuid.UUID, error) {
	rt, err := s.d.Runtime.Get(ctx, projectID)
	if err != nil {
		return uuid.Nil, translate(err)
	}
	return rt.ID, nil
}

func containsKey(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

func hasFile(files []statefile.StateFileSnapshot, typeKey string) bool {
	for _, f := range files {
		if f.TypeKey == typeKey && f.Version > 0 {
			return true
		}
	}
	return false
}
