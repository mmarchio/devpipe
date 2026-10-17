package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"overseer/internal/continuation"
)

// P14-81 — WBS 14.81-14.86 + 14.5 over REST.
func TestP1481ContinuationAPI(t *testing.T) {
	r := p14NewRig(t)
	ctx := context.Background()
	cb := r.base + "/continuations"

	// Settings (T14.5.x, 14.37/14.38/14.41).
	code, raw, body := p9HTTP(t, http.MethodGet, r.base+"/settings/continuation", "")
	if code != 200 || body["context_threshold"].(float64) != 0.8 || !strings.Contains(raw, `"max_continuations_per_work_unit":8`) || body["stored"] != false {
		t.Fatalf("GET settings %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, r.base+"/settings/continuation", `{"max_continuations_per_work_unit":0,"unknown_context_policy":"x","bogus":1}`); code != 400 ||
		!strings.Contains(raw, "bogus") {
		t.Fatalf("PUT invalid %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, r.base+"/settings/continuation", `{"max_continuations_per_work_unit":0,"unknown_context_policy":"x"}`); code != 400 ||
		!strings.Contains(raw, "max_continuations_per_work_unit") || !strings.Contains(raw, "unknown_context_policy") {
		t.Fatalf("PUT invalid fields %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPut, r.base+"/settings/continuation", `{"max_no_progress_continuations":5}`); code != 200 ||
		!strings.Contains(raw, `"max_no_progress_continuations":5`) || !strings.Contains(raw, `"max_continuations_per_work_unit":8`) {
		t.Fatalf("PUT %d %s", code, raw)
	}
	if st, stored, _ := r.cs().ContinuationSettings(ctx, r.pos.ProjectID); !stored || st.MaxNoProgressContinuations != 5 {
		t.Fatal("T14.37.3 settings not persisted")
	}
	// T14.5.1-T14.5.4: the threshold is project_settings.context_threshold,
	// constrained 0 < t <= 1 by the database as well as the domain.
	for th, ok := range map[string]bool{"0.8": true, "1": true, "0": false, "-0.5": false, "1.01": false} {
		_, err := r.h.DBExec(ctx, "UPDATE project_settings SET context_threshold = "+th+" WHERE project_id = '"+r.pos.ProjectID.String()+"'")
		if (err == nil) != ok {
			t.Fatalf("T14.5 threshold %s accepted=%v", th, err == nil)
		}
	}
	if _, err := r.h.DBExec(ctx, "UPDATE project_settings SET context_threshold = 0.8 WHERE project_id = '"+r.pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}

	// Data: C1 completed (same session), C2 pending.
	r.initial(t, p14Report("incomplete", []string{"A"}, []string{"B"}, "B"), 5000)
	r.reply(p14Report("complete", []string{"A", "B"}, nil, "x"), 6000)
	r.process(t)
	if code, raw, _ := p9HTTP(t, http.MethodGet, r.base+"/current-continuation", ""); code != 404 || !strings.Contains(raw, "no_active_continuation") {
		t.Fatalf("14.82 none active: %d %s", code, raw)
	}
	r.initial(t, p14Report("incomplete", []string{"X"}, []string{"Y"}, "Y"), 7000)
	cs := r.unitContinuations(t)
	c1, c2 := cs[0], cs[1]

	code, raw, body = p9HTTP(t, http.MethodGet, cb, "")
	if code != 200 || body["total"].(float64) != 2 {
		t.Fatalf("14.81 list %d %s", code, raw)
	}
	for q, want := range map[string]float64{"?status=COMPLETED": 1, "?strategy=same_session": 1, "?work_unit=" + r.pos.UnitID.String(): 2,
		"?phase=" + r.pos.PhaseID.String(): 2, "?work_unit=" + uuid.NewString(): 0, "?limit=1": 2} {
		code, raw, body := p9HTTP(t, http.MethodGet, cb+q, "")
		if code != 200 || body["total"].(float64) != want {
			t.Fatalf("14.81 filter %s: %d %s", q, code, raw)
		}
		if q == "?limit=1" && body["count"].(float64) != 1 {
			t.Fatal("14.81 pagination")
		}
	}
	for _, q := range []string{"?status=nope", "?strategy=nope", "?phase=x", "?limit=-1"} {
		if code, _, _ := p9HTTP(t, http.MethodGet, cb+q, ""); code != 400 {
			t.Fatalf("filter %s = %d", q, code)
		}
	}
	if code, raw, _ := p9HTTP(t, http.MethodGet, cb+"/"+c1.ID.String(), ""); code != 200 || !strings.Contains(raw, `"worker-reported, advisory"`) {
		t.Fatalf("GET %d %s", code, raw)
	}
	if code, _, _ := p9HTTP(t, http.MethodGet, cb+"/"+uuid.NewString(), ""); code != 404 {
		t.Fatal("unknown continuation")
	}
	// Project isolation: another project's URL never reveals C1.
	other := createViaAPI(t, r.h, "p14 other", "")
	if code, _, _ := p9HTTP(t, http.MethodGet, r.h.BaseURL+"/api/v1/projects/"+other.ID+"/continuations/"+c1.ID.String(), ""); code != 404 {
		t.Fatal("14.81 isolation")
	}
	if _, _, body := p9HTTP(t, http.MethodGet, r.h.BaseURL+"/api/v1/projects/"+other.ID+"/continuations", ""); body["total"].(float64) != 0 {
		t.Fatal("14.81 isolation list")
	}
	code, raw, body = p9HTTP(t, http.MethodGet, r.base+"/current-continuation", "")
	if code != 200 || !strings.Contains(raw, c2.ID.String()) || !strings.Contains(raw, "Attempt 2 of 8") {
		t.Fatalf("14.82 current %d %s", code, raw)
	}
	code, raw, body = p9HTTP(t, http.MethodGet, cb+"/"+c1.ID.String()+"/history", "")
	trs, _ := body["transitions"].([]any)
	if code != 200 || len(trs) != len(p14SameSequence) {
		t.Fatalf("14.83 history %d %s", code, raw)
	}
	for i, tr := range trs {
		m := tr.(map[string]any)
		if int(m["sequence"].(float64)) != i+1 || m["to"] != p14SameSequence[i] {
			t.Fatalf("14.83 order %v", trs)
		}
	}
	if code, raw, _ := p9HTTP(t, http.MethodGet, cb+"/"+c1.ID.String()+"/state", ""); code != 200 || !strings.Contains(raw, "CONTINUATION_STATE") {
		t.Fatalf("14.73 %d %s", code, raw)
	}
	if code, raw, body := p9HTTP(t, http.MethodGet, cb+"/metrics", ""); code != 200 || body["continuations_total"].(float64) != 2 ||
		body["same_session_continuations"].(float64) != 1 {
		t.Fatalf("14.88 %d %s", code, raw)
	}
	// Retry / override safety.
	if code, raw, _ := p9HTTP(t, http.MethodPost, cb+"/"+c1.ID.String()+"/retry", ""); code != 409 || !strings.Contains(raw, "retry_not_allowed") {
		t.Fatalf("14.84 %d %s", code, raw)
	}
	if code, raw, _ := p9HTTP(t, http.MethodPost, cb+"/"+c1.ID.String()+"/override", `{"strategy":"fresh_session"}`); code != 409 {
		t.Fatalf("14.85 override completed %d %s", code, raw)
	}
	if code, _, _ := p9HTTP(t, http.MethodPost, cb+"/"+c2.ID.String()+"/override", `{"strategy":"sideways"}`); code != 400 {
		t.Fatal("override validation")
	}
	news := r.ops("new")
	if code, raw, _ := p9HTTP(t, http.MethodPost, cb+"/"+c2.ID.String()+"/override", `{"strategy":"fresh_session"}`); code != 200 ||
		!strings.Contains(raw, `"override_strategy":"fresh_session"`) || r.ops("new") != news {
		t.Fatalf("14.85 override %d %s", code, raw)
	}
	r.reply(p14Report("complete", []string{"X", "Y"}, nil, "x"), 1000)
	r.process(t)
	if got := r.cont(t, c2.ID); got.Strategy != continuation.ContinueFreshSession || got.Decision.Reason != continuation.ReasonOperatorOverride ||
		r.ops("new") != news+1 {
		t.Fatalf("14.85 override applied at execution: %+v", got.Decision)
	}
}

var p14SameSequence = []string{"PENDING", "DECIDING", "SAME_SESSION", "RESTORING_CWD", "READY", "DISPATCHING", "WAITING", "COMPLETED"}
