package integration

import (
	"context"
	"testing"

	"overseer/internal/compression"
	"overseer/internal/gateway/hermes/hermestest"
	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/worker"
)

// Phase 13 rig: the Phase 12 rig with an active session in the project
// directory and a prepared worker prompt; every dispatch's raw response is
// scripted on the fake gateway and parsed by the completion hook.

const (
	p13Complete   = `{"protocol_version":"1.0","status":"complete","summary":"Implemented worker result parsing.","completed":["Added protocol parser","Added semantic validation","Added parser tests"],"remaining":[],"blocking":[],"next_action":"Run deterministic validation."}`
	p13Incomplete = `{"protocol_version":"1.0","status":"incomplete","summary":"Implemented parser and persistence.","completed":["Parser","Persistence"],"remaining":["HTTP handlers","Handler tests"],"blocking":[],"next_action":"Implement HTTP handlers."}`
	p13Blocked    = `{"protocol_version":"1.0","status":"blocked","summary":"Cannot build the module.","completed":[],"remaining":["Implementation"],"blocking":["dependency example.com/lib does not compile"],"next_action":"Fix the dependency."}`
	p13Failed     = `{"protocol_version":"1.0","status":"failed","summary":"The repository became unusable.","completed":[],"remaining":[],"blocking":["git index corrupted"],"next_action":"Restore the repository."}`
	p13Prose      = "Implementation finished, everything looks good!"
)

type p13Rig struct {
	*p12Rig
	wp      *compression.WorkerPrompt
	attempt int
}

func p13From(t *testing.T, r *p12Rig, cwd string) *p13Rig {
	t.Helper()
	r.newSession(t)
	r.cd(t, cwd)
	return &p13Rig{p12Rig: r, wp: r.prompt(t, "")}
}

func p13NewRig(t *testing.T) *p13Rig { return p13From(t, p12NewRig(t), "/repos/project-a") }

func (r *p13Rig) rs() *service.ResultService { return r.h.App.Results() }

// report dispatches the prompt once with content as the worker's raw reply
// and returns the completed dispatch (parsed by the completion hook).
func (r *p13Rig) report(t *testing.T, content string, extra ...hermestest.Behavior) worker.WorkerDispatch {
	t.Helper()
	b := hermestest.Behavior{Content: &content}
	if len(extra) > 0 {
		b = extra[0]
		b.Content = &content
	}
	r.g.Script(b)
	r.attempt++
	res := r.dispatch(t, r.wp, r.attempt)
	if res.Dispatch.Status != worker.DispatchCompleted {
		t.Fatalf("dispatch not completed: %+v", res.Err)
	}
	return res.Dispatch
}

func (r *p13Rig) result(t *testing.T, d worker.WorkerDispatch) *persistence.StoredWorkerResult {
	t.Helper()
	res, err := r.rs().ResultByDispatch(context.Background(), r.pos.ProjectID, d.ID)
	if err != nil {
		t.Fatalf("no accepted result for %s: %v", d.ID, err)
	}
	return res
}

func (r *p13Rig) attempts(t *testing.T, d worker.WorkerDispatch) []persistence.ParseAttemptRecord {
	t.Helper()
	id := d.ID
	as, _, err := r.rs().Attempts(context.Background(), persistence.ParseAttemptFilter{ProjectID: r.pos.ProjectID, DispatchID: &id})
	if err != nil {
		t.Fatal(err)
	}
	return as
}

// boundary fingerprints workflow + project + phase + acceptance-relevant
// rows (runtime, units, state files, project, phases).
func (r *p13Rig) boundary(t *testing.T) string {
	return r.workflowRows(t) + "#" + p10Rows(t, r.h, "phases", "project_id = '"+r.pos.ProjectID.String()+"'")
}

func workerEvent(s string) worker.EventType { return worker.EventType(s) }
