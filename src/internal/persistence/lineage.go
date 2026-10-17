package persistence

import "time"

// Lineage of one work unit (P18-72, WBS 18.205): every artifact that led
// to its result, by id and hash — never large content.

// LineageStateFile is one state file captured in a context snapshot.
type LineageStateFile struct {
	TypeKey     string `json:"type_key"`
	Scope       string `json:"scope"`
	Version     int    `json:"version"`
	ContentHash string `json:"content_hash"`
}

// LineagePrompt is the prompt chain of one dispatch.
type LineagePrompt struct {
	WorkerPromptID      string             `json:"worker_prompt_id"`
	WorkerPromptHash    string             `json:"worker_prompt_hash"`
	WorkerPromptSource  string             `json:"worker_prompt_source"`
	CanonicalPromptID   string             `json:"canonical_prompt_id"`
	CanonicalPromptHash string             `json:"canonical_prompt_hash"`
	TemplateID          string             `json:"template_id"`
	TemplateVersion     int                `json:"template_version"`
	RoleID              string             `json:"role_id"`
	RoleVersion         int                `json:"role_version"`
	StateKey            string             `json:"state_key"`
	ProjectionID        string             `json:"projection_id,omitempty"`
	ContextSnapshotID   string             `json:"context_snapshot_id"`
	StateFiles          []LineageStateFile `json:"state_files"`
}

// LineageDispatch is one worker dispatch of the unit.
type LineageDispatch struct {
	ID            string         `json:"id"`
	RequestID     string         `json:"request_id"`
	Attempt       int            `json:"attempt"`
	Status        string         `json:"status"`
	RoleKey       string         `json:"role_key"`
	StateKey      string         `json:"state_key"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	Prompt        LineagePrompt  `json:"prompt"`
	Result        *LineageResult `json:"result,omitempty"`
}

// LineageResult is a dispatch's parsed worker result.
type LineageResult struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	RawResponseHash string `json:"raw_response_hash"`
	ParsedHash      string `json:"parsed_hash"`
}

// LineageValidation is one validation run of the unit.
type LineageValidation struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	Attempt       int        `json:"attempt"`
	CorrelationID string     `json:"correlation_id,omitempty"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Commands      int        `json:"commands"`
}

// LineageCheckpoint is a repository checkpoint of the unit.
type LineageCheckpoint struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CommitSHA string `json:"commit_sha,omitempty"`
}

// LineageEvent is a transition event naming the unit.
type LineageEvent struct {
	Seq           int64     `json:"seq"`
	Type          string    `json:"type"`
	CorrelationID string    `json:"correlation_id,omitempty"`
	At            time.Time `json:"at"`
}

// UnitLineage is the end-to-end lineage of one work unit.
type UnitLineage struct {
	ProjectID   string `json:"project_id"`
	WorkUnitID  string `json:"work_unit_id"`
	UnitKey     string `json:"unit_key"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	PhaseID     string `json:"phase_id"`
	PlanID      string `json:"plan_id"`
	PlanVersion int    `json:"plan_version"`
	PlanStatus  string `json:"plan_status"`
	WBSVersion  int    `json:"wbs_version"`
	WBSHash     string `json:"wbs_hash"`
	// ObjectiveHash identifies the task text without repeating it.
	ObjectiveHash  string              `json:"objective_hash"`
	Dispatches     []LineageDispatch   `json:"dispatches"`
	Validations    []LineageValidation `json:"validation_runs"`
	Checkpoints    []LineageCheckpoint `json:"checkpoints"`
	Events         []LineageEvent      `json:"events"`
	CorrelationIDs []string            `json:"correlation_ids"`
	// Complete: a DONE unit whose every link is present (a dispatch with
	// prompt chain and result, a passing validation, a created checkpoint
	// or a checkpoint policy that skipped it).
	Complete bool `json:"complete"`
}

// SystemDiagnostics are the durable facts of the system diagnostic
// bundle (P18-73).
type SystemDiagnostics struct {
	ExecutionCount     int              `json:"execution_count"`
	Executions         []map[string]any `json:"executions"`
	RecoveryConditions []map[string]any `json:"recovery_conditions"`
}

// ProjectDiagnostics are one project's durable diagnostic facts (ids,
// statuses, hashes; bounded lists).
type ProjectDiagnostics struct {
	ProjectID   string           `json:"project_id"`
	Name        string           `json:"name"`
	Status      string           `json:"status"`
	Executions  []map[string]any `json:"executions"`
	Phases      []map[string]any `json:"phases"`
	Plans       []map[string]any `json:"plans"`
	Units       []map[string]any `json:"units"`
	Dispatches  []map[string]any `json:"dispatches"`
	Validations []map[string]any `json:"validation_runs"`
	Checkpoints []map[string]any `json:"checkpoints"`
	EventTail   []map[string]any `json:"event_tail"`
}
