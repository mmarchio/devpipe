package execution

import "sort"

// ReadinessCategory groups readiness errors (WBS 15.12).
type ReadinessCategory string

// Readiness categories in report order (WBS 15.12).
const (
	CategoryProject      ReadinessCategory = "project_configuration"
	CategoryWorkingDir   ReadinessCategory = "working_directory"
	CategoryRepository   ReadinessCategory = "repository"
	CategoryPhase        ReadinessCategory = "phase_configuration"
	CategoryWBS          ReadinessCategory = "wbs"
	CategoryStateMachine ReadinessCategory = "state_machine"
	CategoryRoles        ReadinessCategory = "roles"
	CategoryRoleInstr    ReadinessCategory = "role_instructions"
	CategoryStateInstr   ReadinessCategory = "state_instructions"
	CategoryStateFiles   ReadinessCategory = "state_files"
	CategoryTemplates    ReadinessCategory = "prompt_templates"
	CategoryCompression  ReadinessCategory = "compression_provider"
	CategoryWorker       ReadinessCategory = "worker_provider"
	// CategoryWorkerSession / CategoryCompressionSession are the live
	// provider probes: the worker gateway can open a session and the
	// compression provider serves the configured model.
	CategoryWorkerSession      ReadinessCategory = "worker_session"
	CategoryCompressionSession ReadinessCategory = "compression_session"
	// CategoryMCP: the overseer MCP server the statefile workflow's team
	// reports through runs.
	CategoryMCP          ReadinessCategory = "mcp_server"
	CategoryProtocol     ReadinessCategory = "response_protocol"
	CategoryContext      ReadinessCategory = "context_configuration"
	CategoryContinuation ReadinessCategory = "continuation_configuration"
)

var categoryOrder = []ReadinessCategory{
	CategoryProject, CategoryWorkingDir, CategoryRepository, CategoryPhase, CategoryWBS,
	CategoryStateMachine, CategoryRoles, CategoryRoleInstr, CategoryStateInstr, CategoryStateFiles,
	CategoryTemplates, CategoryCompression, CategoryCompressionSession, CategoryWorker, CategoryWorkerSession,
	CategoryMCP, CategoryProtocol, CategoryContext, CategoryContinuation,
}

// Categories returns every category in report order.
func Categories() []ReadinessCategory {
	return append([]ReadinessCategory(nil), categoryOrder...)
}

func categoryRank(c ReadinessCategory) int {
	for i, v := range categoryOrder {
		if v == c {
			return i
		}
	}
	return len(categoryOrder)
}

// ReadinessError is one readiness problem (WBS 15.13).
type ReadinessError struct {
	Category ReadinessCategory  `json:"category"`
	Code     BootstrapErrorCode `json:"code"`
	Message  string             `json:"message"`
	Resource string             `json:"resource,omitempty"`
}

// ExecutionReadinessReport aggregates readiness (WBS 15.13). Warnings never
// block readiness.
type ExecutionReadinessReport struct {
	Ready    bool             `json:"ready"`
	Errors   []ReadinessError `json:"errors"`
	Warnings []ReadinessError `json:"warnings"`

	// CompletionEligible marks a project whose phases are all DONE
	// (WBS 15.23): no execution is bootstrapped.
	CompletionEligible bool `json:"completion_eligible"`
}

// Add records an error.
func (r *ExecutionReadinessReport) Add(cat ReadinessCategory, code BootstrapErrorCode, resource, msg string) {
	r.Errors = append(r.Errors, ReadinessError{Category: cat, Code: code, Message: msg, Resource: resource})
}

// Warn records a non-blocking warning.
func (r *ExecutionReadinessReport) Warn(cat ReadinessCategory, code BootstrapErrorCode, resource, msg string) {
	r.Warnings = append(r.Warnings, ReadinessError{Category: cat, Code: code, Message: msg, Resource: resource})
}

// Merge appends another report's problems.
func (r *ExecutionReadinessReport) Merge(o *ExecutionReadinessReport) {
	if o == nil {
		return
	}
	r.Errors = append(r.Errors, o.Errors...)
	r.Warnings = append(r.Warnings, o.Warnings...)
	r.CompletionEligible = r.CompletionEligible || o.CompletionEligible
}

// Finalize sorts deterministically (category order, code, resource,
// message), removes exact duplicates and sets Ready.
func (r *ExecutionReadinessReport) Finalize() *ExecutionReadinessReport {
	r.Errors = sortDedup(r.Errors)
	r.Warnings = sortDedup(r.Warnings)
	if r.Errors == nil {
		r.Errors = []ReadinessError{}
	}
	if r.Warnings == nil {
		r.Warnings = []ReadinessError{}
	}
	r.Ready = len(r.Errors) == 0 && !r.CompletionEligible
	return r
}

// FirstCode returns the code of the first error ("" when none).
func (r *ExecutionReadinessReport) FirstCode() BootstrapErrorCode {
	if len(r.Errors) == 0 {
		if r.CompletionEligible {
			return CodeNoPhase
		}
		return ""
	}
	return r.Errors[0].Code
}

func sortDedup(in []ReadinessError) []ReadinessError {
	sort.SliceStable(in, func(i, j int) bool {
		a, b := in[i], in[j]
		if ra, rb := categoryRank(a.Category), categoryRank(b.Category); ra != rb {
			return ra < rb
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Resource != b.Resource {
			return a.Resource < b.Resource
		}
		return a.Message < b.Message
	})
	var out []ReadinessError
	for i, e := range in {
		if i > 0 && e == in[i-1] {
			continue
		}
		out = append(out, e)
	}
	return out
}
