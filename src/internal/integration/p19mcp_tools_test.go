package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"overseer/internal/execution"
)

// T5.2–T5.5, T6.1–T6.8, T7.1–T7.11, T9.7 — the MCP tools against a live
// dispatch: discovery, protocol errors, binding by the command's token,
// reads, validation, policy and idempotency; then the token dies with its
// dispatch.
func TestP19MCPTools(t *testing.T) {
	r, team := p19mcpRig(t)
	c := team.mcp
	ctx := context.Background()

	// T5.3: the registry.
	lt, err := c.cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range lt.Tools {
		names = append(names, tl.Name)
	}
	if got := strings.Join(names, ","); !strings.Contains(got, "get_current_state") || len(names) != 11 {
		t.Fatalf("tools %s", got)
	}
	// T5.4: an unknown tool is a protocol error.
	if _, _, err := c.call("drop_tables", map[string]any{}); err == nil {
		t.Fatal("unknown tool accepted")
	}
	// T5.5: malformed arguments are refused before any handler.
	if text, isErr, err := c.call("get_task", map[string]any{"token": 42}); err == nil && !isErr {
		t.Fatalf("malformed arguments accepted: %s", text)
	}
	// T5.6: an unknown token is refused.
	if text, isErr, _ := c.call("get_current_state", map[string]any{"token": "ovt_" + strings.Repeat("0", 64)}); !isErr || !strings.HasPrefix(text, "UNAUTHORIZED") {
		t.Fatalf("unknown token: %s", text)
	}
	// T5.10: a client without the bearer token cannot connect.
	if _, err := p19mcpConnect(t, r.h.App.MCPServer().Addr(), "wrong"); err == nil {
		t.Fatal("connected with a wrong bearer token")
	}

	errText := func(name string, args map[string]any) string {
		t.Helper()
		text, isErr, err := c.call(name, args)
		if err != nil || !isErr {
			t.Errorf("%s %v: expected a tool error, got %s %v", name, args, text, err)
		}
		return text
	}
	team.during = map[string]func(string) bool{
		"test task A": func(tok string) bool {
			// T6.1 / T6.8: the position, only the requested fields.
			st := c.must("get_current_state", map[string]any{"token": tok, "fields": []string{"state", "task", "command_state", "role"}})
			if st["task"] != "A" || st["command_state"] != "TEST" || st["role"] != "TESTER" || len(st) != 4 {
				t.Errorf("state %v", st)
			}
			// T6.2 / T6.3.
			task := c.must("get_task", map[string]any{"token": tok})
			if task["key"] != "A" || task["objective"] != "Deliver A" || len(task["acceptance_checks"].([]any)) != 2 {
				t.Errorf("task %v", task)
			}
			if b := c.must("get_task", map[string]any{"token": tok, "key": "B"}); b["depends_on"].([]any)[0] != "A" {
				t.Errorf("task B %v", b)
			}
			if text := errText("get_task", map[string]any{"token": tok, "key": "Z"}); !strings.HasPrefix(text, "NOT_FOUND") {
				t.Errorf("missing task: %s", text)
			}
			// T6.5 / T6.6 / T6.7.
			if ctxv := c.must("get_context", map[string]any{"token": tok}); ctxv["active_task"] != "A" || len(ctxv["plan"].([]any)) != 2 {
				t.Errorf("context %v", ctxv)
			}
			if role := c.must("get_role", map[string]any{"token": tok}); role["key"] != "TESTER" || !strings.Contains(role["instructions"].(string), "TESTER") {
				t.Errorf("role %v", role)
			}
			hist := c.must("get_execution_history", map[string]any{"token": tok})
			reps := hist["reports"].([]any)
			if len(reps) != 1 || reps[0].(map[string]any)["kind"] != "IMPLEMENT" {
				t.Errorf("history %v", hist)
			}
			// T7.5: the result must match the task's checks.
			if text := errText("submit_result", map[string]any{"token": tok, "idempotency_key": "bad", "status": "COMPLETE", "summary": "x",
				"criteria_met": []bool{true}}); !strings.HasPrefix(text, "CRITERIA_COUNT") {
				t.Errorf("criteria count: %s", text)
			}
			if text := errText("submit_result", map[string]any{"token": tok, "idempotency_key": "bad2", "status": "COMPLETE", "summary": "x",
				"criteria_met": []bool{true, false}}); !strings.HasPrefix(text, "CRITERIA_UNMET") {
				t.Errorf("complete with an unmet check: %s", text)
			}
			// T7.7 / T7.8: transitions are requests within the check state's outcomes.
			if text := errText("request_transition", map[string]any{"token": tok, "idempotency_key": "t1", "outcome": "PHASE_COMPLETE"}); !strings.HasPrefix(text, "ILLEGAL_TRANSITION") ||
				!strings.Contains(text, "COMPLETE, INCOMPLETE, RETRY") {
				t.Errorf("illegal transition: %s", text)
			}
			c.must("request_transition", map[string]any{"token": tok, "idempotency_key": "t2", "outcome": "COMPLETE", "reason": "all green"})
			// T7.11 / T3.8 / T3.9: idempotency.
			args := map[string]any{"token": tok, "idempotency_key": "p1", "message": "testing"}
			first := c.must("report_progress", args)
			again := c.must("report_progress", args)
			if first["replayed"] == true || again["replayed"] != true {
				t.Errorf("replay %v %v", first, again)
			}
			if text := errText("report_progress", map[string]any{"token": tok, "idempotency_key": "p1", "message": "other"}); !strings.HasPrefix(text, "CONFLICT") {
				t.Errorf("key reuse: %s", text)
			}
			// T7.3 / T7.10: a blocker and a context reset are queued for the outbox.
			c.must("report_blocker", map[string]any{"token": tok, "idempotency_key": "b1", "reason": "flaky disk"})
			c.must("request_context_reset", map[string]any{"token": tok, "idempotency_key": "r1", "reason": "context full"})
			c.must("request_retry", map[string]any{"token": tok, "idempotency_key": "rt1"})
			return false
		},
	}
	res, err := r.eng().Run(ctx, r.execID, 200)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if st := r.execStatus(t); st != string(execution.ExecutionCompleted) {
		t.Fatalf("execution %s, recovery %q\ntrace: %s", st, r.recoveryReason(t), p16Trace(res))
	}
	if team.during["test task A"] != nil {
		t.Fatal("the in-flight checks did not run")
	}
	where := "project_id = '" + r.pid.String() + "'"
	if n := r.count(t, "SELECT count(*) FROM mcp_requests q JOIN worker_dispatches d ON d.id = q.dispatch_id WHERE d."+where+" AND q.tool = 'report_progress' AND q.idempotency_key = 'p1'"); n != 1 {
		t.Fatalf("%d stored p1 requests", n)
	}
	if n := r.count(t, "SELECT count(*) FROM workflow_outbox WHERE "+where+" AND kind IN ('BLOCKER_NOTIFY', 'CONTEXT_RESET')"); n != 2 {
		t.Fatalf("%d outbox actions", n)
	}
	evs := r.events(t)
	for _, typ := range []string{"MCP_BLOCKER", "MCP_TRANSITION_REQUESTED", "MCP_RETRY_REQUESTED", "MCP_CONTEXT_RESET_REQUESTED"} {
		if countOf(evs, typ) != 1 {
			t.Fatalf("%s: %v", typ, evs)
		}
	}
	// The requested reset was applied before the next command: one session
	// more than the skill, the plan and each task open (4).
	if n := r.count(t, "SELECT count(*) FROM worker_sessions WHERE "+where); n != 5 {
		t.Fatalf("%d sessions, want 5", n)
	}
	// T9.7: every command finished — its token no longer binds.
	team.mu.Lock()
	tok := team.tokens[len(team.tokens)-1]
	team.mu.Unlock()
	if text, isErr, _ := c.call("get_current_state", map[string]any{"token": tok}); !isErr || !strings.Contains(text, "no longer in progress") {
		t.Fatalf("late call: %s", text)
	}
}

// T7.12 / T12.9 — conflicting results submitted at once for one command:
// exactly one is stored, the other is told it was already submitted.
func TestP19MCPConcurrentSubmissions(t *testing.T) {
	r, team := p19mcpRig(t)
	c := team.mcp
	var accepted, refused atomic.Int32
	team.during = map[string]func(string) bool{
		"implement task A": func(tok string) bool {
			var wg sync.WaitGroup
			for i, status := range []string{"COMPLETE", "BLOCKED"} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					text, isErr, err := c.call("submit_result", map[string]any{"token": tok, "idempotency_key": fmt.Sprintf("race%d", i),
						"status": status, "summary": "racing " + status})
					switch {
					case err != nil:
						t.Errorf("submit: %v", err)
					case !isErr:
						accepted.Add(1)
					case strings.HasPrefix(text, "ALREADY_SUBMITTED"):
						refused.Add(1)
					default:
						t.Errorf("submit: %s", text)
					}
				}()
			}
			wg.Wait()
			return true
		},
	}
	// Run to the first test of A (or to recovery, when BLOCKED won).
	res, err := r.eng().Run(context.Background(), r.execID, 8)
	if err != nil {
		t.Fatalf("run: %v (%s)", err, p16Trace(res))
	}
	if accepted.Load() != 1 || refused.Load() != 1 {
		t.Fatalf("accepted %d, refused %d", accepted.Load(), refused.Load())
	}
	if n := r.count(t, "SELECT count(*) FROM task_reports WHERE kind = 'IMPLEMENT' AND project_id = '"+r.pid.String()+"'"); n != 1 {
		t.Fatalf("%d IMPLEMENT reports", n)
	}
	// The stored result decided: BLOCKED needs the operator, COMPLETE
	// goes on to the test.
	var status string
	if err := r.h.DBScan(context.Background(), "SELECT status FROM task_reports WHERE kind = 'IMPLEMENT' AND project_id = '"+r.pid.String()+"'", &status); err != nil {
		t.Fatal(err)
	}
	if blocked := r.execStatus(t) == string(execution.ExecutionRecovery); blocked != (status == "BLOCKED") {
		t.Fatalf("result %s, execution %s", status, r.execStatus(t))
	}
}
