package access

// Action is an operation requested through the web control plane
// (WBS 17.128). READ is the only non-mutating action.
type Action string

const (
	ActionRead            Action = "READ"
	ActionProjectCreate   Action = "PROJECT_CREATE"
	ActionProjectUpdate   Action = "PROJECT_UPDATE"
	ActionProjectDelete   Action = "PROJECT_DELETE"
	ActionLifecycleStart  Action = "LIFECYCLE_START"
	ActionLifecyclePause  Action = "LIFECYCLE_PAUSE"
	ActionLifecycleResume Action = "LIFECYCLE_RESUME"
	ActionLifecycleCancel Action = "LIFECYCLE_CANCEL"
	ActionEngineStep      Action = "ENGINE_STEP"
	ActionPhaseAppend     Action = "PHASE_APPEND"
	ActionPhaseUpdate     Action = "PHASE_UPDATE"
	ActionWBSEdit         Action = "WBS_EDIT"
	ActionConfigEdit      Action = "CONFIG_EDIT"
	ActionRoleEdit        Action = "ROLE_EDIT"
	ActionWorkflowEdit    Action = "WORKFLOW_EDIT"
	ActionStateFileEdit   Action = "STATE_FILE_EDIT"
	ActionTemplateEdit    Action = "TEMPLATE_EDIT"
	ActionDriftResolve    Action = "DRIFT_RESOLVE"
	ActionRecoveryAction  Action = "RECOVERY_ACTION"
	ActionWorkerOperate   Action = "WORKER_OPERATE"
	ActionMaintenance     Action = "MAINTENANCE"
	// ActionAcknowledgeAuditFailure acknowledges an audit verification
	// failure so new work may start again (WBS 18.110, decision U9).
	ActionAcknowledgeAuditFailure Action = "ACKNOWLEDGE_AUDIT_FAILURE"
	// ActionRestoreEnable enables execution on a restored database after
	// its startup checks passed (WBS 18.46, decision U4).
	ActionRestoreEnable Action = "RESTORE_ENABLE"
	// ActionPreview is a non-mutating computation requested with POST
	// (prompt / transition / compression previews, cache explain).
	ActionPreview Action = "PREVIEW"
)

// Actions lists every action in a stable order.
func Actions() []Action {
	return []Action{
		ActionRead, ActionPreview, ActionProjectCreate, ActionProjectUpdate, ActionProjectDelete,
		ActionLifecycleStart, ActionLifecyclePause, ActionLifecycleResume, ActionLifecycleCancel,
		ActionEngineStep, ActionPhaseAppend, ActionPhaseUpdate, ActionWBSEdit, ActionConfigEdit,
		ActionRoleEdit, ActionWorkflowEdit, ActionStateFileEdit, ActionTemplateEdit,
		ActionDriftResolve, ActionRecoveryAction, ActionWorkerOperate, ActionMaintenance,
		ActionAcknowledgeAuditFailure, ActionRestoreEnable,
	}
}

// Valid reports whether a is a known action.
func (a Action) Valid() bool {
	for _, x := range Actions() {
		if x == a {
			return true
		}
	}
	return false
}

// IsMutation reports whether a changes persisted state. READ and PREVIEW
// do not.
func (a Action) IsMutation() bool {
	return a.Valid() && a != ActionRead && a != ActionPreview
}

// Resource identifies what an action targets (WBS 17.128).
type Resource struct {
	// Kind is a stable resource kind ("project", "phase", "execution", ...).
	Kind string
	// ProjectID scopes project-owned resources ("" for global ones).
	ProjectID string
	// ID is the resource id within its kind ("" for collections).
	ID string
}

// Authorizer decides whether a principal may perform an action on a
// resource (WBS 17.128).
type Authorizer interface {
	Can(p Principal, a Action, r Resource) bool
}

// LocalPolicy is the v1 policy: local administrators and the system may do
// everything; users may read; an unauthenticated (zero / invalid) principal
// may do nothing.
type LocalPolicy struct{}

// Can implements Authorizer.
func (LocalPolicy) Can(p Principal, a Action, _ Resource) bool {
	if p.Validate() != nil || !a.Valid() {
		return false
	}
	switch p.Type {
	case PrincipalLocalAdmin, PrincipalSystem:
		return true
	case PrincipalUser:
		return !a.IsMutation()
	}
	return false
}
