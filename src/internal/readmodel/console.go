package readmodel

import "time"

// ---------------------------------------------------------------------------
// Console DTOs (WBS 17.4–17.114). Every value is computed by Go services
// from persisted state; templates only render them.

// StatusCount is one status bucket.
type StatusCount struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// Dashboard is the main dashboard (WBS 17.4).
type Dashboard struct {
	ProjectsTotal      int           `json:"projects_total"`
	ByStatus           []StatusCount `json:"by_status"`
	Running            int           `json:"running"`
	Paused             int           `json:"paused"`
	RecoveryRequired   int           `json:"recovery_required"`
	Completed          int           `json:"completed"`
	Failed             int           `json:"failed"`
	ActiveExecutions   int           `json:"active_executions"`
	ActiveSessions     int           `json:"active_sessions"`
	ActiveDispatches   int           `json:"active_dispatches"`
	RunningValidations int           `json:"running_validations"`
	RecentEvents       []TimelineRow `json:"recent_events"`
}

// ProjectRow is one project list row (WBS 17.5).
type ProjectRow struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Status          string    `json:"status"`
	CurrentPhase    string    `json:"current_phase"`
	PhaseStatus     string    `json:"phase_status"`
	Progress        Progress  `json:"progress"`
	CurrentState    string    `json:"current_state"`
	CurrentTask     string    `json:"current_task"`
	LastActivity    time.Time `json:"last_activity"`
	ExecutionStatus string    `json:"execution_status"`
	Active          bool      `json:"active"`
	Recovery        bool      `json:"recovery_required"`
	// Deletable reports whether the project's status permits deletion.
	Deletable bool `json:"deletable"`
}

// Page carries server-side pagination facts.
type Page struct {
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// HasNext reports a following page.
func (p Page) HasNext() bool { return p.Offset+p.Limit < p.Total }

// HasPrev reports a preceding page.
func (p Page) HasPrev() bool { return p.Offset > 0 }

// NextOffset / PrevOffset compute neighbour offsets.
func (p Page) NextOffset() int { return p.Offset + p.Limit }

// PrevOffset is the previous page's offset (never negative).
func (p Page) PrevOffset() int {
	if p.Offset-p.Limit < 0 {
		return 0
	}
	return p.Offset - p.Limit
}

// ProjectList is one project list page (WBS 17.5–17.7).
type ProjectList struct {
	Rows []ProjectRow `json:"projects"`
	Page Page         `json:"page"`
}

// ExecutionSummary is the current / latest execution.
type ExecutionSummary struct {
	ID             string     `json:"id"`
	Number         int        `json:"number"`
	Status         string     `json:"status"`
	Generation     int        `json:"generation"`
	Active         bool       `json:"active"`
	WaitReason     string     `json:"wait_reason,omitempty"`
	RecoveryReason string     `json:"recovery_reason,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// PhaseRef identifies a phase.
type PhaseRef struct {
	ID     string `json:"id"`
	Order  int    `json:"order"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// PlanRef identifies an execution plan and its WBS source.
type PlanRef struct {
	ID        string `json:"id"`
	Version   int    `json:"version"`
	SourceWBS int    `json:"source_wbs_version"`
	Status    string `json:"status"`
	Kind      string `json:"kind"`
}

// Staleness is the backend plan staleness state (WBS 17.23).
type Staleness struct {
	State      string `json:"state"`
	Stale      bool   `json:"stale"`
	CurrentWBS int    `json:"current_wbs_version"`
	PlanSource int    `json:"plan_source_version"`
}

// UnitRef identifies a work unit with its display stage.
type UnitRef struct {
	ID     string    `json:"id"`
	Key    string    `json:"key"`
	Name   string    `json:"name"`
	Status string    `json:"status"`
	Kind   string    `json:"kind"`
	Stage  UnitStage `json:"stage"`
}

// RuntimeRef is the runtime workflow position.
type RuntimeRef struct {
	StateKey  string     `json:"state_key"`
	StateName string     `json:"state_name"`
	Kind      string     `json:"kind"`
	EnteredAt *time.Time `json:"entered_at,omitempty"`
}

// CurrentTask is the current task panel (WBS 17.31).
type CurrentTask struct {
	UnitID    string     `json:"unit_id"`
	Key       string     `json:"key"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	Stage     UnitStage  `json:"stage"`
	Role      string     `json:"role"`
	Attempt   int        `json:"attempt"`
	SessionID string     `json:"session_id,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

// Overview is the project overview (WBS 17.10–17.14, 17.23, 17.164).
type Overview struct {
	ProjectID      string            `json:"project_id"`
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	Status         string            `json:"status"`
	Deletable      bool              `json:"deletable"`
	ProjectVersion int               `json:"project_version"`
	Execution      *ExecutionSummary `json:"execution,omitempty"`
	Phase          *PhaseRef         `json:"current_phase,omitempty"`
	PhasesDone     int               `json:"phases_done"`
	PhasesTotal    int               `json:"phases_total"`
	Progress       Progress          `json:"wbs_progress"`
	Context        ContextUsage      `json:"worker_context"`
	Plan           *PlanRef          `json:"plan,omitempty"`
	Staleness      Staleness         `json:"staleness"`
	CurrentUnit    *UnitRef          `json:"current_unit,omitempty"`
	Runtime        RuntimeRef        `json:"runtime"`
	Actions        []string          `json:"allowed_actions"`
	Recovery       bool              `json:"recovery_required"`
	CurrentTask    *CurrentTask      `json:"current_task,omitempty"`
	UpdatedAt      time.Time         `json:"updated_at"`
	// LatestEventSeq is the project's latest committed event sequence when
	// the overview was read; live streams resume after it.
	LatestEventSeq int64 `json:"latest_event_seq"`
}

// HasAction reports whether a backend-allowed action is present.
func (o *Overview) HasAction(a string) bool {
	for _, x := range o.Actions {
		if x == a {
			return true
		}
	}
	return false
}

// PhaseRow is one phase list row (WBS 17.15 / 17.16).
type PhaseRow struct {
	ID          string     `json:"id"`
	Order       int        `json:"order"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Status      string     `json:"status"`
	Progress    Progress   `json:"progress"`
	Current     bool       `json:"current"`
	WBSVersion  int        `json:"wbs_version"`
	WBSHash     string     `json:"wbs_hash"`
	Plan        *PlanRef   `json:"plan,omitempty"`
	Staleness   Staleness  `json:"staleness"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// Movable reports whether the phase can be reordered; LockReason says
	// why not.
	Movable    bool   `json:"movable"`
	LockReason string `json:"lock_reason,omitempty"`
}

// KindCount is one unit-kind bucket of a plan (WBS 17.24).
type KindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

// PhaseDetail is the phase page (WBS 17.18 / 17.24).
type PhaseDetail struct {
	Phase       PhaseRow          `json:"phase"`
	KindCounts  []KindCount       `json:"kind_counts"`
	Validations []ValidationRun   `json:"phase_validations"`
	Events      []TimelineRow     `json:"events"`
	Execution   *ExecutionSummary `json:"execution,omitempty"`
}

// UnitRow is one work-unit table row (WBS 17.25–17.30).
type UnitRow struct {
	ID          string     `json:"id"`
	Sequence    int        `json:"sequence"`
	Key         string     `json:"key"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Stage       UnitStage  `json:"stage"`
	Deps        []string   `json:"dependencies"`
	Blocking    []string   `json:"blocked_by"`
	Readiness   string     `json:"readiness"`
	Attempts    int        `json:"attempts"`
	Validation  string     `json:"validation"`
	Checkpoint  string     `json:"checkpoint"`
	Current     bool       `json:"current"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// UnitPage is one work-unit page.
type UnitPage struct {
	ProjectID string    `json:"project_id"`
	PhaseID   string    `json:"phase_id"`
	Plan      *PlanRef  `json:"plan,omitempty"`
	Rows      []UnitRow `json:"units"`
	Page      Page      `json:"page"`
}

// Edge is one persisted dependency edge: From must be DONE before To.
type Edge struct {
	FromID  string `json:"from_id"`
	FromKey string `json:"from_key"`
	ToID    string `json:"to_id"`
	ToKey   string `json:"to_key"`
}

// AcceptanceItem is one acceptance criterion with its deterministic verdict
// (WBS 17.91): manual / review items are never shown as passed without
// approval.
type AcceptanceItem struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	Verification string `json:"verification"`
	Verdict      string `json:"verdict"`
	Label        string `json:"label"`
	Symbol       string `json:"symbol"`
}

// DispatchRow is one worker dispatch (WBS 17.83).
type DispatchRow struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	Role        string     `json:"role"`
	PromptID    string     `json:"prompt_id"`
	Status      string     `json:"status"`
	Delivery    string     `json:"delivery"`
	DeliveryTag string     `json:"delivery_label"`
	Result      string     `json:"result,omitempty"`
	Attempt     int        `json:"attempt"`
	Model       string     `json:"model"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	DurationMS  int64      `json:"duration_ms"`
	UnitID      string     `json:"work_unit_id,omitempty"`
	SessionID   string     `json:"session_id"`
}

// ContinuationRow is one continuation (WBS 17.86 / 17.87).
type ContinuationRow struct {
	ID             string     `json:"id"`
	Number         int        `json:"number"`
	Trigger        string     `json:"trigger"`
	Status         string     `json:"status"`
	Strategy       string     `json:"strategy"`
	StrategyLabel  string     `json:"strategy_label"`
	Reason         string     `json:"reason"`
	ContextBefore  *float64   `json:"context_before_percent,omitempty"`
	Completed      int        `json:"completed"`
	Remaining      int        `json:"remaining"`
	Blocking       int        `json:"blocking"`
	Reset          bool       `json:"session_reset"`
	OldSession     string     `json:"old_session,omitempty"`
	NewSession     string     `json:"new_session,omitempty"`
	UnitID         string     `json:"work_unit_id"`
	DispatchID     string     `json:"dispatch_id,omitempty"`
	RecoveryReason string     `json:"recovery_reason,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

// ValidationRun is one validation attempt (WBS 17.88–17.90).
type ValidationRun struct {
	ID          string           `json:"id"`
	ExecutionID string           `json:"execution_id"`
	Scope       string           `json:"scope"`
	UnitID      string           `json:"work_unit_id,omitempty"`
	PhaseID     string           `json:"phase_id,omitempty"`
	Label       string           `json:"label"`
	Attempt     int              `json:"attempt"`
	Status      string           `json:"status"`
	Commands    int              `json:"commands"`
	Failed      int              `json:"failed"`
	CWD         string           `json:"cwd"`
	Acceptance  []AcceptanceItem `json:"acceptance"`
	Overall     string           `json:"overall"`
	StartedAt   time.Time        `json:"started_at"`
	FinishedAt  *time.Time       `json:"finished_at,omitempty"`
}

// CommandResult is one validation command (WBS 17.89). Output fields hold
// only the first bytes; the rest is range-loaded on demand (17.121).
type CommandResult struct {
	ID          string   `json:"id"`
	Ord         int      `json:"ord"`
	Name        string   `json:"name"`
	Executable  string   `json:"executable"`
	Args        []string `json:"args"`
	CWD         string   `json:"cwd"`
	ExitCode    *int     `json:"exit_code"`
	DurationMS  int64    `json:"duration_ms"`
	TimeoutMS   int64    `json:"timeout_ms"`
	TimedOut    bool     `json:"timed_out"`
	Outcome     string   `json:"outcome"`
	InfraReason string   `json:"infra_reason,omitempty"`
	StdoutHead  string   `json:"stdout_head"`
	StderrHead  string   `json:"stderr_head"`
	StdoutLen   int      `json:"stdout_length"`
	StderrLen   int      `json:"stderr_length"`
	// StdoutNext / StderrNext are the character offsets after the heads.
	StdoutNext int  `json:"stdout_next"`
	StderrNext int  `json:"stderr_next"`
	Truncated  bool `json:"truncated"`
}

// ValidationDetail is one run with its commands.
type ValidationDetail struct {
	Run      ValidationRun   `json:"run"`
	Commands []CommandResult `json:"commands"`
}

// FixAttempt is one fix attempt (WBS 17.92).
type FixAttempt struct {
	ID            string    `json:"id"`
	Attempt       int       `json:"attempt"`
	Max           int       `json:"max"`
	TargetKind    string    `json:"target_kind"`
	TargetID      string    `json:"target_id"`
	Reason        string    `json:"reason"`
	FailedRunID   string    `json:"failed_run_id,omitempty"`
	Role          string    `json:"role"`
	Status        string    `json:"status"`
	DispatchID    string    `json:"dispatch_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	FailedCommand []string  `json:"failed_commands,omitempty"`
}

// Checkpoint is one repository checkpoint (WBS 17.93).
type Checkpoint struct {
	ID               string     `json:"id"`
	ExecutionID      string     `json:"execution_id"`
	Kind             string     `json:"kind"`
	Policy           string     `json:"policy"`
	Status           string     `json:"status"`
	Commit           string     `json:"commit"`
	ShortCommit      string     `json:"short_commit"`
	Parent           string     `json:"parent"`
	Tree             string     `json:"tree"`
	TargetID         string     `json:"target_id,omitempty"`
	TargetLabel      string     `json:"target_label,omitempty"`
	ValidationRunID  string     `json:"validation_run_id,omitempty"`
	ValidationStatus string     `json:"validation_status,omitempty"`
	Error            string     `json:"error,omitempty"`
	Key              string     `json:"key"`
	CreatedAt        time.Time  `json:"created_at"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
}

// RepositoryStatus is the observational repository panel (WBS 17.94 /
// 17.95).
type RepositoryStatus struct {
	Available          bool        `json:"available"`
	Error              string      `json:"error,omitempty"`
	Branch             string      `json:"branch"`
	Head               string      `json:"head"`
	Dirty              bool        `json:"dirty"`
	BaselineHead       string      `json:"baseline_head,omitempty"`
	BaselineDirty      bool        `json:"baseline_dirty"`
	PreExistingChanges []string    `json:"pre_existing_changes"`
	OverseerChanges    []string    `json:"overseer_changes"`
	ChangesTracked     bool        `json:"changes_tracked"`
	LatestCheckpoint   *Checkpoint `json:"latest_checkpoint,omitempty"`
}

// Recovery is the recovery dashboard + evidence (WBS 17.96 / 17.97).
type Recovery struct {
	Required          bool           `json:"required"`
	ExecutionID       string         `json:"execution_id"`
	ExecutionStatus   string         `json:"execution_status"`
	Reason            string         `json:"reason"`
	AffectedUnit      *UnitRef       `json:"affected_unit,omitempty"`
	RepositoryChanged *bool          `json:"repository_modified_since_dispatch,omitempty"`
	AutomaticStopped  bool           `json:"automatic_execution_stopped"`
	Evidence          map[string]any `json:"evidence"`
	Dispatch          *DispatchRow   `json:"dispatch,omitempty"`
	PromptHash        string         `json:"prompt_hash,omitempty"`
	// Prompt is what the evidence dispatch sent to the model; Response the
	// model's raw reply (nil when none was received).
	Prompt     *RecoveryPrompt   `json:"prompt,omitempty"`
	Response   *RecoveryResponse `json:"response,omitempty"`
	Validation *ValidationRun    `json:"validation,omitempty"`
	Events     []TimelineRow     `json:"events"`
	Actions    []RecoveryAction  `json:"actions"`
	Repository *RepositoryStatus `json:"repository,omitempty"`
	// Condition / Classification / PlanHash describe the deterministic
	// recovery plan (WBS 18.7–18.9); PlanHash is the evidence identity an
	// operator action records (WBS 18.182).
	Condition      string `json:"condition,omitempty"`
	Classification string `json:"classification,omitempty"`
	PlanHash       string `json:"plan_hash,omitempty"`
}

// RecoveryPrompt is the prompt the recovery evidence dispatch sent.
type RecoveryPrompt struct {
	Content string `json:"content"`
	Hash    string `json:"hash"`
	// Source is COMMAND (a workflow command sent as is), CANONICAL or
	// COMPRESSED (a composed prompt).
	Source string `json:"source"`
}

// RecoveryResponse is the model response recorded for the recovery
// evidence dispatch.
type RecoveryResponse struct {
	Content     string    `json:"content"`
	ContentHash string    `json:"content_hash"`
	CompletedAt time.Time `json:"completed_at"`
}

// RecoveryAction is one backend-approved recovery action (WBS 17.98).
type RecoveryAction struct {
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	HighImpact bool   `json:"high_impact"`
}

// TimelineRow is one persisted execution event (WBS 17.80).
type TimelineRow struct {
	Seq         int64             `json:"seq"`
	ProjectSeq  int64             `json:"project_seq"`
	At          time.Time         `json:"at"`
	Type        string            `json:"type"`
	Category    string            `json:"category"`
	Label       string            `json:"label"`
	Status      string            `json:"status,omitempty"`
	Refs        map[string]string `json:"refs,omitempty"`
	ExecutionID string            `json:"execution_id,omitempty"`
}

// Timeline is one timeline page (keyset: NextBefore is the cursor of the
// next, older page; 0 at the end).
type Timeline struct {
	ExecutionID string        `json:"execution_id"`
	Rows        []TimelineRow `json:"events"`
	NextBefore  int64         `json:"next_before"`
	Categories  []string      `json:"categories"`
}

// FlowStep is one step of a live flow panel (WBS 17.167–17.171).
type FlowStep struct {
	Label  string `json:"label"`
	State  string `json:"state"` // DONE | ACTIVE | PENDING | FAILED
	Detail string `json:"detail,omitempty"`
}

// Flow is an ordered live flow.
type Flow struct {
	Name  string     `json:"name"`
	Steps []FlowStep `json:"steps"`
}

// SessionPanel is the worker session panel (WBS 17.82).
type SessionPanel struct {
	Present      bool   `json:"present"`
	Provider     string `json:"provider"`
	SessionID    string `json:"session_id"`
	ProviderSess string `json:"provider_session_id"`
	// ProviderStats are the provider's own counters for the session
	// (totals over the session; nil when the provider reports none).
	ProviderStats *ProviderSessionStats `json:"provider_stats,omitempty"`
	Status        string                `json:"status"`
	Model         string                `json:"model"`
	Context       ContextUsage          `json:"context"`
	CWD           string                `json:"cwd"`
	CreatedAt     *time.Time            `json:"created_at,omitempty"`
	LastActivity  *time.Time            `json:"last_activity,omitempty"`
	Connection    string                `json:"connection"`
}

// WorkerResult is a structured worker report (WBS 17.84): ADVISORY.
type WorkerResult struct {
	ID         string    `json:"id"`
	DispatchID string    `json:"dispatch_id"`
	Status     string    `json:"status"`
	Summary    string    `json:"summary"`
	Completed  []string  `json:"completed"`
	Remaining  []string  `json:"remaining"`
	Blocking   []string  `json:"blocking"`
	NextAction string    `json:"next_action"`
	TestsClaim string    `json:"tests_claim"`
	Advisory   bool      `json:"advisory"`
	Report     Report    `json:"report"`
	UnitID     string    `json:"work_unit_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// UnitDetail is the work-unit page (WBS 17.27–17.29, 17.85, 17.168).
type UnitDetail struct {
	Unit          UnitRow           `json:"unit"`
	PhaseID       string            `json:"phase_id"`
	PlanID        string            `json:"plan_id"`
	PlanVersion   int               `json:"plan_version"`
	Objective     string            `json:"objective"`
	Instructions  string            `json:"instructions"`
	Description   string            `json:"description"`
	Acceptance    []AcceptanceItem  `json:"acceptance_criteria"`
	Tests         []string          `json:"tests"`
	Constraints   []string          `json:"constraints"`
	Validation    []string          `json:"validation_commands"`
	Paths         []string          `json:"paths"`
	Report        Report            `json:"report"`
	Incoming      []Edge            `json:"depends_on"`
	Dispatches    []DispatchRow     `json:"worker_attempts"`
	Results       []WorkerResult    `json:"worker_results"`
	Continuations []ContinuationRow `json:"continuations"`
	Validations   []ValidationRun   `json:"validations"`
	FixAttempts   []FixAttempt      `json:"fix_attempts"`
	Checkpoints   []Checkpoint      `json:"checkpoints"`
	Events        []TimelineRow     `json:"history"`
	Stages        Flow              `json:"stages"`
}

// ProviderHealth is one provider row (WBS 17.108).
type ProviderHealth struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// SystemStatus is the system page (WBS 17.107–17.109).
type SystemStatus struct {
	Database           string           `json:"database"`
	SchedulerEnabled   bool             `json:"scheduler_enabled"`
	SchedulerInterval  string           `json:"scheduler_interval"`
	Runnable           int              `json:"runnable_executions"`
	Waiting            int              `json:"waiting_executions"`
	ActiveDispatches   int              `json:"active_dispatches"`
	ActiveValidations  int              `json:"active_validations"`
	ValidationCapacity int              `json:"validation_capacity"`
	ActiveExecutions   int              `json:"active_executions"`
	Providers          []ProviderHealth `json:"providers"`
	Version            string           `json:"version"`
	Profile            string           `json:"profile"`
	SchemaVersion      int              `json:"schema_version"`
	InstanceID         string           `json:"instance_id"`
	StartedAt          time.Time        `json:"started_at"`
	Uptime             string           `json:"uptime"`
	// AuditVerification is the latest audit chain verification status
	// (VALID / INVALID / INCOMPLETE; "" before the first, WBS 18.110).
	AuditVerification string `json:"audit_verification"`
	// AuditAcknowledged: the current audit failure was acknowledged.
	AuditAcknowledged bool `json:"audit_acknowledged"`
	// NewWorkBlocked lists why new work is blocked (decision U9).
	NewWorkBlocked []string `json:"new_work_blocked"`
}

// Metric is one labeled value.
type Metric struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
	Known bool    `json:"known"`
}

// PhaseMetrics is one phase's metrics (WBS 17.111).
type PhaseMetrics struct {
	PhaseID            string  `json:"phase_id"`
	Order              int     `json:"order"`
	Name               string  `json:"name"`
	Status             string  `json:"status"`
	DurationSeconds    float64 `json:"duration_seconds"`
	Units              int     `json:"units"`
	PlanAttempts       int     `json:"plan_attempts"`
	Continuations      int     `json:"continuations"`
	ValidationAttempts int     `json:"validation_attempts"`
	Fixes              int     `json:"fixes"`
}

// Metrics is the metrics page (WBS 17.110–17.114).
type Metrics struct {
	Execution []Metric       `json:"execution"`
	Context   []Metric       `json:"context"`
	Planning  []Metric       `json:"planning"`
	Prompt    []Metric       `json:"prompt"`
	Phases    []PhaseMetrics `json:"phases"`
}

// ProviderInfo is a configured provider as shown to the console (WBS
// 17.59 / 17.60 / 17.108 / 17.122): the endpoint host only, and the secret
// as a reference name plus presence — never the value.
type ProviderInfo struct {
	Kind             string   `json:"kind"`
	Name             string   `json:"name"`
	ProviderKind     string   `json:"provider_kind"`
	EndpointHost     string   `json:"endpoint_host"`
	SecretRef        string   `json:"secret_ref"`
	SecretConfigured bool     `json:"secret_configured"`
	Capabilities     []string `json:"capabilities"`
}

// ProviderTest is the outcome of a provider connection test: one
// capabilities prompt sent to the provider's model.
type ProviderTest struct {
	Kind       string `json:"kind"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Prompt     string `json:"prompt"`
	OK         bool   `json:"ok"`
	Response   string `json:"response,omitempty"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// ConfigDrift lists configuration changed after the active execution
// captured its configuration (WBS 18.4). The execution keeps its captured
// snapshot; the drift is informational.
type ConfigDrift struct {
	ExecutionID string      `json:"execution_id,omitempty"`
	Items       []DriftItem `json:"items"`
}

// DriftItem is one drifted area.
type DriftItem struct {
	Source  string `json:"source"`
	Section string `json:"section"`
	Detail  string `json:"detail"`
}

// ProviderSessionStats are a worker provider session's running counters
// (Hermes /api/sessions/{id}).
type ProviderSessionStats struct {
	Messages        int64 `json:"messages"`
	ToolCalls       int64 `json:"tool_calls"`
	ModelCalls      int64 `json:"model_calls"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
}
