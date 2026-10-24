package mcp

import (
	"context"
	"encoding/json"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"overseer/internal/service"
)

// Tool inputs. The schema of each is inferred from its struct (required
// unless omitempty); malformed arguments are refused before the handler.

type tokenArg struct {
	Token string `json:"token" jsonschema:"the capability token from the overseer command"`
}

type readArgs struct {
	Token  string   `json:"token" jsonschema:"the capability token from the overseer command"`
	Fields []string `json:"fields,omitempty" jsonschema:"top-level fields to return (all when omitted)"`
}

type taskArgs struct {
	Token  string   `json:"token" jsonschema:"the capability token from the overseer command"`
	Key    string   `json:"key,omitempty" jsonschema:"a task key of the plan (the command's task when omitted)"`
	Fields []string `json:"fields,omitempty" jsonschema:"top-level fields to return (all when omitted)"`
}

type roleArgs struct {
	Token string `json:"token" jsonschema:"the capability token from the overseer command"`
	Role  string `json:"role,omitempty" jsonschema:"a role key (the command's role when omitted)"`
}

type progressArgs struct {
	Token          string `json:"token" jsonschema:"the capability token from the overseer command"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"unique per request; resend the same key only to repeat this request"`
	Message        string `json:"message" jsonschema:"what was done since the last report"`
}

type blockerArgs struct {
	Token          string `json:"token" jsonschema:"the capability token from the overseer command"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"unique per request; resend the same key only to repeat this request"`
	Reason         string `json:"reason" jsonschema:"what blocks the work"`
	Needs          string `json:"needs,omitempty" jsonschema:"what would unblock it"`
}

type resultArgs struct {
	Token          string         `json:"token" jsonschema:"the capability token from the overseer command"`
	IdempotencyKey string         `json:"idempotency_key" jsonschema:"unique per request; resend the same key only to repeat this request"`
	Status         string         `json:"status" jsonschema:"COMPLETE, INCOMPLETE, BLOCKED or FAILED"`
	Summary        string         `json:"summary" jsonschema:"what was done and the outcome"`
	CriteriaMet    []bool         `json:"criteria_met,omitempty" jsonschema:"test results: one true/false per acceptance check of the task, in order"`
	Evidence       map[string]any `json:"evidence,omitempty" jsonschema:"supporting evidence such as test output, files changed or commands run"`
	Plan           any            `json:"plan,omitempty" jsonschema:"plan results: {summary, units: [{key, name, kind, objective, acceptance_checks, validation, depends_on}]}"`
}

type transitionArgs struct {
	Token          string `json:"token" jsonschema:"the capability token from the overseer command"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"unique per request; resend the same key only to repeat this request"`
	Outcome        string `json:"outcome" jsonschema:"the outcome requested of the command's check state"`
	Reason         string `json:"reason,omitempty" jsonschema:"why"`
}

type reasonArgs struct {
	Token          string `json:"token" jsonschema:"the capability token from the overseer command"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"unique per request; resend the same key only to repeat this request"`
	Reason         string `json:"reason,omitempty" jsonschema:"why"`
}

// Tool names.
const (
	ToolGetCurrentState     = "get_current_state"
	ToolGetTask             = "get_task"
	ToolGetContext          = "get_context"
	ToolGetRole             = "get_role"
	ToolGetExecutionHistory = "get_execution_history"
	ToolReportProgress      = "report_progress"
	ToolReportBlocker       = "report_blocker"
	ToolSubmitResult        = "submit_result"
	ToolRequestTransition   = "request_transition"
	ToolRequestRetry        = "request_retry"
	ToolRequestContextReset = "request_context_reset"
)

// ToolNames lists the registered tools.
func ToolNames() []string {
	return []string{ToolGetCurrentState, ToolGetTask, ToolGetContext, ToolGetRole, ToolGetExecutionHistory, ToolReportProgress,
		ToolReportBlocker, ToolSubmitResult, ToolRequestTransition, ToolRequestRetry, ToolRequestContextReset}
}

func registerTools(ms *sdk.Server, s *Server) {
	readOnly := &sdk.ToolAnnotations{ReadOnlyHint: true}
	add(ms, s, &sdk.Tool{Name: ToolGetCurrentState, Description: "The workflow position: execution status, state, the command's role, phase and task.", Annotations: readOnly},
		func(ctx context.Context, b *service.MCPBinding, in readArgs) (any, []string, error) {
			v, err := s.svc.CurrentState(ctx, b)
			return v, in.Fields, err
		})
	add(ms, s, &sdk.Tool{Name: ToolGetTask, Description: "A task of the plan: objective, instructions, acceptance checks, validation and dependencies.", Annotations: readOnly},
		func(ctx context.Context, b *service.MCPBinding, in taskArgs) (any, []string, error) {
			v, err := s.svc.Task(ctx, b, in.Key)
			return v, in.Fields, err
		})
	add(ms, s, &sdk.Tool{Name: ToolGetContext, Description: "The working context: the phase WBS, the plan's tasks with their status, and the active task.", Annotations: readOnly},
		func(ctx context.Context, b *service.MCPBinding, in readArgs) (any, []string, error) {
			v, err := s.svc.Context(ctx, b)
			return v, in.Fields, err
		})
	add(ms, s, &sdk.Tool{Name: ToolGetRole, Description: "A role's system prompt and instructions (the command's role by default).", Annotations: readOnly},
		func(ctx context.Context, b *service.MCPBinding, in roleArgs) (any, []string, error) {
			v, err := s.svc.Role(ctx, b, in.Role)
			return v, nil, err
		})
	add(ms, s, &sdk.Tool{Name: ToolGetExecutionHistory, Description: "The results submitted for the task so far, oldest first.", Annotations: readOnly},
		func(ctx context.Context, b *service.MCPBinding, in readArgs) (any, []string, error) {
			v, err := s.svc.History(ctx, b)
			return v, in.Fields, err
		})
	add(ms, s, &sdk.Tool{Name: ToolReportProgress, Description: "Record progress on the command."},
		func(ctx context.Context, b *service.MCPBinding, in progressArgs) (any, []string, error) {
			v, err := s.svc.ReportProgress(ctx, b, in.IdempotencyKey, in.Message)
			return v, nil, err
		})
	add(ms, s, &sdk.Tool{Name: ToolReportBlocker, Description: "Record what blocks the command; the operator is notified."},
		func(ctx context.Context, b *service.MCPBinding, in blockerArgs) (any, []string, error) {
			v, err := s.svc.ReportBlocker(ctx, b, in.IdempotencyKey, in.Reason, in.Needs)
			return v, nil, err
		})
	add(ms, s, &sdk.Tool{Name: ToolSubmitResult, Description: "Submit the command's result (required before replying when the command expects one). " +
		"TEST: criteria_met per acceptance check; PLAN: the plan; COMPLETE requires every check met."},
		func(ctx context.Context, b *service.MCPBinding, in resultArgs) (any, []string, error) {
			var plan json.RawMessage
			if in.Plan != nil {
				raw, err := json.Marshal(in.Plan)
				if err != nil {
					return nil, nil, err
				}
				plan = raw
			}
			v, err := s.svc.SubmitResult(ctx, b, in.IdempotencyKey, service.MCPResultInput{Status: in.Status, Summary: in.Summary,
				CriteriaMet: in.CriteriaMet, Evidence: in.Evidence, Plan: plan})
			return v, nil, err
		})
	add(ms, s, &sdk.Tool{Name: ToolRequestTransition, Description: "Ask for an outcome of the command's check state; the overseer decides."},
		func(ctx context.Context, b *service.MCPBinding, in transitionArgs) (any, []string, error) {
			v, err := s.svc.RequestTransition(ctx, b, in.IdempotencyKey, in.Outcome, in.Reason)
			return v, nil, err
		})
	add(ms, s, &sdk.Tool{Name: ToolRequestRetry, Description: "Ask for the command to be resent (bounded by the retry policy)."},
		func(ctx context.Context, b *service.MCPBinding, in reasonArgs) (any, []string, error) {
			v, err := s.svc.RequestRetry(ctx, b, in.IdempotencyKey, in.Reason)
			return v, nil, err
		})
	add(ms, s, &sdk.Tool{Name: ToolRequestContextReset, Description: "Ask for a fresh worker session after this command."},
		func(ctx context.Context, b *service.MCPBinding, in reasonArgs) (any, []string, error) {
			v, err := s.svc.RequestContextReset(ctx, b, in.IdempotencyKey, in.Reason)
			return v, nil, err
		})
}

// tokenOf reads the token of any tool input.
func tokenOf(in any) string {
	b, _ := json.Marshal(in)
	var t tokenArg
	_ = json.Unmarshal(b, &t)
	return t.Token
}

// add registers one tool: bounded by the request timeout, bound by its
// token, its result filtered to the requested fields and capped in size.
// Domain errors are tool errors (IsError) with actionable text; malformed
// arguments and unknown tools are protocol errors raised by the SDK.
func add[In any](ms *sdk.Server, s *Server, t *sdk.Tool, fn func(context.Context, *service.MCPBinding, In) (any, []string, error)) {
	sdk.AddTool(ms, t, func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		defer cancel()
		var body []byte
		b, err := s.svc.Bind(ctx, tokenOf(in))
		if err == nil {
			var (
				v      any
				fields []string
			)
			if v, fields, err = fn(ctx, b, in); err == nil {
				body, err = render(v, fields, s.cfg.MaxResponseBytes)
			}
		}
		outcome := "ok"
		if err != nil {
			outcome = code(err)
		}
		s.logger.InfoContext(ctx, "mcp tool call", "tool", t.Name, "outcome", outcome, "duration_ms", time.Since(start).Milliseconds())
		if err != nil {
			if outcome == "INTERNAL" {
				s.logger.ErrorContext(ctx, "mcp tool failed", "tool", t.Name, "error", err.Error())
			}
			return toolError(err), nil, nil
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(body)}}}, nil, nil
	})
}
