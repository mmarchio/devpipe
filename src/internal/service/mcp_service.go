package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// ---------------------------------------------------------------------------
// Overseer MCP service (Phase 19). Hermes reads its working context and
// submits structured results through the overseer MCP server; this service
// is the domain behind the tools. Every call is bound by the capability
// token the engine wrote into the command (never by an id in the
// arguments) to that command's in-flight dispatch. Tools record intent and
// evidence only: the engine's check states decide every transition.

var (
	// ErrMCPUnauthorized: the call carries no valid token, or the token's
	// dispatch is not in progress.
	ErrMCPUnauthorized = errors.New("service: MCP call not authorized")
	// ErrMCPRejected: the request is well-formed but the workflow rules
	// refuse it (the message says how to fix it).
	ErrMCPRejected = errors.New("service: MCP request rejected")
)

// MCPRejection is a refused request with actionable feedback.
type MCPRejection struct {
	Code    string
	Message string
}

func (r *MCPRejection) Error() string { return r.Code + ": " + r.Message }

// Is makes a rejection match ErrMCPRejected.
func (r *MCPRejection) Is(target error) bool { return target == ErrMCPRejected }

func reject(code, format string, args ...any) error {
	return &MCPRejection{Code: code, Message: fmt.Sprintf(format, args...)}
}

// mcpUnauthorized wraps ErrMCPUnauthorized with the reason.
func mcpUnauthorized(reason string) error { return fmt.Errorf("%w: %s", ErrMCPUnauthorized, reason) }

// DefaultMCPTokenTTL bounds a token's life; the in-flight dispatch check
// cuts it off as soon as the dispatch finishes.
const DefaultMCPTokenTTL = 24 * time.Hour

// mcpTokenPrefix marks overseer tokens in a command.
const mcpTokenPrefix = "ovt_"

// MCPService serves the MCP tools.
type MCPService struct {
	e    *Engine
	repo persistence.MCPRepository
	ttl  time.Duration
}

// NewMCPService wires the service (ttl 0 = DefaultMCPTokenTTL).
func NewMCPService(e *Engine, repo persistence.MCPRepository, ttl time.Duration) *MCPService {
	if ttl <= 0 {
		ttl = DefaultMCPTokenTTL
	}
	return &MCPService{e: e, repo: repo, ttl: ttl}
}

// HashMCPToken is the stored form of a token.
func HashMCPToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newMCPToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return mcpTokenPrefix + hex.EncodeToString(b), nil
}

// storeToken stores the token of a command prompt (only its hash).
func (s *MCPService) storeToken(ctx context.Context, ex persistence.Executor, st *engineState, promptID uuid.UUID, roleKey, stateKey, token string) error {
	now := s.e.d.Now().UTC()
	return s.repo.CreateToken(ctx, ex, &persistence.MCPTokenRecord{TokenHash: HashMCPToken(token), WorkerPromptID: promptID, ProjectID: st.rec.ProjectID,
		ExecutionID: st.rec.ID, WorkUnitID: unitIDOf(st), RoleKey: roleKey, StateKey: stateKey, CreatedAt: now, ExpiresAt: now.Add(s.ttl)})
}

// MCPBinding is the dispatch a call acts for, resolved from its token.
type MCPBinding struct {
	ProjectID      uuid.UUID
	ExecutionID    uuid.UUID
	DispatchID     uuid.UUID
	WorkerPromptID uuid.UUID
	WorkUnitID     *uuid.UUID
	RoleKey        string
	StateKey       string
}

// Bind resolves a token: unknown, revoked or expired tokens, and tokens
// whose dispatch is not in flight, are unauthorized.
func (s *MCPService) Bind(ctx context.Context, token string) (*MCPBinding, error) {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, mcpTokenPrefix) {
		return nil, mcpUnauthorized("the token is missing or malformed; pass the token from the overseer command")
	}
	rec, err := s.repo.GetToken(ctx, nil, HashMCPToken(token))
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, mcpUnauthorized("unknown token")
	} else if err != nil {
		return nil, translate(err)
	}
	now := s.e.d.Now()
	switch {
	case rec.RevokedAt != nil:
		return nil, mcpUnauthorized("the token was revoked")
	case !now.Before(rec.ExpiresAt):
		return nil, mcpUnauthorized("the token expired")
	}
	d, err := s.repo.InFlightDispatch(ctx, nil, rec.WorkerPromptID)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, mcpUnauthorized("the command this token was issued for is no longer in progress")
	} else if err != nil {
		return nil, translate(err)
	}
	return &MCPBinding{ProjectID: rec.ProjectID, ExecutionID: rec.ExecutionID, DispatchID: d, WorkerPromptID: rec.WorkerPromptID,
		WorkUnitID: rec.WorkUnitID, RoleKey: rec.RoleKey, StateKey: rec.StateKey}, nil
}

// reportKinds maps a command state to the kind of result it expects
// (states without one, such as SKILL, need no result).
var reportKinds = map[string]string{
	"PLAN": persistence.ReportPlan, "REPLAN": persistence.ReportPlan,
	"ACCEPTANCE": persistence.ReportAcceptance,
	"IMPLEMENT":  persistence.ReportImplement, "CONTINUE": persistence.ReportImplement,
	// A fix may rewrite the task definition: its result carries the task
	// as a one-task plan (optional).
	"FIX_TASK":     persistence.ReportPlan,
	"TEST":         persistence.ReportTest,
	"ANALYZE_TASK": persistence.ReportAnalysis,
}

// ReportKind is the result kind a command state expects ("" for none).
func ReportKind(stateKey string) string { return reportKinds[stateKey] }

// ---------------------------------------------------------------------------
// Reads (Phase 19.6). Every view is built from the database; the returned
// values are copies, so changing them changes nothing stored.

// MCPPhase identifies a phase.
type MCPPhase struct {
	Order int    `json:"order"`
	Name  string `json:"name"`
}

// MCPState is get_current_state.
type MCPState struct {
	ExecutionID     string    `json:"execution_id"`
	ExecutionStatus string    `json:"execution_status"`
	State           string    `json:"state"`
	CommandState    string    `json:"command_state"`
	Role            string    `json:"role"`
	ResultKind      string    `json:"result_kind,omitempty"`
	Phase           *MCPPhase `json:"phase,omitempty"`
	Task            string    `json:"task,omitempty"`
	TaskStatus      string    `json:"task_status,omitempty"`
}

// MCPCheck is one acceptance check of a task.
type MCPCheck struct {
	Index        int      `json:"index"`
	Text         string   `json:"text"`
	Verification string   `json:"verification"`
	Command      string   `json:"command,omitempty"`
	Tests        []string `json:"tests,omitempty"`
}

// MCPTask is get_task.
type MCPTask struct {
	Key              string     `json:"key"`
	Name             string     `json:"name"`
	Kind             string     `json:"kind"`
	Status           string     `json:"status"`
	Archived         bool       `json:"archived"`
	Objective        string     `json:"objective"`
	Instructions     string     `json:"instructions,omitempty"`
	AcceptanceChecks []MCPCheck `json:"acceptance_checks"`
	Validation       []string   `json:"validation,omitempty"`
	DependsOn        []string   `json:"depends_on,omitempty"`
	Paths            []string   `json:"paths,omitempty"`
}

// MCPPlanTask is one task of the plan in get_context.
type MCPPlanTask struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Archived  bool     `json:"archived"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// MCPContext is get_context: what the team used to read from the
// statefiles (the phase WBS, the backlog, the active task).
type MCPContext struct {
	Phase      *MCPPhase     `json:"phase,omitempty"`
	WBS        string        `json:"wbs,omitempty"`
	Plan       []MCPPlanTask `json:"plan"`
	ActiveTask string        `json:"active_task,omitempty"`
}

// MCPRole is get_role.
type MCPRole struct {
	Key          string `json:"key"`
	Instructions string `json:"instructions"`
}

// MCPReport is one submitted result in get_execution_history.
type MCPReport struct {
	Kind        string         `json:"kind"`
	Status      string         `json:"status"`
	Role        string         `json:"role"`
	State       string         `json:"state"`
	Summary     string         `json:"summary"`
	CriteriaMet []bool         `json:"criteria_met,omitempty"`
	Evidence    map[string]any `json:"evidence,omitempty"`
	CreatedAt   string         `json:"created_at"`
}

// MCPHistory is get_execution_history.
type MCPHistory struct {
	Task    string      `json:"task,omitempty"`
	Reports []MCPReport `json:"reports"`
}

func (s *MCPService) load(ctx context.Context, b *MCPBinding) (*engineState, error) {
	st, err := s.e.load(ctx, b.ExecutionID)
	if err != nil {
		return nil, err
	}
	if st.rec.ProjectID != b.ProjectID {
		return nil, mcpUnauthorized("the token does not belong to this project")
	}
	return st, nil
}

func phaseOf(st *engineState) *MCPPhase {
	if p := st.workPhase(); p != nil {
		return &MCPPhase{Order: p.Order, Name: p.Name}
	}
	return nil
}

// boundUnit is the token's work unit within the loaded plan.
func boundUnit(st *engineState, b *MCPBinding) *wbs.WorkUnit {
	if b.WorkUnitID == nil {
		return st.unit
	}
	for _, u := range st.units {
		if u.ID == *b.WorkUnitID {
			return u
		}
	}
	return nil
}

// CurrentState is get_current_state.
func (s *MCPService) CurrentState(ctx context.Context, b *MCPBinding) (*MCPState, error) {
	st, err := s.load(ctx, b)
	if err != nil {
		return nil, err
	}
	out := &MCPState{ExecutionID: st.rec.ID.String(), ExecutionStatus: string(st.rec.Status), State: st.cur.Key, CommandState: b.StateKey,
		Role: b.RoleKey, ResultKind: ReportKind(b.StateKey), Phase: phaseOf(st)}
	if u := boundUnit(st, b); u != nil {
		out.Task, out.TaskStatus = wbs.UnitKey(u), string(u.Status)
	}
	return out, nil
}

// Task is get_task: the bound task, or the plan's task with the key.
func (s *MCPService) Task(ctx context.Context, b *MCPBinding, key string) (*MCPTask, error) {
	st, err := s.load(ctx, b)
	if err != nil {
		return nil, err
	}
	var u *wbs.WorkUnit
	if key = strings.TrimSpace(key); key == "" {
		if u = boundUnit(st, b); u == nil {
			return nil, reject("NO_ACTIVE_TASK", "this command has no task; pass the key of a task of the plan")
		}
	} else {
		for _, x := range st.units {
			if wbs.UnitKey(x) == key {
				u = x
			}
		}
		if u == nil {
			return nil, fmt.Errorf("%w: task %s is not in the current plan", ErrNotFound, key)
		}
	}
	archived, deps, err := s.planFacts(ctx, st)
	if err != nil {
		return nil, err
	}
	t := &MCPTask{Key: wbs.UnitKey(u), Name: u.Name, Kind: string(u.Kind), Status: string(u.Status), Archived: archived[u.ID],
		DependsOn: deps[u.ID], AcceptanceChecks: []MCPCheck{}}
	if sp := u.Specification; sp != nil {
		t.Objective, t.Instructions, t.Paths = sp.Objective, sp.ImplementationInstructions, append([]string(nil), sp.Paths...)
		for i, c := range sp.AcceptanceChecks {
			mc := MCPCheck{Index: i, Text: c.Text, Verification: c.Verification, Tests: append([]string(nil), c.Tests...)}
			if c.Command != nil {
				mc.Command = commandLine(*c.Command)
			}
			t.AcceptanceChecks = append(t.AcceptanceChecks, mc)
		}
		for _, v := range sp.Validation {
			t.Validation = append(t.Validation, commandLine(v))
		}
	}
	return t, nil
}

func commandLine(v wbs.ValidationSpec) string {
	return strings.TrimSpace(v.Program + " " + strings.Join(v.Args, " "))
}

// planFacts returns the archived units and each unit's dependency keys.
func (s *MCPService) planFacts(ctx context.Context, st *engineState) (map[uuid.UUID]bool, map[uuid.UUID][]string, error) {
	if st.plan == nil {
		return map[uuid.UUID]bool{}, map[uuid.UUID][]string{}, nil
	}
	archived, err := s.repo.ArchivedUnits(ctx, nil, st.plan.ID)
	if err != nil {
		return nil, nil, translate(err)
	}
	ids, err := s.e.d.Repo.UnitDependencies(ctx, nil, st.plan.ID)
	if err != nil {
		return nil, nil, translate(err)
	}
	keys := map[uuid.UUID]string{}
	for _, u := range st.units {
		keys[u.ID] = wbs.UnitKey(u)
	}
	deps := map[uuid.UUID][]string{}
	for id, ds := range ids {
		for _, d := range ds {
			deps[id] = append(deps[id], keys[d])
		}
		sort.Strings(deps[id])
	}
	return archived, deps, nil
}

// Context is get_context.
func (s *MCPService) Context(ctx context.Context, b *MCPBinding) (*MCPContext, error) {
	st, err := s.load(ctx, b)
	if err != nil {
		return nil, err
	}
	out := &MCPContext{Phase: phaseOf(st), Plan: []MCPPlanTask{}}
	if p := st.workPhase(); p != nil {
		if v, err := s.e.d.WBSVersions.GetCurrentVersion(ctx, p.ID.String()); err == nil && v > 0 {
			rec, err := s.e.d.WBSVersions.GetVersion(ctx, p.ID.String(), v)
			if err != nil {
				return nil, translate(err)
			}
			out.WBS = rec.Content
		}
	}
	archived, deps, err := s.planFacts(ctx, st)
	if err != nil {
		return nil, err
	}
	for _, u := range st.units {
		out.Plan = append(out.Plan, MCPPlanTask{Key: wbs.UnitKey(u), Name: u.Name, Status: string(u.Status), Archived: archived[u.ID], DependsOn: deps[u.ID]})
	}
	if st.unit != nil {
		out.ActiveTask = wbs.UnitKey(st.unit)
	}
	return out, nil
}

// Role is get_role: the role's system prompt and instructions as captured
// in the execution's configuration snapshot (the bound role by default).
func (s *MCPService) Role(ctx context.Context, b *MCPBinding, key string) (*MCPRole, error) {
	st, err := s.load(ctx, b)
	if err != nil {
		return nil, err
	}
	if key = strings.TrimSpace(key); key == "" {
		key = b.RoleKey
	}
	for _, r := range st.content.Roles {
		if r.RoleKey == key {
			return &MCPRole{Key: key, Instructions: renderRoleFile(r)}, nil
		}
	}
	return nil, fmt.Errorf("%w: role %s is not in this execution", ErrNotFound, key)
}

// History is get_execution_history: the bound task's submitted results,
// oldest first (every result of the execution when there is no task).
func (s *MCPService) History(ctx context.Context, b *MCPBinding) (*MCPHistory, error) {
	st, err := s.load(ctx, b)
	if err != nil {
		return nil, err
	}
	out := &MCPHistory{Reports: []MCPReport{}}
	var unitID *uuid.UUID
	if u := boundUnit(st, b); u != nil {
		out.Task, unitID = wbs.UnitKey(u), &u.ID
	}
	recs, err := s.repo.ListTaskReports(ctx, nil, b.ExecutionID, unitID)
	if err != nil {
		return nil, translate(err)
	}
	for _, r := range recs {
		out.Reports = append(out.Reports, MCPReport{Kind: r.Kind, Status: r.Status, Role: r.RoleKey, State: r.StateKey, Summary: r.Summary,
			CriteriaMet: r.CriteriaMet, Evidence: r.Evidence, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano)})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mutations (Phase 19.7). Each is keyed by the caller's idempotency key: a
// repeat returns the stored result, the same key with another payload is a
// conflict. The request, its evidence, its event and any follow-up action
// commit in one transaction.

// MCPAck is the result of a mutation.
type MCPAck struct {
	Accepted bool   `json:"accepted"`
	Message  string `json:"message"`
	Replayed bool   `json:"replayed,omitempty"`
}

func payloadHash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// mutate runs one idempotent mutation.
func (s *MCPService) mutate(ctx context.Context, b *MCPBinding, tool, key string, payload any,
	fn func(tx persistence.Transaction) (*MCPAck, error)) (*MCPAck, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 200 {
		return nil, reject("IDEMPOTENCY_KEY", "idempotency_key is required (1-200 characters); reuse it only to repeat the same request")
	}
	hash, err := payloadHash(payload)
	if err != nil {
		return nil, err
	}
	replay := func(ex persistence.Executor) (*MCPAck, error) {
		prev, err := s.repo.GetRequest(ctx, ex, b.DispatchID, tool, key)
		if err != nil {
			return nil, err
		}
		if prev.PayloadHash != hash {
			return nil, fmt.Errorf("%w: idempotency_key %q was already used with a different request", ErrConflict, key)
		}
		var ack MCPAck
		if err := json.Unmarshal(prev.Result, &ack); err != nil {
			return nil, err
		}
		ack.Replayed = true
		return &ack, nil
	}
	var out *MCPAck
	err = s.e.inTx(ctx, nil, func(tx persistence.Transaction) error {
		out = nil
		if ack, err := replay(tx); err == nil {
			out = ack
			return nil
		} else if !errors.Is(err, persistence.ErrNotFound) {
			return err
		}
		ack, err := fn(tx)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(ack)
		if err != nil {
			return err
		}
		if err := s.repo.CreateRequest(ctx, tx, &persistence.MCPRequestRecord{ID: uuid.New(), DispatchID: b.DispatchID, Tool: tool,
			IdempotencyKey: key, PayloadHash: hash, Result: raw, CreatedAt: s.e.d.Now().UTC()}); err != nil {
			return err
		}
		out = ack
		return nil
	})
	if errors.Is(err, persistence.ErrConflict) {
		// A concurrent request with the key committed first.
		if ack, rerr := replay(nil); rerr == nil {
			return ack, nil
		} else if !errors.Is(rerr, persistence.ErrNotFound) {
			return nil, translateMCP(rerr)
		}
	}
	if err != nil {
		return nil, translateMCP(err)
	}
	return out, nil
}

func translateMCP(err error) error {
	if errors.Is(err, ErrMCPRejected) || errors.Is(err, ErrMCPUnauthorized) || errors.Is(err, ErrConflict) {
		return err
	}
	return translate(err)
}

func (s *MCPService) event(ctx context.Context, tx persistence.Executor, b *MCPBinding, typ execution.EventType, payload map[string]any) error {
	ex := b.ExecutionID
	payload["dispatch_id"] = b.DispatchID.String()
	payload["role"] = b.RoleKey
	payload["state"] = b.StateKey
	if b.WorkUnitID != nil {
		payload["work_unit_id"] = b.WorkUnitID.String()
	}
	return s.e.d.Execs.AppendEvent(ctx, tx, &execution.Event{ID: uuid.New(), ProjectID: b.ProjectID, ExecutionID: &ex, Type: typ,
		Payload: payload, CreatedAt: s.e.d.Now().UTC()})
}

func (s *MCPService) outbox(ctx context.Context, tx persistence.Executor, b *MCPBinding, kind, dedupe string, payload map[string]any) error {
	ex := b.ExecutionID
	return s.repo.EnqueueOutbox(ctx, tx, &persistence.OutboxRecord{ID: uuid.New(), ProjectID: b.ProjectID, ExecutionID: &ex, Kind: kind,
		Payload: payload, DedupeKey: dedupe, CreatedAt: s.e.d.Now().UTC()})
}

// ReportProgress is report_progress.
func (s *MCPService) ReportProgress(ctx context.Context, b *MCPBinding, key, message string) (*MCPAck, error) {
	if strings.TrimSpace(message) == "" {
		return nil, reject("MISSING_MESSAGE", "message is required: say what was done since the last report")
	}
	return s.mutate(ctx, b, "report_progress", key, map[string]any{"message": message}, func(tx persistence.Transaction) (*MCPAck, error) {
		if err := s.event(ctx, tx, b, execution.EventMCPProgress, map[string]any{"message": message}); err != nil {
			return nil, err
		}
		return &MCPAck{Accepted: true, Message: "progress recorded"}, nil
	})
}

// ReportBlocker is report_blocker: recorded, and the operator is notified
// through the outbox.
func (s *MCPService) ReportBlocker(ctx context.Context, b *MCPBinding, key, reason, needs string) (*MCPAck, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, reject("MISSING_REASON", "reason is required: say what blocks the work")
	}
	p := map[string]any{"reason": reason, "needs": needs}
	return s.mutate(ctx, b, "report_blocker", key, p, func(tx persistence.Transaction) (*MCPAck, error) {
		if err := s.event(ctx, tx, b, execution.EventMCPBlocker, map[string]any{"reason": reason, "needs": needs}); err != nil {
			return nil, err
		}
		if err := s.outbox(ctx, tx, b, persistence.OutboxBlockerNotify, "blocker:"+b.DispatchID.String()+":"+key,
			map[string]any{"reason": reason, "needs": needs, "dispatch_id": b.DispatchID.String()}); err != nil {
			return nil, err
		}
		return &MCPAck{Accepted: true, Message: "blocker recorded; the operator is notified. Submit the result with status BLOCKED when you stop."}, nil
	})
}

// MCPResultInput is submit_result.
type MCPResultInput struct {
	Status      string
	Summary     string
	CriteriaMet []bool
	Evidence    map[string]any
	// Plan is the plan of a PLAN result (the plan candidate JSON).
	Plan json.RawMessage
}

// SubmitResult is submit_result: validated against the command's result
// kind and the task, then stored for the command's check state.
func (s *MCPService) SubmitResult(ctx context.Context, b *MCPBinding, key string, in MCPResultInput) (*MCPAck, error) {
	kind := ReportKind(b.StateKey)
	if kind == "" {
		return nil, reject("NO_RESULT_EXPECTED", "the %s command takes no result; reply when it is done", b.StateKey)
	}
	status := strings.ToUpper(strings.TrimSpace(in.Status))
	switch status {
	case persistence.ReportComplete, persistence.ReportIncomplete, persistence.ReportBlocked, persistence.ReportFailed:
	default:
		return nil, reject("INVALID_STATUS", "status must be COMPLETE, INCOMPLETE, BLOCKED or FAILED")
	}
	if strings.TrimSpace(in.Summary) == "" {
		return nil, reject("MISSING_SUMMARY", "summary is required")
	}
	st, err := s.load(ctx, b)
	if err != nil {
		return nil, err
	}
	u := boundUnit(st, b)
	ev := map[string]any{}
	for k, v := range in.Evidence {
		ev[k] = v
	}
	switch kind {
	case persistence.ReportTest:
		if u == nil {
			return nil, reject("NO_ACTIVE_TASK", "this test command has no task")
		}
		var checks int
		if u.Specification != nil {
			checks = len(u.Specification.AcceptanceChecks)
		}
		if len(in.CriteriaMet) != checks {
			return nil, reject("CRITERIA_COUNT", "criteria_met has %d entries; task %s has %d acceptance checks (call get_task) — give one true/false per check, in order",
				len(in.CriteriaMet), wbs.UnitKey(u), checks)
		}
		if status == persistence.ReportComplete {
			for i, m := range in.CriteriaMet {
				if !m {
					return nil, reject("CRITERIA_UNMET", "status COMPLETE requires every acceptance check met; check %d is not — submit INCOMPLETE with what fails", i)
				}
			}
		}
	case persistence.ReportPlan:
		if status == persistence.ReportComplete && (len(in.Plan) > 0 || b.StateKey != "FIX_TASK") {
			if len(in.Plan) == 0 {
				return nil, reject("MISSING_PLAN", "a complete plan result needs plan: {summary, units: [{key, name, kind, objective, acceptance_checks, validation, depends_on}]}")
			}
			if _, err := wbs.DecodePlanCandidate(in.Plan); err != nil {
				return nil, reject("MALFORMED_PLAN", "plan: %s", err.Error())
			}
			var p any
			if err := json.Unmarshal(in.Plan, &p); err != nil {
				return nil, reject("MALFORMED_PLAN", "plan: %s", err.Error())
			}
			ev["plan"] = p
		}
	}
	met := append([]bool(nil), in.CriteriaMet...)
	payload := map[string]any{"status": status, "summary": in.Summary, "criteria_met": met, "evidence": ev}
	return s.mutate(ctx, b, "submit_result", key, payload, func(tx persistence.Transaction) (*MCPAck, error) {
		rec := &persistence.TaskReportRecord{ID: uuid.New(), ProjectID: b.ProjectID, ExecutionID: b.ExecutionID, DispatchID: b.DispatchID,
			WorkUnitID: b.WorkUnitID, RoleKey: b.RoleKey, StateKey: b.StateKey, Kind: kind, Status: status, CriteriaMet: met, Summary: in.Summary,
			Evidence: ev, CreatedAt: s.e.d.Now().UTC()}
		if err := s.repo.CreateTaskReport(ctx, tx, rec); errors.Is(err, persistence.ErrConflict) {
			return nil, reject("ALREADY_SUBMITTED", "a %s result was already submitted for this command; it cannot be replaced", kind)
		} else if err != nil {
			return nil, err
		}
		if err := s.event(ctx, tx, b, execution.EventMCPResult, map[string]any{"kind": kind, "status": status, "report_id": rec.ID.String()}); err != nil {
			return nil, err
		}
		return &MCPAck{Accepted: true, Message: kind + " result recorded; the overseer decides the next step when you reply"}, nil
	})
}

// checkStateOf is the control state a command state submits to.
func checkStateOf(st *engineState, stateKey string) string {
	for _, t := range st.machine.Transitions {
		if t.Enabled && t.From == stateKey && t.Outcome == "SUBMIT" {
			return t.To
		}
	}
	return ""
}

// RequestTransition is request_transition: the requested outcome must be
// one the command's check state can take; the check state decides.
func (s *MCPService) RequestTransition(ctx context.Context, b *MCPBinding, key, outcome, reason string) (*MCPAck, error) {
	st, err := s.load(ctx, b)
	if err != nil {
		return nil, err
	}
	outcome = strings.ToUpper(strings.TrimSpace(outcome))
	check := checkStateOf(st, b.StateKey)
	var legal []string
	for _, t := range st.machine.Transitions {
		if t.Enabled && t.From == check {
			legal = append(legal, t.Outcome)
		}
	}
	sort.Strings(legal)
	ok := false
	for _, o := range legal {
		ok = ok || o == outcome
	}
	if !ok {
		return nil, reject("ILLEGAL_TRANSITION", "%q is not an outcome of %s; legal outcomes: %s", outcome, b.StateKey, strings.Join(legal, ", "))
	}
	p := map[string]any{"outcome": outcome, "reason": reason, "check_state": check}
	return s.mutate(ctx, b, "request_transition", key, p, func(tx persistence.Transaction) (*MCPAck, error) {
		if err := s.event(ctx, tx, b, execution.EventMCPTransitionRequested, map[string]any{"outcome": outcome, "reason": reason, "check_state": check}); err != nil {
			return nil, err
		}
		return &MCPAck{Accepted: true, Message: "transition request recorded; " + check + " decides from the submitted result"}, nil
	})
}

// RequestRetry is request_retry: refused once the command state's resend
// limit is reached.
func (s *MCPService) RequestRetry(ctx context.Context, b *MCPBinding, key, reason string) (*MCPAck, error) {
	st, err := s.load(ctx, b)
	if err != nil {
		return nil, err
	}
	check := checkStateOf(st, b.StateKey)
	n, err := s.e.consecutiveStatefileRetries(ctx, st, check)
	if err != nil {
		return nil, translate(err)
	}
	if n >= maxCommandRetries {
		return nil, reject("RETRY_LIMIT", "the %s command was already resent %d times; submit the result (BLOCKED or FAILED) instead", b.StateKey, n)
	}
	p := map[string]any{"reason": reason}
	return s.mutate(ctx, b, "request_retry", key, p, func(tx persistence.Transaction) (*MCPAck, error) {
		if err := s.event(ctx, tx, b, execution.EventMCPRetryRequested, map[string]any{"reason": reason, "resends": n}); err != nil {
			return nil, err
		}
		return &MCPAck{Accepted: true, Message: "retry requested; reply without submitting a result and the command is resent"}, nil
	})
}

// RequestContextReset is request_context_reset: queued; the worker session
// is replaced once this command's dispatch has finished.
func (s *MCPService) RequestContextReset(ctx context.Context, b *MCPBinding, key, reason string) (*MCPAck, error) {
	p := map[string]any{"reason": reason}
	return s.mutate(ctx, b, "request_context_reset", key, p, func(tx persistence.Transaction) (*MCPAck, error) {
		if err := s.event(ctx, tx, b, execution.EventMCPContextResetRequest, map[string]any{"reason": reason}); err != nil {
			return nil, err
		}
		if err := s.outbox(ctx, tx, b, persistence.OutboxContextReset, "reset:"+b.DispatchID.String(),
			map[string]any{"dispatch_id": b.DispatchID.String(), "reason": reason}); err != nil {
			return nil, err
		}
		return &MCPAck{Accepted: true, Message: "context reset queued; the next command starts in a new session"}, nil
	})
}
