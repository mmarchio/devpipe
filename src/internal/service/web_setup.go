package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/hardening"
	"overseer/internal/persistence"
	"overseer/internal/project"
	"overseer/internal/wbs"
)

// FieldErrors is a validation failure carrying every field problem
// (WBS 17.9, 17.134); it unwraps to ErrInvalidInput.
type FieldErrors struct {
	Fields map[string]string
}

func (e *FieldErrors) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return "invalid input: " + strings.Join(parts, "; ")
}

func (e *FieldErrors) Unwrap() error { return ErrInvalidInput }

// engineSettingsStore reads and writes engine settings (the Engine).
type engineSettingsStore interface {
	EngineSettings(ctx context.Context, projectID uuid.UUID) (execution.EngineSettings, hardening.Settings, bool, error)
	UpdateEngineSettings(ctx context.Context, projectID uuid.UUID, s execution.EngineSettings, h hardening.Settings) error
}

// PlanningSettingsService reads and writes the console's planning policy
// (WBS 17.61, P17-22): project planning_policy / granularity plus the
// engine's AtomicPolicy.
type PlanningSettingsService struct {
	settings *SettingsService
	engine   engineSettingsStore
}

// NewPlanningSettingsService builds the service.
func NewPlanningSettingsService(settings *SettingsService, engine engineSettingsStore) *PlanningSettingsService {
	return &PlanningSettingsService{settings: settings, engine: engine}
}

// Get returns the persisted configuration.
func (s *PlanningSettingsService) Get(ctx context.Context, projectID uuid.UUID) (wbs.PlanningConfiguration, error) {
	st, err := s.settings.GetSettings(ctx, projectID.String())
	if err != nil {
		return wbs.PlanningConfiguration{}, err
	}
	es, _, _, err := s.engine.EngineSettings(ctx, projectID)
	if err != nil {
		return wbs.PlanningConfiguration{}, err
	}
	return wbs.DecodePlanningConfiguration(st.PlanningPolicy, st.PlanningGranularity, es.AtomicPolicy), nil
}

// Update validates (every field) and persists the configuration exactly.
func (s *PlanningSettingsService) Update(ctx context.Context, projectID uuid.UUID, c wbs.PlanningConfiguration) (wbs.PlanningConfiguration, error) {
	if probs := c.Validate(); len(probs) > 0 {
		return wbs.PlanningConfiguration{}, &FieldErrors{Fields: probs}
	}
	policy, gran, atomic := c.Encode()
	es, hs, _, err := s.engine.EngineSettings(ctx, projectID)
	if err != nil {
		return wbs.PlanningConfiguration{}, err
	}
	es.AtomicPolicy = atomic
	if err := s.engine.UpdateEngineSettings(ctx, projectID, es, hs); err != nil {
		return wbs.PlanningConfiguration{}, err
	}
	if err := s.settings.UpdatePlanning(ctx, projectID.String(), policy, gran); err != nil {
		return wbs.PlanningConfiguration{}, err
	}
	return s.Get(ctx, projectID)
}

// ProjectSetup is the create-project form (WBS 17.8).
type ProjectSetup struct {
	Name                string                    `json:"name"`
	Description         string                    `json:"description"`
	WorkingDirectory    string                    `json:"working_directory"`
	WorkerProvider      string                    `json:"worker_provider"`
	WorkerModel         string                    `json:"worker_model"`
	CompressionProvider string                    `json:"compression_provider"`
	CompressionModel    string                    `json:"compression_model"`
	ContextWindow       int                       `json:"context_window"`
	ContextThreshold    float64                   `json:"context_threshold"`
	Planning            wbs.PlanningConfiguration `json:"planning"`
	CheckpointPolicy    string                    `json:"checkpoint_policy"`
	// UseGit sets the checkpoint policies when CheckpointPolicy is empty:
	// REQUIRED with git, DISABLED (the default) without.
	UseGit bool `json:"use_git"`
}

// ProjectSetupService creates a fully configured project through the
// Phase 3 project / settings services and the engine settings (P17-47).
type ProjectSetupService struct {
	projects   *ProjectService
	settings   *SettingsService
	planning   *PlanningSettingsService
	engine     engineSettingsStore
	workers    func() []string
	compressor func() []string
	init       *ProjectInitializer
}

// NewProjectSetupService builds the service; workers / compressors list
// the configured provider names.
func NewProjectSetupService(projects *ProjectService, settings *SettingsService, planning *PlanningSettingsService, engine engineSettingsStore,
	workers, compressors func() []string) *ProjectSetupService {
	return &ProjectSetupService{projects: projects, settings: settings, planning: planning, engine: engine, workers: workers, compressor: compressors}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Validate returns every field problem (empty when valid). The backend is
// authoritative: browser validation never replaces this (WBS 17.9).
func (s *ProjectSetupService) Validate(in ProjectSetup) map[string]string {
	f := map[string]string{}
	name := strings.TrimSpace(in.Name)
	switch {
	case name == "":
		f["name"] = "required"
	case len(name) > 200:
		f["name"] = "at most 200 characters"
	}
	if len(in.Description) > 4000 {
		f["description"] = "at most 4000 characters"
	}
	if in.WorkingDirectory == "" {
		f["working_directory"] = "required"
	} else if st := project.ValidateWorkingDirectory(in.WorkingDirectory); !st.Valid {
		f["working_directory"] = "must be an existing, accessible directory"
	}
	if in.WorkerProvider == "" {
		f["worker_provider"] = "required"
	} else if s.workers != nil && !contains(s.workers(), in.WorkerProvider) {
		f["worker_provider"] = "unknown worker provider"
	}
	if strings.TrimSpace(in.WorkerModel) == "" {
		f["worker_model"] = "required"
	}
	if in.CompressionProvider != "" {
		if s.compressor != nil && !contains(s.compressor(), in.CompressionProvider) {
			f["compression_provider"] = "unknown compression provider"
		}
		if strings.TrimSpace(in.CompressionModel) == "" {
			f["compression_model"] = "required when a compression provider is set"
		}
	} else if in.CompressionModel != "" {
		f["compression_model"] = "requires a compression provider"
	}
	if in.ContextWindow <= 0 {
		f["context_window"] = "must be a positive token count"
	}
	if in.ContextThreshold <= 0 || in.ContextThreshold > 1 {
		f["context_threshold"] = "must be in (0, 1]"
	}
	for k, v := range in.Planning.Validate() {
		f["planning."+k] = v
	}
	switch execution.CheckpointPolicy(in.CheckpointPolicy) {
	case "", execution.CheckpointRequired, execution.CheckpointBestEffort, execution.CheckpointDisabled:
	default:
		f["checkpoint_policy"] = "must be REQUIRED, BEST_EFFORT or DISABLED"
	}
	return f
}

// Create validates everything first, then creates and configures the
// project; a failure after creation deletes the created project.
func (s *ProjectSetupService) Create(ctx context.Context, in ProjectSetup) (*project.Project, error) {
	if f := s.Validate(in); len(f) > 0 {
		return nil, &FieldErrors{Fields: f}
	}
	p, st, err := s.projects.CreateProject(ctx, strings.TrimSpace(in.Name), in.Description)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*project.Project, error) {
		_, _ = s.projects.DeleteProject(context.WithoutCancel(ctx), p.ID.String())
		return nil, err
	}
	st.WorkingDirectory, st.WorkerProvider, st.WorkerModel = in.WorkingDirectory, in.WorkerProvider, in.WorkerModel
	st.CompressionProvider, st.CompressionModel = in.CompressionProvider, in.CompressionModel
	st.ContextWindow, st.ContextThreshold = in.ContextWindow, in.ContextThreshold
	if st.WorkerTimeout <= 0 {
		st.WorkerTimeout = DefaultWorkerTimeout
	}
	if _, err := s.settings.UpdateSettings(ctx, st, p.Version); err != nil {
		return fail(err)
	}
	es, hs, _, err := s.engine.EngineSettings(ctx, p.ID)
	if err != nil {
		return fail(err)
	}
	es.CheckpointPolicy = execution.CheckpointPolicy(in.CheckpointPolicy)
	if es.CheckpointPolicy == "" {
		es.CheckpointPolicy = execution.CheckpointPolicyFor(in.UseGit)
	}
	hs.GroupCheckpointPolicy = string(execution.CheckpointPolicyFor(es.CheckpointPolicy.UsesGit()))
	if err := s.engine.UpdateEngineSettings(ctx, p.ID, es, hs); err != nil {
		return fail(err)
	}
	if _, err := s.planning.Update(ctx, p.ID, in.Planning); err != nil {
		return fail(err)
	}
	if s.init != nil {
		if err := s.init.Initialize(ctx, p.ID); err != nil {
			return fail(err)
		}
	}
	out, err := s.projects.GetProject(ctx, p.ID.String())
	if err != nil {
		return fail(err)
	}
	return out, nil
}

// errFieldsOf extracts field errors from err (nil when it has none).
func ErrFieldsOf(err error) map[string]string {
	var fe *FieldErrors
	if errors.As(err, &fe) {
		return fe.Fields
	}
	return nil
}

// DefaultStateRoles are the default workflow's state → role assignments
// (in order) a new project receives; each assigned role is allowed the
// state. "plan phase <N>" is worked by the ARCHITECT, then the PLANNER.
var DefaultStateRoles = map[string][]string{"SKILL": {"PROJECT_MANAGER"}, "PLAN": {"ARCHITECT", "PLANNER"}, "ACCEPTANCE": {"PROJECT_MANAGER"},
	"IMPLEMENT": {"ENGINEER"}, "CONTINUE": {"ENGINEER"}, "TEST": {"TESTER"}, "HARDENING_FIX": {"ENGINEER"},
	"VALIDATE": {"TESTER"}, "PHASE_VALIDATE": {"TESTER"}}

// ProjectInitializer gives a new project its executable configuration from
// the global defaults (WBS 17.183 / 17.200: a project configured through the
// web reaches readiness without database manipulation): project roles, the
// default workflow, default state → role assignments and allowed states,
// and the response protocol pinned by every prompted state.
type ProjectInitializer struct {
	Roles     *RoleService
	Templates *StateTemplateService
	States    persistence.WorkflowStateRepository
	Config    *StateConfigService
	Results   *ResultService
	Protocols persistence.ResponseProtocolRepository
}

// Initialize performs the initialization (each step idempotent where the
// underlying service is).
func (in *ProjectInitializer) Initialize(ctx context.Context, projectID uuid.UUID) error {
	roles, err := in.Roles.InitializeProjectRoles(ctx, projectID.String())
	if err != nil {
		return fmt.Errorf("initialize roles: %w", err)
	}
	roleIDs := map[string]string{}
	for _, r := range roles {
		roleIDs[r.Key] = r.ID.String()
	}
	// The default workflow template is seeded on first use (idempotent,
	// advisory-locked), so a fresh installation can initialize projects
	// without a separate seeding step (P18-20).
	if _, err := in.Templates.SeedDefaultTemplate(ctx); err != nil {
		return fmt.Errorf("seed default workflow template: %w", err)
	}
	mid, err := in.Templates.CopyToProject(ctx, projectID.String())
	if err != nil {
		return fmt.Errorf("copy default workflow: %w", err)
	}
	machine, err := uuid.Parse(mid)
	if err != nil {
		return ErrInfrastructure
	}
	allowed := map[string][]string{"PROJECT_MANAGER": {"PHASE_VALIDATE"}}
	keys := make([]string, 0, len(DefaultStateRoles))
	for k := range DefaultStateRoles {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	states, err := in.States.ListByMachine(ctx, machine, 1)
	if err != nil {
		return translate(err)
	}
	present := map[string]bool{}
	for _, s := range states {
		present[s.Key] = true
	}
	for _, state := range keys {
		for _, role := range DefaultStateRoles[state] {
			if !present[state] || roleIDs[role] == "" {
				continue
			}
			if _, err := in.Config.AssignRole(ctx, projectID.String(), state, roleIDs[role]); err != nil && !errors.Is(err, ErrConflict) {
				return fmt.Errorf("assign %s to %s: %w", role, state, err)
			}
			allowed[role] = append(allowed[role], state)
		}
	}
	for role, sts := range allowed {
		if id := roleIDs[role]; id != "" {
			if _, err := in.Roles.SetAllowedStates(ctx, id, sts); err != nil {
				return fmt.Errorf("allowed states %s: %w", role, err)
			}
		}
	}
	ref, err := in.Results.EnsureProtocol(ctx, projectID)
	if err != nil {
		return fmt.Errorf("response protocol: %w", err)
	}
	for _, s := range states {
		if s.PromptTemplate == "" {
			continue
		}
		if _, err := in.Protocols.CreateStateReference(ctx, s.ID, ref.ProtocolID, ref.Version); err != nil && !errors.Is(err, persistence.ErrConflict) {
			return fmt.Errorf("pin protocol for %s: %w", s.Key, translate(err))
		}
	}
	return nil
}

// WithInitializer makes Create initialize the project from the global
// defaults.
func (s *ProjectSetupService) WithInitializer(in *ProjectInitializer) *ProjectSetupService {
	s.init = in
	return s
}
