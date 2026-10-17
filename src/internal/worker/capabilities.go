package worker

// WorkerCapabilities advertises what a worker supports (WBS 12.7/12.8).
// The zero value of every flag means unsupported; versions are optional
// and empty when the provider does not report them.
type WorkerCapabilities struct {
	Provider        WorkerProvider
	ProviderVersion string
	ProtocolVersion string

	Sessions bool

	NewSession      bool
	ChangeDirectory bool

	ContextTelemetry bool

	Streaming    bool
	Cancellation bool

	ToolExecution bool
}

// WorkflowRequirements are the capabilities a workflow needs from its
// worker (WBS 12.96/12.97).
type WorkflowRequirements struct {
	NewSession       bool
	ChangeDirectory  bool
	ContextTelemetry bool
}

// DefaultWorkflowRequirements is what the Hermes workflow needs: fresh
// contexts and working-directory control. Context telemetry is preferred
// but estimates are acceptable (WBS 12.15).
var DefaultWorkflowRequirements = WorkflowRequirements{NewSession: true, ChangeDirectory: true}
