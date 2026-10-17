package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
)

// T18.205 — every DONE unit has a complete end-to-end lineage (WBS
// version / hash, plan, task hash, prompt chain with state-file snapshot
// hashes, dispatches, worker result, validation, checkpoint, events,
// correlation ids) by ids and hashes only; another project's unit is not
// reachable through this project.
func TestT18205UnitLineage(t *testing.T) {
	r := p17NewRig(t)
	r.queuePlan(r.planJSON(p16Unit{key: "a"}, p16Unit{key: "b", deps: []string{"a"}}))
	r.queue("complete")
	r.queue("complete")
	r.runUntil(t, execution.StepCompleted, 200)
	pid := r.pid.String()
	units := p18IDs(t, r, "SELECT id::text FROM work_units WHERE project_id = '"+pid+"' AND status = 'DONE' ORDER BY unit_key")
	if len(units) != 2 {
		t.Fatalf("setup: %d DONE units", len(units))
	}
	for _, u := range units {
		var l persistence.UnitLineage
		if code := r.b.getJSON("/api/v1/projects/"+pid+"/work-units/"+u+"/lineage", &l); code != http.StatusOK {
			t.Fatalf("lineage %s: %d", u, code)
		}
		if !l.Complete {
			t.Fatalf("lineage of %s incomplete: %+v", u, l)
		}
		if l.WBSHash == "" || l.PlanID == "" || l.ObjectiveHash == "" || len(l.Dispatches) == 0 || len(l.Validations) == 0 ||
			len(l.Checkpoints) == 0 || len(l.Events) == 0 || len(l.CorrelationIDs) == 0 {
			t.Fatalf("lineage of %s: %+v", u, l)
		}
		d := l.Dispatches[0]
		files := r.count(t, "SELECT count(*) FROM context_snapshot_files WHERE snapshot_id = '"+d.Prompt.ContextSnapshotID+"'")
		if d.Prompt.CanonicalPromptHash == "" || d.Prompt.WorkerPromptHash == "" || len(d.Prompt.StateFiles) != files || d.Result == nil {
			t.Fatalf("dispatch lineage: %+v", d)
		}
	}
	// The console unit page shows the lineage.
	if code, page := r.b.page("/projects/" + pid + "/work-units/" + units[0]); code != http.StatusOK || !strings.Contains(page, "Lineage") ||
		!strings.Contains(page, "COMPLETE") {
		t.Fatalf("unit page %d lacks the lineage", code)
	}
	// No large content: the response carries no prompt text.
	_, raw := r.b.api(http.MethodGet, "/api/v1/projects/"+pid+"/work-units/"+units[0]+"/lineage", nil)
	if strings.Contains(string(raw), "Planned the phase") || strings.Contains(string(raw), "Worker reports") {
		t.Fatal("lineage carries content")
	}
	// Ownership: a unit is not reachable through another project id.
	if code, _ := r.b.api(http.MethodGet, "/api/v1/projects/"+uuid.NewString()+"/work-units/"+units[0]+"/lineage", nil); code != http.StatusNotFound {
		t.Fatalf("foreign project: %d", code)
	}
}

func p18IDs(t *testing.T, r *p17Rig, sql string) []string {
	t.Helper()
	rows, err := r.h.App.DB().Raw().Query(t.Context(), sql)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}
