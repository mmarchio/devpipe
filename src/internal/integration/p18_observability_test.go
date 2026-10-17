package integration

import (
	"net/http"
	"strings"
	"testing"

	"overseer/internal/persistence"
)

// T18.233 — observability acceptance: one work unit is traced using only
// the external surfaces — the lineage API gives its dispatches, prompts,
// result, validation, checkpoint and correlation ids; every correlation id
// leads to log records of the execution; the metrics reflect the run; the
// audit log holds the operator requests that drove it.
func TestT18233TraceOneUnit(t *testing.T) {
	logs := captureLogs(t)
	r := p17NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}))
	r.queue("complete")
	pid, eid := r.pid.String(), r.execID.String()
	// The operator drives the run step by step (each request has its id).
	for i := 0; i < 60 && r.execStatus(t) != "COMPLETED"; i++ {
		r.b.headers = map[string]string{"X-Request-ID": "obs-" + itoa(i)}
		if code, body := r.b.api(http.MethodPost, "/api/v1/projects/"+pid+"/executions/"+eid+"/step", nil); code != http.StatusOK {
			t.Fatalf("step %d: %d %s", i, code, body)
		}
	}
	r.b.headers = nil
	if r.execStatus(t) != "COMPLETED" {
		t.Fatal("setup: not completed")
	}
	unit := p18FirstID(t, r.h, "SELECT id::text FROM work_units WHERE project_id = '"+pid+"' AND unit_key = 'a'")
	// Lineage.
	var l persistence.UnitLineage
	if code := r.b.getJSON("/api/v1/projects/"+pid+"/work-units/"+unit+"/lineage", &l); code != http.StatusOK || !l.Complete {
		t.Fatalf("lineage %d complete=%v", code, l.Complete)
	}
	if len(l.CorrelationIDs) == 0 || len(l.Dispatches) == 0 || l.Dispatches[0].CorrelationID == "" {
		t.Fatalf("lineage carries no correlation: %+v", l.CorrelationIDs)
	}
	// Logs: every correlation id of the unit leads to records of this execution.
	for _, c := range l.CorrelationIDs {
		if !p18LogLine(logs.String(), c, eid) {
			t.Errorf("no log record for correlation %s with the execution id", c)
		}
	}
	// Audit: the requests that drove the unit are there under the same ids.
	for _, c := range l.CorrelationIDs {
		if strings.HasPrefix(c, "obs-") {
			if n := r.count(t, "SELECT count(*) FROM audit_events WHERE request_id = '"+c+"' AND action = 'ENGINE_STEP'"); n != 1 {
				t.Errorf("audit has %d ENGINE_STEP rows for %s", n, c)
			}
		}
	}
	// Metrics reflect the run.
	_, body := r.b.page("/metrics")
	for _, series := range []string{`overseer_worker_dispatches_total{outcome="completed"}`, `overseer_validation_runs_total{status="PASS"}`,
		`overseer_repository_checkpoints_total{kind="UNIT",status="CREATED"}`, `overseer_executions{status="COMPLETED"}`} {
		if p18Series(body, series) < 1 {
			t.Errorf("metric %s missing", series)
		}
	}
}
