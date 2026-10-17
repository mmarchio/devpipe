package readmodel

import "time"

// AuditRow is one audit log row / detail (WBS 17.103, 17.105). Metadata is
// already redacted.
type AuditRow struct {
	ID           string         `json:"id"`
	Seq          int64          `json:"seq"`
	At           time.Time      `json:"at"`
	ProjectID    string         `json:"project_id,omitempty"`
	ProjectName  string         `json:"project_name,omitempty"`
	ExecutionID  string         `json:"execution_id,omitempty"`
	PhaseID      string         `json:"phase_id,omitempty"`
	WorkUnitID   string         `json:"work_unit_id,omitempty"`
	ActorID      string         `json:"actor_id"`
	ActorName    string         `json:"actor_name"`
	ActorType    string         `json:"actor_type"`
	Action       string         `json:"action"`
	ResourceKind string         `json:"resource_kind"`
	ResourceID   string         `json:"resource_id,omitempty"`
	Result       string         `json:"result"`
	HTTPStatus   int            `json:"http_status"`
	Reason       string         `json:"reason,omitempty"`
	RequestID    string         `json:"request_id"`
	Method       string         `json:"method"`
	Path         string         `json:"path"`
	Metadata     map[string]any `json:"metadata"`
}

// AuditPage is one audit page with its next cursor ("" at the end).
type AuditPage struct {
	Rows       []AuditRow `json:"events"`
	NextCursor string     `json:"next_cursor"`
}
