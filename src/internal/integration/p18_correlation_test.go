package integration

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"overseer/internal/execution"
)

// T18.111 / T18.112 — one operation is traceable end to end: an operator
// step request's X-Request-ID is the correlation id of the step it drives,
// of that step's events, of the worker dispatch it sends, of the audit row
// and of every log record (which also carries execution_id / project_id).
// Scheduler-driven steps get a generated id, shared by the step, its
// events and the validation run it starts.
func TestT18112OperationTraceable(t *testing.T) {
	logs := captureLogs(t)
	r := p17NewRig(t)
	ctx := context.Background()
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	stepPath := "/api/v1/projects/" + r.pid.String() + "/executions/" + r.execID.String() + "/step"
	exec := r.execID.String()

	// Drive steps by request until one sends a worker request.
	var traced string
	for i := 0; i < 10 && traced == ""; i++ {
		id := fmt.Sprintf("trace-%d", i)
		r.b.headers = map[string]string{"X-Request-ID": id}
		before := r.count(t, "SELECT count(*) FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"'")
		if code, body := r.b.api(http.MethodPost, stepPath, nil); code != http.StatusOK {
			t.Fatalf("step: %d %s", code, body)
		}
		if r.count(t, "SELECT count(*) FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"'") > before {
			traced = id
		}
	}
	r.b.headers = nil
	if traced == "" {
		t.Fatal("setup: no step sent a worker request")
	}
	q := func(sql string) int { return r.count(t, strings.ReplaceAll(sql, "$ID", traced)) }
	if q("SELECT count(*) FROM execution_steps WHERE execution_id = '"+exec+"' AND detail->>'correlation_id' = '$ID'") != 1 {
		t.Fatal("step not tagged with the request id")
	}
	if q("SELECT count(*) FROM execution_events WHERE execution_id = '"+exec+"' AND payload->>'correlation_id' = '$ID'") == 0 {
		t.Fatal("events not tagged")
	}
	if q("SELECT count(*) FROM worker_dispatches WHERE project_id = '"+r.pid.String()+"' AND correlation_id = '$ID'") != 1 {
		t.Fatal("dispatch not tagged")
	}
	if q("SELECT count(*) FROM audit_events WHERE request_id = '$ID' AND action = 'ENGINE_STEP'") != 1 {
		t.Fatal("audit row not tagged")
	}
	var tagged int
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, traced) && (strings.Contains(line, `"correlation_id":"`+traced+`"`) || strings.Contains(line, "correlation_id="+traced)) {
			tagged++
			if !strings.Contains(line, exec) {
				t.Fatalf("log line of the step lacks execution_id: %s", line)
			}
		}
	}
	if tagged == 0 {
		t.Fatalf("no log record carries correlation_id %s", traced)
	}

	// Scheduler-driven steps: generated, shared by step / events /
	// validation run.
	r.runUntil(t, execution.StepCompleted, 200)
	var cid string
	if err := r.h.DBScan(ctx, "SELECT correlation_id FROM validation_runs WHERE execution_id = '"+exec+"' ORDER BY started_at LIMIT 1", &cid); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cid, "op-") {
		t.Fatalf("validation run correlation %q", cid)
	}
	if r.count(t, "SELECT count(*) FROM execution_steps WHERE execution_id = '"+exec+"' AND detail->>'correlation_id' = '"+cid+"'") != 1 ||
		r.count(t, "SELECT count(*) FROM execution_events WHERE execution_id = '"+exec+"' AND payload->>'correlation_id' = '"+cid+"'") == 0 {
		t.Fatal("the validation step's trace is incomplete")
	}
	// 18.111: every engine step of the execution carries a correlation id.
	if n := r.count(t, "SELECT count(*) FROM execution_steps WHERE execution_id = '"+exec+"' AND COALESCE(detail->>'correlation_id', '') = ''"); n != 0 {
		t.Fatalf("%d steps without correlation id", n)
	}
}
