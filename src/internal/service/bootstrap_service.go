// Phase 15 (WBS 15.1–15.168): project initialization & execution bootstrap.
//
// BootstrapService converts a READY project into a durable, activated
// execution: readiness → plan → configuration snapshot (with the execution
// row, one transaction) → state files → runtime → context → initial prompt
// → checkpoint → activation. It never calls the worker transport — it has
// no WorkerService dependency (WBS 15.77/15.78) — never creates a
// continuation, never marks work DONE and never writes phases / plans /
// WBS.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"overseer/internal/hardening"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/compression"
	"overseer/internal/continuation"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/phase"
	"overseer/internal/precompute"
	"overseer/internal/project"
	"overseer/internal/prompt"
	"overseer/internal/role"
	"overseer/internal/state"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// WorkerProviderInfo is the non-secret configuration of a worker provider
// (WBS 15.37): AuthSecretEnv is a reference, never a value.
type WorkerProviderInfo struct {
	Kind             string
	Endpoint         string
	AuthSecretEnv    string
	ConnectTimeout   time.Duration
	SessionTimeout   time.Duration
	MaxResponseBytes int64
	AllowedRoots     []string
	Capabilities     []string
}

// CompressionProviderInfo is the non-secret configuration of a compression
// provider (WBS 15.36).
type CompressionProviderInfo struct {
	Kind      string
	BaseURL   string
	APIKeyEnv string
}

// bootstrapPlans lists a phase's plans.
type bootstrapPlans interface {
	ListByPhase(ctx context.Context, phaseID uuid.UUID) ([]*wbs.ExecutionPlan, error)
}

// bootstrapUnits lists a plan's units and its dependency-ready units.
type bootstrapUnits interface {
	ListByPlan(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error)
	ListEligible(ctx context.Context, planID uuid.UUID) ([]*wbs.WorkUnit, error)
	Get(ctx context.Context, id uuid.UUID) (*wbs.WorkUnit, error)
}

// BootstrapResults is the Phase 13 surface bootstrap reads (read-only).
type BootstrapResults interface {
	ProtocolSettings(ctx context.Context, projectID uuid.UUID) (workerresult.Settings, bool, error)
	Readiness(ctx context.Context, projectID uuid.UUID) error
}

// BootstrapContinuations is the Phase 14 surface bootstrap reads.
type BootstrapContinuations interface {
	ContinuationSettings(ctx context.Context, projectID uuid.UUID) (continuation.Settings, bool, error)
}

// BootstrapWorkerSettings is the Phase 12 settings surface (read-only; no
// transport method is reachable from bootstrap).
type BootstrapWorkerSettings interface {
	WorkerSettings(ctx context.Context, projectID uuid.UUID) (worker.Settings, bool, error)
}

// BootstrapPrecompute schedules speculative preparation (WBS 15.75).
type BootstrapPrecompute interface {
	OnTrigger(ctx context.Context, projectID uuid.UUID, trigger precompute.Trigger) *precompute.Job
}

// BootstrapDeps wires the service.
type BootstrapDeps struct {
	// Build is recorded on every execution (WBS 18.206).
	Build BuildInfo

	Pool *pgxpool.Pool
	Repo persistence.ExecutionRepository

	Projects        persistence.ProjectRepository
	ProjectSettings persistence.SettingsRepository
	Phases          persistence.PhaseRepository
	WBSVersions     persistence.WBSVersionRepository
	Plans           bootstrapPlans
	Units           bootstrapUnits

	Runtime       persistence.RuntimeStateRepository
	Machines      persistence.StateMachineRepository
	States        persistence.WorkflowStateRepository
	MachineSnaps  *StateMachineSnapshotService
	Validation    *StateMachineValidationService
	Assignments   persistence.StateRoleAssignmentRepository
	Roles         persistence.RoleRepository
	RoleSvc       *RoleService
	Templates     persistence.PromptTemplateRepository
	TemplateVers  persistence.PromptTemplateVersionRepository
	Protocols     persistence.ResponseProtocolRepository
	ProtocolFind  persistence.ResponseProtocolLookup
	ScopeRes      *StateFileScopeResolutionService
	StateFiles    *StateFileService
	ActiveTask    *ActiveTaskService
	Materialize   *StateFileMaterializeService
	ContextSnaps  *ExecutionSnapshotService
	SnapshotsRepo persistence.ContextSnapshotRepository

	Cache        *PromptCacheService
	Compression  *CompressionService
	Precompute   BootstrapPrecompute
	Worker       BootstrapWorkerSettings
	Results      BootstrapResults
	Continuation BootstrapContinuations
	Lifecycle    *LifecycleService
	Inspector    execution.RepositoryInspector
	// Identifier / Identities record the execution's repository identity
	// at bootstrap (WBS 18.49); nil skips it (unit scope).
	Identifier execution.RepositoryIdentifier
	Identities persistence.RepositoryIdentityStore
	// Engine supplies the Phase 16 engine / hardening settings captured
	// in the configuration snapshot (P16-77); nil = defaults.
	Engine persistence.EngineRepository

	// WorkerProvider / CompressionProvider resolve configured providers
	// by name (nil = only "configured" is checked).
	WorkerProvider      func(name string) (WorkerProviderInfo, bool)
	CompressionProvider func(name string) (CompressionProviderInfo, bool)
	// WorkerSessionProbe reports why the project's worker cannot open a
	// session (gateway unreachable, unauthenticated, missing capability);
	// nil error = ready. CompressionSessionProbe reports why the
	// compression provider cannot serve the model. nil = not probed.
	WorkerSessionProbe      func(ctx context.Context, projectID uuid.UUID) error
	CompressionSessionProbe func(ctx context.Context, provider, model string) error
	// ProviderTestPassed reports whether the latest console connection
	// test of kind ("worker" / "compression") against provider and model
	// succeeded; such a session check passes without probing.
	ProviderTestPassed func(kind, provider, model string) bool

	Owner  string
	Logger *slog.Logger
	Now    func() time.Time
}

// BootstrapService is the WBS 15.6 bootstrap service.
type BootstrapService struct {
	d     BootstrapDeps
	files derivedFileWriter
	// stageHook is a test seam run before each stage (integration tests
	// inject concurrent edits / crashes); returning an error aborts.
	stageHook func(ctx context.Context, rec *persistence.ExecutionRecord, stage execution.BootstrapStage) error
	// createHook runs between readiness and the snapshot commit (test
	// seam for concurrent configuration edits, WBS 15.90/15.128).
	createHook func(ctx context.Context, attempt int)
	// onActivated runs after an execution moves READY → RUNNING (the app
	// kicks the engine scheduler so the first step need not wait a tick).
	onActivated func()
	// locks serializes bootstrap requests per project inside this
	// instance (the claim serializes across instances).
	locks sync.Map
}

// projectLock returns the per-project in-process bootstrap mutex.
func (s *BootstrapService) projectLock(projectID uuid.UUID) *sync.Mutex {
	m, _ := s.locks.LoadOrStore(projectID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// BuildInfo identifies the running application for reproducibility
// metadata (WBS 18.206).
type BuildInfo struct {
	AppVersion    string
	SchemaVersion int
	ConfigHash    string
}

// NewBootstrapService wires the service.
func NewBootstrapService(d BootstrapDeps) *BootstrapService {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Owner == "" {
		d.Owner = "overseer-" + uuid.NewString()[:8]
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &BootstrapService{d: d, files: derivedFileWriter{crud: d.StateFiles}}
}

// SetStageHook installs the test seam.
func (s *BootstrapService) SetStageHook(h func(ctx context.Context, rec *persistence.ExecutionRecord, stage execution.BootstrapStage) error) {
	s.stageHook = h
}

// SetCreateHook installs the pre-commit test seam.
func (s *BootstrapService) SetCreateHook(h func(ctx context.Context, attempt int)) { s.createHook = h }

// SetActivationHook installs the callback run after each activation.
func (s *BootstrapService) SetActivationHook(h func()) { s.onActivated = h }

// SetEngineSettings supplies the Phase 16 engine-settings source captured
// in configuration snapshots (P16-77).
func (s *BootstrapService) SetEngineSettings(r persistence.EngineRepository) { s.d.Engine = r }

// Owner is this instance's claim owner.
func (s *BootstrapService) Owner() string { return s.d.Owner }

// ---------------------------------------------------------------------------
// Settings (P15-39).

// ExecutionSettings returns the project's execution settings (defaults when
// none stored).
func (s *BootstrapService) ExecutionSettings(ctx context.Context, projectID uuid.UUID) (execution.Settings, bool, error) {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return execution.Settings{}, false, translate(err)
	}
	st, ok, err := s.d.Repo.GetSettings(ctx, projectID)
	return st, ok, translateNil(err)
}

// UpdateExecutionSettings validates and stores settings.
func (s *BootstrapService) UpdateExecutionSettings(ctx context.Context, projectID uuid.UUID, st execution.Settings) (execution.Settings, error) {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return execution.Settings{}, translate(err)
	}
	if err := st.Validate(); err != nil {
		return execution.Settings{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.d.Repo.UpsertSettings(ctx, projectID, st); err != nil {
		return execution.Settings{}, translate(err)
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// Gathering (P15-44): one read pass over every input.

type templateResolution struct {
	rec *persistence.PromptTemplateRecord
	ver *persistence.PromptTemplateVersionRecord
}

type gathered struct {
	project  *project.Project
	settings *project.Settings
	exec     execution.Settings
	engine   execution.EngineSettings
	hard     hardening.Settings

	canonicalCWD string
	cwdErrs      []execution.ReadinessError
	repo         *execution.RepositorySnapshot
	repoErr      error

	phases     []*phase.Phase
	facts      execution.RoutingFacts
	wbsVersion int
	wbsHash    string
	units      []*wbs.WorkUnit
	eligible   []*wbs.WorkUnit

	machineRec   *persistence.MachineRecord
	machine      *state.StateMachineSnapshot
	machineErr   error
	validation   *ValidationReport
	stateRecs    map[string]*persistence.WorkflowStateRecord
	path         *execution.ControlPath
	pathErr      error
	reachable    []state.StateSnapshot
	stateRoles   map[string][]*persistence.RoleRecord // reachable state key → assigned roles (ordinal order)
	fallbackRole *persistence.RoleRecord
	roleKey      string
	roleSnaps    map[string]*role.RoleSnapshot
	roleProblems []execution.ReadinessError
	templates    map[string]*templateResolution
	missingFiles []string
	noProtocol   []string
	fileErr      error
	compression  compression.Settings
	compHash     string
	compErr      error
	workerSt     worker.Settings
	workerErr    error
	workerInfo   WorkerProviderInfo
	workerKnown  bool
	// workerProbe / compProbe are the live session probe results
	// (probed = the probe ran).
	workerProbed, compProbed bool
	workerProbe, compProbe   error
	protoSt                  workerresult.Settings
	protoErr                 error
	protoReady               error
	protoRec                 *persistence.ResponseProtocolRecord
	protoVer                 *persistence.ResponseProtocolVersionRecord
	contSt                   continuation.Settings
	contErr                  error
}

// derivedStateFiles are the types bootstrap initializes itself (PLAN
// Decision 13); ACTIVE_TASK only with a resolved unit.
var derivedStateFiles = []string{"PROJECT_STATE", "PHASE_STATE", "TASK_ARCHIVE", "TEST_STATE"}

func (s *BootstrapService) gather(ctx context.Context, projectID uuid.UUID) (*gathered, error) {
	g := &gathered{stateRecs: map[string]*persistence.WorkflowStateRecord{}, stateRoles: map[string][]*persistence.RoleRecord{},
		roleSnaps: map[string]*role.RoleSnapshot{}, templates: map[string]*templateResolution{}}
	var err error
	if g.project, err = s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return nil, translateNil(err)
	}
	if g.settings, err = s.d.ProjectSettings.Get(ctx, projectID.String()); err != nil {
		return nil, translateNil(err)
	}
	if g.exec, _, err = s.d.Repo.GetSettings(ctx, projectID); err != nil {
		return nil, translateNil(err)
	}
	g.engine, g.hard = execution.DefaultEngineSettings(), hardening.DefaultSettings()
	if s.d.Engine != nil {
		if g.engine, g.hard, _, err = s.d.Engine.GetEngineSettings(ctx, projectID); err != nil {
			return nil, translateNil(err)
		}
	}
	// Working directory + repository (WBS 15.15–15.21).
	var roots []string
	if s.d.WorkerProvider != nil {
		if info, ok := s.d.WorkerProvider(g.settings.WorkerProvider); ok {
			g.workerInfo, g.workerKnown = info, true
			roots = info.AllowedRoots
		}
	}
	g.canonicalCWD, g.cwdErrs = execution.CheckWorkingDirectory(g.settings.WorkingDirectory, roots)
	if g.canonicalCWD != "" && s.d.Inspector != nil {
		snap, rerr := s.d.Inspector.Inspect(ctx, g.canonicalCWD)
		if rerr != nil {
			g.repoErr = rerr
		} else {
			g.repo = &snap
		}
	}
	// Phases / WBS / plan / units (WBS 15.22–15.27, 15.64–15.68).
	if g.phases, err = s.d.Phases.ListByProject(ctx, projectID); err != nil {
		return nil, translateNil(err)
	}
	in := execution.RoutingInput{Phases: g.phases}
	if cur := wbs.CurrentPhase(g.phases); cur != nil {
		v, verr := s.d.WBSVersions.GetCurrentVersion(ctx, cur.ID.String())
		if verr != nil && !errors.Is(verr, persistence.ErrNotFound) {
			return nil, translate(verr)
		}
		if v > 0 {
			g.wbsVersion = v
			if rec, err := s.d.WBSVersions.GetVersion(ctx, cur.ID.String(), v); err == nil {
				g.wbsHash = rec.ContentHash
			}
		}
		in.CurrentWBSVersion = g.wbsVersion
		plans, err := s.d.Plans.ListByPhase(ctx, cur.ID)
		if err != nil {
			return nil, translateNil(err)
		}
		in.Plans = plans
		if active := wbs.ActivePlan(cur.ID, plans); active != nil {
			if g.units, err = s.d.Units.ListByPlan(ctx, active.ID); err != nil {
				return nil, translateNil(err)
			}
			if g.eligible, err = s.d.Units.ListEligible(ctx, active.ID); err != nil {
				return nil, translateNil(err)
			}
			in.Units, in.EligibleUnits = g.units, g.eligible
		}
	}
	g.facts = execution.ResolveRouting(in)
	// State machine (WBS 15.28–15.30).
	machines, err := s.d.Machines.ListByProject(ctx, projectID)
	if err != nil {
		return nil, translateNil(err)
	}
	g.machineRec = pickCurrentMachine(machines)
	if g.machineRec != nil {
		g.machine, g.machineErr = s.d.MachineSnaps.Snapshot(ctx, g.machineRec.FamilyID.String(), g.machineRec.Version)
		if s.d.Validation != nil {
			g.validation, _ = s.d.Validation.Validate(ctx, g.machineRec.ID.String())
		}
		recs, err := s.d.States.ListByMachine(ctx, g.machineRec.ID, g.machineRec.Version)
		if err != nil {
			return nil, translateNil(err)
		}
		for _, r := range recs {
			g.stateRecs[r.Key] = r
		}
	}
	if g.machine != nil && g.machineRec.Enabled {
		g.reachable = execution.ReachableStates(g.machine)
		if !g.facts.CompletionEligible && g.facts.Phase != nil {
			g.path, g.pathErr = execution.ResolveFirstActionable(g.machine, g.facts, g.exec.MaxBootstrapControlTransitions)
		}
		if err := s.gatherRoles(ctx, projectID, g); err != nil {
			return nil, err
		}
		if err := s.gatherTemplates(ctx, projectID, g); err != nil {
			return nil, err
		}
		var ids []uuid.UUID
		for _, st := range g.reachable {
			if g.stateTemplateKey(st.Key) != "" && g.stateRecs[st.Key] != nil {
				ids = append(ids, g.stateRecs[st.Key].ID)
			}
		}
		pinned, err := s.d.Repo.StatesWithProtocol(ctx, ids)
		if err != nil {
			return nil, translateNil(err)
		}
		for _, st := range g.reachable {
			if rec := g.stateRecs[st.Key]; rec != nil && rec.PromptTemplate != "" && !pinned[rec.ID] {
				g.noProtocol = append(g.noProtocol, st.Key)
			}
		}
	}
	// State files (WBS 15.50/15.70/15.71).
	if g.facts.Phase != nil && s.d.ScopeRes != nil && g.path != nil {
		var unitID *uuid.UUID
		if g.facts.NextUnit != nil {
			unitID = &g.facts.NextUnit.ID
		}
		pid := g.facts.Phase.ID
		_, missing, ferr := s.d.ScopeRes.ResolveStateFileScopesAt(ctx, projectID.String(), &pid, unitID)
		if ferr != nil && !errors.Is(ferr, ErrNotFound) && !errors.Is(ferr, persistence.ErrNotFound) {
			g.fileErr = ferr
		}
		derived := map[string]bool{}
		for _, k := range derivedStateFiles {
			derived[k] = true
		}
		if unitID != nil {
			derived["ACTIVE_TASK"] = true
		}
		for _, m := range missing {
			if !derived[m.Key] {
				g.missingFiles = append(g.missingFiles, m.Key)
			}
		}
		sort.Strings(g.missingFiles)
	}
	// Compression / worker / protocol / continuation (WBS 15.36–15.39).
	if s.d.Compression != nil {
		g.compression, _, g.compHash, g.compErr = s.d.Compression.ConfigIdentity(ctx, projectID)
	}
	if s.d.Worker != nil {
		g.workerSt, _, g.workerErr = s.d.Worker.WorkerSettings(ctx, projectID)
	} else {
		g.workerSt = worker.DefaultSettings()
	}
	// Live session probes run only once the provider is configured; the
	// configuration rows already report a missing one. A passed console
	// connection test for the same provider and model stands in for the
	// probe.
	tested := func(kind, provider, model string) bool {
		return s.d.ProviderTestPassed != nil && s.d.ProviderTestPassed(kind, provider, model)
	}
	if s.d.WorkerSessionProbe != nil && g.settings.WorkerProvider != "" && g.settings.WorkerModel != "" &&
		(s.d.WorkerProvider == nil || g.workerKnown) {
		g.workerProbed = true
		if !tested("worker", g.settings.WorkerProvider, g.settings.WorkerModel) {
			g.workerProbe = s.d.WorkerSessionProbe(ctx, projectID)
		}
	}
	if s.d.CompressionSessionProbe != nil && g.settings.CompressionProvider != "" && g.settings.CompressionModel != "" {
		known := true
		if s.d.CompressionProvider != nil {
			_, known = s.d.CompressionProvider(g.settings.CompressionProvider)
		}
		if known {
			g.compProbed = true
			if !tested("compression", g.settings.CompressionProvider, g.settings.CompressionModel) {
				g.compProbe = s.d.CompressionSessionProbe(ctx, g.settings.CompressionProvider, g.settings.CompressionModel)
			}
		}
	}
	if s.d.Results != nil {
		g.protoSt, _, g.protoErr = s.d.Results.ProtocolSettings(ctx, projectID)
		g.protoReady = s.d.Results.Readiness(ctx, projectID)
	}
	if s.d.ProtocolFind != nil {
		pid := projectID
		if rec, err := s.d.ProtocolFind.FindByKind(ctx, &pid, string(prompt.KindImplementationResponse)); err == nil && rec != nil {
			g.protoRec = rec
			if s.d.Protocols != nil {
				g.protoVer, _ = s.d.Protocols.GetCurrentVersion(ctx, rec.ID)
			}
		}
	}
	if s.d.Continuation != nil {
		g.contSt, _, g.contErr = s.d.Continuation.ContinuationSettings(ctx, projectID)
	} else {
		g.contSt = continuation.DefaultSettings()
	}
	return g, nil
}

// gatherRoles resolves the roles of every reachable state (assignment
// order) and the first actionable state's role (WBS 15.31/15.69).
func (s *BootstrapService) gatherRoles(ctx context.Context, projectID uuid.UUID, g *gathered) error {
	projRoles, err := s.d.Roles.ListByScope(ctx, &projectID)
	if err != nil {
		return translate(err)
	}
	for _, r := range projRoles {
		if r.Enabled && (g.fallbackRole == nil || r.Key < g.fallbackRole.Key) {
			g.fallbackRole = r
		}
	}
	for _, st := range g.reachable {
		if execution.IsControl(st) || st.Terminal || st.Kind == string(state.KindTerminal) {
			continue
		}
		rec := g.stateRecs[st.Key]
		if rec == nil {
			continue
		}
		asg, err := s.d.Assignments.ListByState(ctx, rec.ID)
		if err != nil {
			return translate(err)
		}
		sort.SliceStable(asg, func(i, j int) bool { return asg[i].Ordinal < asg[j].Ordinal })
		var roles []*persistence.RoleRecord
		for _, a := range asg {
			r, err := s.d.Roles.Get(ctx, a.RoleID.String())
			if err != nil {
				if errors.Is(err, persistence.ErrNotFound) {
					continue
				}
				return translate(err)
			}
			roles = append(roles, r)
		}
		g.stateRoles[st.Key] = roles
	}
	// Role snapshots of every usable role (assigned enabled roles + the
	// fallback) — captured for the configuration snapshot (WBS 15.32).
	want := map[string]*persistence.RoleRecord{}
	for _, roles := range g.stateRoles {
		for _, r := range roles {
			if r.Enabled {
				want[r.Key] = r
			}
		}
	}
	if g.fallbackRole != nil {
		want[g.fallbackRole.Key] = g.fallbackRole
	}
	for key, r := range want {
		if s.d.RoleSvc == nil {
			break
		}
		snap, err := s.d.RoleSvc.SnapshotRole(ctx, r.ID.String())
		if err != nil {
			g.roleProblems = append(g.roleProblems, execution.ReadinessError{Category: execution.CategoryRoles,
				Code: execution.CodeRoleInvalid, Resource: key, Message: fmt.Sprintf("%s role cannot be snapshotted", key)})
			continue
		}
		g.roleSnaps[key] = snap
	}
	if g.path != nil {
		g.roleKey = s.roleFor(g, g.path.FirstActionable.Key)
	}
	return nil
}

// roleFor applies the Phase 7 role rule: the first enabled assigned role,
// else the project's smallest-key enabled role.
func (s *BootstrapService) roleFor(g *gathered, stateKey string) string {
	for _, r := range g.stateRoles[stateKey] {
		if r.Enabled {
			return r.Key
		}
	}
	if g.fallbackRole != nil {
		return g.fallbackRole.Key
	}
	return ""
}

func (s *BootstrapService) gatherTemplates(ctx context.Context, projectID uuid.UUID, g *gathered) error {
	for _, st := range g.reachable {
		key := g.stateTemplateKey(st.Key)
		if key == "" {
			continue
		}
		if _, done := g.templates[key]; done {
			continue
		}
		pid := projectID
		rec, err := s.d.Templates.GetByKey(ctx, &pid, key)
		if errors.Is(err, persistence.ErrNotFound) {
			rec, err = s.d.Templates.GetByKey(ctx, nil, key)
		}
		if errors.Is(err, persistence.ErrNotFound) {
			g.templates[key] = &templateResolution{}
			continue
		}
		if err != nil {
			return translate(err)
		}
		tr := &templateResolution{rec: rec}
		if rec.Enabled {
			if ver, err := s.d.TemplateVers.GetCurrentVersion(ctx, rec.ID); err == nil {
				tr.ver = ver
			}
		}
		g.templates[key] = tr
	}
	return nil
}

func (g *gathered) stateTemplateKey(stateKey string) string {
	if r := g.stateRecs[stateKey]; r != nil {
		return r.PromptTemplate
	}
	return ""
}

// ---------------------------------------------------------------------------
// Readiness (P15-45 .. P15-47).

func (s *BootstrapService) readiness(g *gathered) *execution.ExecutionReadinessReport {
	r := &execution.ExecutionReadinessReport{}
	E := execution.CategoryProject
	// Project status (WBS 15.11).
	if g.project.Status != project.StatusREADY {
		r.Add(E, execution.CodeProjectNotReady, g.project.ID.String(), fmt.Sprintf("project is %s, not READY", g.project.Status))
	}
	if err := g.exec.Validate(); err != nil {
		r.Add(E, execution.CodeConfigInvalid, "execution_settings", err.Error())
	}
	// Working directory + repository.
	r.Errors = append(r.Errors, g.cwdErrs...)
	if g.repoErr != nil {
		r.Add(execution.CategoryRepository, execution.CodeRepositoryInvalid, g.canonicalCWD, "repository cannot be inspected: "+g.repoErr.Error())
	}
	if g.repo != nil {
		errs, warns := execution.EvaluateRepositoryPolicy(*g.repo, g.exec)
		r.Errors = append(r.Errors, errs...)
		r.Warnings = append(r.Warnings, warns...)
	}
	// Phases / WBS.
	switch {
	case len(g.phases) == 0:
		r.Add(execution.CategoryPhase, execution.CodeNoPhase, "", "project has no phases")
	case g.facts.CompletionEligible:
		r.CompletionEligible = true
	default:
		ph := g.facts.Phase
		res := fmt.Sprintf("phase %d", ph.Order)
		if ph.Status == phase.StatusFAILED {
			r.Add(execution.CategoryPhase, execution.CodeConfigInvalid, res, fmt.Sprintf("phase %d — %s is FAILED", ph.Order, ph.Name))
		}
		if g.wbsVersion <= 0 {
			r.Add(execution.CategoryWBS, execution.CodeWBSInvalid, res, fmt.Sprintf("phase %d — %s has no WBS", ph.Order, ph.Name))
		}
		if g.facts.NoExecutableUnit {
			r.Add(execution.CategoryWBS, execution.CodeWBSInvalid, res, "execution plan has no executable work unit (dependencies not DONE or units FAILED/BLOCKED)")
		}
	}
	// State machine.
	SM := execution.CategoryStateMachine
	switch {
	case g.machineRec == nil:
		r.Add(SM, execution.CodeStateMachineInvalid, "", "project has no state machine")
	case !g.machineRec.Enabled:
		r.Add(SM, execution.CodeStateMachineInvalid, g.machineRec.Name, "state machine is disabled")
	case g.machineErr != nil:
		r.Add(SM, execution.CodeStateMachineInvalid, g.machineRec.Name, "state machine cannot be snapshotted: "+g.machineErr.Error())
	case g.machine.InitialState.Key == "":
		r.Add(SM, execution.CodeStateMachineInvalid, g.machineRec.Name, "state machine has no initial state")
	}
	if g.validation != nil && !g.validation.Valid {
		// Issues confined to states unreachable from the initial state
		// cannot affect this execution: they are warnings, not blockers.
		reach := map[string]bool{}
		for _, st := range g.reachable {
			reach[st.Key] = true
		}
		for _, iss := range g.validation.Errors {
			msg := fmt.Sprintf("state machine invalid: %s %s", iss.Code, iss.Detail)
			if iss.StateKey != "" && len(reach) > 0 && !reach[iss.StateKey] {
				r.Warn(SM, execution.CodeStateMachineInvalid, iss.StateKey, msg)
				continue
			}
			r.Add(SM, execution.CodeStateMachineInvalid, iss.StateKey, msg)
		}
	}
	if g.pathErr != nil {
		r.Add(SM, execution.CodeStateMachineInvalid, "", g.pathErr.Error())
	}
	// Roles (WBS 15.31/15.58).
	r.Errors = append(r.Errors, s.roleReadiness(g)...)
	// State files.
	for _, k := range g.missingFiles {
		r.Add(execution.CategoryStateFiles, execution.CodeStateFileInvalid, k, fmt.Sprintf("required state file %s is unavailable", k))
	}
	if g.fileErr != nil {
		r.Add(execution.CategoryStateFiles, execution.CodeStateFileInvalid, "", "state files cannot be resolved: "+g.fileErr.Error())
	}
	// Templates (WBS 15.34).
	keys := make([]string, 0, len(g.templates))
	for k := range g.templates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t := g.templates[k]
		switch {
		case t.rec == nil:
			r.Add(execution.CategoryTemplates, execution.CodePromptInvalid, k, k+" template is missing.")
		case !t.rec.Enabled:
			r.Add(execution.CategoryTemplates, execution.CodePromptInvalid, k, k+" template is disabled.")
		case t.ver == nil:
			r.Add(execution.CategoryTemplates, execution.CodePromptInvalid, k, k+" template has no version.")
		}
	}
	// Compression (WBS 15.36).
	C := execution.CategoryCompression
	if g.settings.CompressionProvider == "" || g.settings.CompressionModel == "" {
		r.Add(C, execution.CodeConfigInvalid, "compression", "compression provider/model is not configured")
	} else if s.d.CompressionProvider != nil {
		if _, ok := s.d.CompressionProvider(g.settings.CompressionProvider); !ok {
			r.Add(C, execution.CodeConfigInvalid, g.settings.CompressionProvider, "compression provider "+g.settings.CompressionProvider+" is not configured on this server")
		}
	}
	if g.compErr != nil {
		r.Add(C, execution.CodeConfigInvalid, "compression", "compression configuration invalid: "+g.compErr.Error())
	}
	if g.compProbed && g.compProbe != nil {
		r.Add(execution.CategoryCompressionSession, execution.CodeConfigInvalid, g.settings.CompressionProvider,
			"compression session not ready: "+g.compProbe.Error())
	}
	// Worker (WBS 15.37).
	W := execution.CategoryWorker
	if g.settings.WorkerProvider == "" || g.settings.WorkerModel == "" {
		r.Add(W, execution.CodeConfigInvalid, "worker", "worker provider/model is not configured")
	} else if s.d.WorkerProvider != nil && !g.workerKnown {
		r.Add(W, execution.CodeConfigInvalid, g.settings.WorkerProvider, "worker provider "+g.settings.WorkerProvider+" is not configured on this server")
	}
	if g.workerErr != nil {
		r.Add(W, execution.CodeConfigInvalid, "worker", "worker settings invalid: "+g.workerErr.Error())
	} else if err := g.workerSt.Validate(); err != nil {
		r.Add(W, execution.CodeConfigInvalid, "worker", "worker settings invalid: "+err.Error())
	}
	if g.workerProbed && g.workerProbe != nil {
		r.Add(execution.CategoryWorkerSession, execution.CodeConfigInvalid, g.settings.WorkerProvider,
			"worker session not ready: "+g.workerProbe.Error())
	}
	// Protocol (WBS 15.38): composition requires every prompted state to
	// pin a response protocol (Phase 8).
	for _, k := range g.noProtocol {
		r.Add(execution.CategoryProtocol, execution.CodePromptInvalid, k, k+" state has no response protocol.")
	}
	if g.protoErr != nil {
		r.Add(execution.CategoryProtocol, execution.CodeConfigInvalid, "response_protocol", g.protoErr.Error())
	} else if g.protoReady != nil {
		r.Add(execution.CategoryProtocol, execution.CodeConfigInvalid, "response_protocol", "response protocol not ready: "+g.protoReady.Error())
	}
	// Context (WBS 15.12) + continuation (WBS 15.39).
	if g.settings.ContextWindow <= 0 || g.settings.ContextThreshold <= 0 || g.settings.ContextThreshold > 1 {
		r.Add(execution.CategoryContext, execution.CodeConfigInvalid, "context", "context window/threshold configuration is invalid")
	}
	if g.contErr != nil {
		r.Add(execution.CategoryContinuation, execution.CodeConfigInvalid, "continuation", g.contErr.Error())
	} else if err := g.contSt.Validate(); err != nil {
		r.Add(execution.CategoryContinuation, execution.CodeConfigInvalid, "continuation", "continuation settings invalid: "+err.Error())
	}
	return r.Finalize()
}

// roleReadiness checks every reachable non-control state's roles.
func (s *BootstrapService) roleReadiness(g *gathered) []execution.ReadinessError {
	out := append([]execution.ReadinessError(nil), g.roleProblems...)
	validated := map[string]bool{}
	invalid := func(r *persistence.RoleRecord) []string {
		if s.d.RoleSvc == nil {
			return nil
		}
		var out []string
		for _, p := range s.d.RoleSvc.ValidateRole(context.Background(), r.ID.String()) {
			out = append(out, p.Message)
		}
		return out
	}
	keys := make([]string, 0, len(g.stateRoles))
	for k := range g.stateRoles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	needFallback := false
	for _, st := range g.reachable {
		if !execution.IsControl(st) && !st.Terminal && st.Kind != string(state.KindTerminal) && len(g.stateRoles[st.Key]) == 0 {
			needFallback = true
		}
	}
	for _, key := range keys {
		roles := g.stateRoles[key]
		if len(roles) == 0 {
			continue
		}
		usable := false
		for _, r := range roles {
			if !r.Enabled {
				continue
			}
			usable = true
			if validated[r.Key] {
				continue
			}
			validated[r.Key] = true
			if probs := invalid(r); len(probs) > 0 {
				out = append(out, execution.ReadinessError{Category: execution.CategoryRoles, Code: execution.CodeRoleInvalid,
					Resource: r.Key, Message: fmt.Sprintf("%s role is invalid: %s", r.Key, joinProblems(probs))})
			}
		}
		if !usable {
			var names []string
			for _, r := range roles {
				names = append(names, r.Key)
			}
			out = append(out, execution.ReadinessError{Category: execution.CategoryRoles, Code: execution.CodeRoleInvalid,
				Resource: key, Message: fmt.Sprintf("%s requires role %s, which is disabled", key, joinProblems(names))})
		}
	}
	if needFallback && g.fallbackRole == nil {
		out = append(out, execution.ReadinessError{Category: execution.CategoryRoles, Code: execution.CodeRoleInvalid,
			Message: "project has no enabled role"})
	}
	return out
}

func joinProblems(p []string) string {
	out := ""
	for i, s := range p {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// ---------------------------------------------------------------------------
// Plan, sources, configuration content (P15-44, P15-48, P15-49).

func hashJSON(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *BootstrapService) sources(g *gathered) execution.SourceVersions {
	st := *g.settings
	st.CreatedAt, st.UpdatedAt = time.Time{}, time.Time{}
	v := execution.SourceVersions{
		ProjectVersion:    g.project.Version,
		SettingsHash:      hashJSON(st),
		WBSVersion:        g.wbsVersion,
		WBSHash:           g.wbsHash,
		RoleHashes:        map[string]string{},
		TemplateVersions:  map[string]int{},
		CompressionHash:   hashJSON([]any{g.compHash, g.compErr != nil}),
		WorkerHash:        hashJSON([]any{g.workerSt, g.workerInfo}),
		ProtocolHash:      hashJSON([]any{g.protoSt, protoVerHash(g.protoVer)}),
		ContinuationHash:  hashJSON(g.contSt),
		ExecutionSettings: hashJSON(g.exec),
	}
	type ph struct {
		ID     uuid.UUID
		Order  int
		Status string
		Name   string
	}
	var phs []ph
	for _, p := range g.phases {
		phs = append(phs, ph{p.ID, p.Order, string(p.Status), p.Name})
	}
	sort.Slice(phs, func(i, j int) bool { return phs[i].Order < phs[j].Order })
	v.PhaseListHash = hashJSON(phs)
	if g.facts.Plan != nil {
		v.PlanID, v.PlanVersion, v.PlanStatus = g.facts.Plan.ID.String(), g.facts.Plan.Version, string(g.facts.Plan.Status)
	}
	var us []string
	for _, u := range g.units {
		us = append(us, fmt.Sprintf("%s:%d:%s", u.ID, u.Sequence, u.Status))
	}
	for _, u := range g.eligible {
		us = append(us, "eligible:"+u.ID.String())
	}
	sort.Strings(us)
	v.UnitStatusHash = hashJSON(us)
	if g.machine != nil {
		v.MachineID, v.MachineVersion, v.MachineHash = g.machineRec.FamilyID.String(), g.machineRec.Version, g.machine.Hash
	}
	for k, snap := range g.roleSnaps {
		v.RoleHashes[k] = snap.ConfigurationHash()
	}
	for _, roles := range g.stateRoles {
		for _, r := range roles {
			if !r.Enabled {
				v.RoleHashes[r.Key] = "disabled"
			}
		}
	}
	for k, t := range g.templates {
		if t.ver != nil {
			v.TemplateVersions[k] = t.ver.Version
		} else {
			v.TemplateVersions[k] = 0
		}
	}
	return v
}

func protoVerHash(v *persistence.ResponseProtocolVersionRecord) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d:%s", v.ProtocolID, v.Version, v.ContentHash)
}

func (s *BootstrapService) plan(g *gathered) *execution.BootstrapPlan {
	p := &execution.BootstrapPlan{ProjectID: g.project.ID, WorkingDirectory: g.canonicalCWD, Sources: s.sources(g)}
	if ph := g.facts.Phase; ph != nil {
		p.PhaseID, p.PhaseOrder, p.PhaseName = ph.ID, ph.Order, ph.Name
	}
	p.WBSVersion, p.WBSHash, p.PlanState = g.wbsVersion, g.wbsHash, g.facts.PlanState
	if g.facts.Plan != nil {
		id := g.facts.Plan.ID
		p.PlanID, p.PlanVersion = &id, g.facts.Plan.Version
	}
	if g.machine != nil {
		p.MachineID, p.MachineVersion, p.MachineHash = g.machineRec.FamilyID, g.machineRec.Version, g.machine.Hash
		p.InitialState = g.machine.InitialState.Key
	}
	if g.path != nil {
		p.ControlPath = []string{g.path.InitialState.Key}
		for _, t := range g.path.Transitions {
			p.ControlPath = append(p.ControlPath, t.To)
		}
		p.FirstActionable = g.path.FirstActionable.Key
		p.Role = g.roleKey
		p.TemplateKey = g.stateTemplateKey(p.FirstActionable)
		if t := g.templates[p.TemplateKey]; t != nil && t.rec != nil && t.ver != nil {
			id := t.rec.ID
			p.TemplateID, p.TemplateVersion = &id, t.ver.Version
		}
	}
	if g.facts.NextUnit != nil {
		id := g.facts.NextUnit.ID
		p.WorkUnitID = &id
	}
	return p
}

func (s *BootstrapService) content(g *gathered) execution.ConfigurationContent {
	c := execution.ConfigurationContent{
		ProjectVersion:   g.project.Version,
		WorkingDirectory: g.canonicalCWD,
		Execution:        g.exec,
	}
	eng, hard := g.engine, g.hard
	c.Engine, c.Hardening = &eng, &hard
	if g.facts.Phase != nil {
		c.WBS = execution.WBSSnapshot{PhaseID: g.facts.Phase.ID, Version: g.wbsVersion, Hash: g.wbsHash}
	}
	if g.machine != nil {
		c.StateMachine = *g.machine
	}
	for _, snap := range g.roleSnaps {
		c.Roles = append(c.Roles, *snap)
	}
	for k, t := range g.templates {
		if t.rec == nil || t.ver == nil {
			continue
		}
		c.PromptTemplates = append(c.PromptTemplates, execution.PromptTemplateSnapshot{Key: k, TemplateID: t.rec.ID, Version: t.ver.Version, ContentHash: t.ver.ContentHash})
	}
	c.Compression = execution.CompressionConfigurationSnapshot{Provider: g.settings.CompressionProvider, Model: g.settings.CompressionModel,
		Settings: g.compression, ConfigHash: g.compHash}
	if s.d.CompressionProvider != nil {
		if info, ok := s.d.CompressionProvider(g.settings.CompressionProvider); ok {
			c.Compression.Kind, c.Compression.BaseURL, c.Compression.APIKeyEnv = info.Kind, execution.StripUserinfo(info.BaseURL), info.APIKeyEnv
		}
	}
	c.Worker = execution.WorkerConfigurationSnapshot{Provider: g.settings.WorkerProvider, Model: g.settings.WorkerModel,
		Kind: g.workerInfo.Kind, Endpoint: execution.StripUserinfo(g.workerInfo.Endpoint), AuthSecretEnv: g.workerInfo.AuthSecretEnv,
		ConnectTimeout: g.workerInfo.ConnectTimeout, SessionTimeout: g.workerInfo.SessionTimeout, WorkerTimeout: g.settings.WorkerTimeout,
		MaxRetries: g.settings.MaxRetries, MaxResponseBytes: g.workerInfo.MaxResponseBytes,
		AllowedRoots: append([]string(nil), g.workerInfo.AllowedRoots...), Capabilities: append([]string(nil), g.workerInfo.Capabilities...),
		Settings: g.workerSt}
	c.ResponseProtocol = execution.ResponseProtocolSnapshot{Settings: g.protoSt}
	if g.protoRec != nil {
		id := g.protoRec.ID
		c.ResponseProtocol.ProtocolID = &id
	}
	if g.protoVer != nil {
		c.ResponseProtocol.Version, c.ResponseProtocol.ContentHash = g.protoVer.Version, g.protoVer.ContentHash
	}
	c.Continuation = execution.ContinuationConfigurationSnapshot{ContextThreshold: g.settings.ContextThreshold,
		ContextWindow: g.settings.ContextWindow, Settings: g.contSt}
	c.Normalize()
	return c
}

// ---------------------------------------------------------------------------
// Read-only surfaces built on gather (P15-62 part).

// Readiness evaluates execution readiness without writing (WBS 15.96).
func (s *BootstrapService) Readiness(ctx context.Context, projectID uuid.UUID) (*execution.ExecutionReadinessReport, error) {
	g, err := s.gather(ctx, projectID)
	if err != nil {
		return nil, err
	}
	return s.readiness(g), nil
}

// Preview resolves the bootstrap plan and readiness without writing
// (WBS 15.100/15.157).
func (s *BootstrapService) Preview(ctx context.Context, projectID uuid.UUID) (*BootstrapPreview, error) {
	g, err := s.gather(ctx, projectID)
	if err != nil {
		return nil, err
	}
	rep := s.readiness(g)
	p := s.plan(g)
	v := execution.NewPreviewView(g.project.Name, string(g.project.Status), p, rep)
	return &BootstrapPreview{Plan: p, PlanHash: p.Hash(), Readiness: rep, View: v}, nil
}

// BootstrapPreview is the preview read model.
type BootstrapPreview struct {
	Plan      *execution.BootstrapPlan
	PlanHash  string
	Readiness *execution.ExecutionReadinessReport
	View      execution.PreviewView
}

// isDerivedType reports a type bootstrap writes itself.
func isDerivedType(key string) bool {
	for _, k := range derivedStateFiles {
		if k == key {
			return true
		}
	}
	return key == "ACTIVE_TASK"
}

var _ = statefile.ScopeProject
