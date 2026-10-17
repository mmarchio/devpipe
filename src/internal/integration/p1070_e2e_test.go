package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"overseer/internal/compressionprovider/providertest"
	"overseer/internal/dbrepo"
)

// P10-58 (WBS 10.70 + Phase 10 DoD): end-to-end acceptance through the
// application's REST API and the real OpenAI-compatible adapter.
//
// Given (reached through the versioned write paths): Work Unit "10.17" at
// IMPLEMENT; ENGINEER instruction v5; PROJECT_STATE v11, ARCHITECTURE v7,
// PHASE_STATE v4, ACTIVE_TASK v2; template v6; response protocol v3;
// compression model A, target 8,000.
func TestP1070Phase10EndToEndAcceptance(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	h, _, boot := p9RestartRig(t, srv)
	ctx := context.Background()
	raw := h.App.DB().Raw()
	spec := p9DefaultSpec()
	spec.UnitName = "10.17"
	pos := p9Rig(t, h, spec)
	e := &p10Env{h: h, pos: pos}

	for v := 2; v <= 5; v++ {
		if _, err := dbrepo.NewInstructionVersionRepository(raw).AppendVersion(ctx, pos.InstructionID.String(), "ENGINEER.md v"+itoa(v)+": implement the active unit with tests first."); err != nil {
			t.Fatal(err)
		}
	}
	reach := func(typ string, target int, content func(v int) string) {
		for v := 2; v <= target; v++ {
			e.writeFile(t, typ, content(v))
		}
	}
	reach("PROJECT_STATE", 11, func(v int) string { return "PROJECT_STATE v" + itoa(v) + "\n" + p9Background("PROJECT_STATE", 120) })
	reach("ARCHITECTURE", 7, func(v int) string { return "ARCHITECTURE v" + itoa(v) + "\n" + p9Background("ARCHITECTURE", 120) })
	reach("PHASE_STATE", 4, func(v int) string { return "PHASE_STATE v" + itoa(v) + "\n" + p9Background("PHASE_STATE", 40) })
	reach("ACTIVE_TASK", 2, func(v int) string { return "ACTIVE_TASK v" + itoa(v) + ": implement work unit 10.17." })
	for v := 2; v <= 6; v++ {
		if _, err := dbrepo.NewPromptTemplateVersionRepository(raw).AppendVersion(ctx, pos.TemplateID, "IMPLEMENT_WORK_UNIT v"+itoa(v)+": implement the active work unit.\n"); err != nil {
			t.Fatal(err)
		}
	}
	for v := 2; v <= 3; v++ {
		if _, err := dbrepo.NewResponseProtocolRepository(raw).AppendVersion(ctx, pos.ProtocolID, p9Protocol); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.DBExec(ctx, "UPDATE state_protocol_references SET protocol_version = 3 WHERE state_id = '"+pos.StateID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DBExec(ctx, "UPDATE project_settings SET compression_provider = 'fake', compression_model = 'model-A', context_window = 64000 WHERE project_id = '"+pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	base := h.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/settings/compression", `{"mode":"AUTO","target_tokens":8000,"max_attempts":2}`); code != 200 {
		t.Fatalf("settings: %s", raw)
	}
	snap1 := e.snapshot(t)
	prepare := func(base string, snap string) map[string]any {
		code, raw, body := p9HTTP(t, http.MethodPost, base+"/worker-prompts/prepare",
			`{"snapshot_id":"`+snap+`","template_id":"`+pos.TemplateID.String()+`","template_version":6}`)
		if code != 200 {
			t.Fatalf("prepare: %d %s", code, raw)
		}
		return body
	}

	// First preparation: K1 MISS → compose → compress → validate → WP1 → store.
	p1 := prepare(base, snap1.String())
	k1 := p1["cache_key"].(string)
	wp1 := p1["worker_prompt"].(map[string]any)
	if p1["hit"] != false || srv.Calls() != 1 || wp1["source"] != "COMPRESSED" || p1["entry"] == nil {
		t.Fatalf("first preparation: %v", p1)
	}
	if !strings.Contains(wp1["content"].(string), "name: 10.17") || wp1["estimated_tokens"].(float64) > 8000 {
		t.Fatal("WP1 content / size")
	}
	entry1 := p1["entry"].(map[string]any)
	md := entry1["metadata"].(map[string]any)
	files := map[string]float64{}
	for _, f := range md["files"].([]any) {
		fm := f.(map[string]any)
		files[fm["type_key"].(string)] = fm["version"].(float64)
	}
	if files["PROJECT_STATE"] != 11 || files["ARCHITECTURE"] != 7 || files["PHASE_STATE"] != 4 || files["ACTIVE_TASK"] != 2 {
		t.Fatalf("stored key-source versions %v", files)
	}

	// Second preparation: K1 HIT → WP1, no provider call, hit count + last-used.
	time.Sleep(5 * time.Millisecond)
	p2 := prepare(base, snap1.String())
	if p2["hit"] != true || p2["cache_key"] != k1 || srv.Calls() != 1 || p2["worker_prompt"].(map[string]any)["id"] != wp1["id"] {
		t.Fatalf("second preparation: %v", p2)
	}
	_, _, got := p9HTTP(t, http.MethodGet, base+"/prompt-cache/"+entry1["id"].(string), "")
	if got["hit_count"] != float64(1) || got["last_used_at"].(string) <= entry1["last_used_at"].(string) {
		t.Fatalf("hit count / last used: %v", got)
	}

	// ACTIVE_TASK v2 → v3: snapshot hash changes → K2 MISS; K1 historical.
	e.writeFile(t, "ACTIVE_TASK", "ACTIVE_TASK v3: implement work unit 10.17 including error mapping.")
	snap2 := e.snapshot(t)
	p3 := prepare(base, snap2.String())
	k2 := p3["cache_key"].(string)
	if p3["hit"] != false || k2 == k1 || srv.Calls() != 2 {
		t.Fatal("ACTIVE_TASK change did not miss")
	}
	_, exRaw, ex := p9HTTP(t, http.MethodPost, base+"/prompt-cache/explain", `{"snapshot_id":"`+snap2.String()+`","template_id":"`+pos.TemplateID.String()+`","template_version":6}`)
	if ex["result"] != "HIT" {
		t.Fatalf("explain after K2 build: %s", exRaw)
	}

	// Compression model A → B: canonical reused, compression misses, Model B
	// recompresses, new worker prompt.
	if _, err := h.DBExec(ctx, "UPDATE project_settings SET compression_model = 'model-B' WHERE project_id = '"+pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	canonBefore := p9Count(t, h, "canonical_prompts", "project_id = '"+pos.ProjectID.String()+"'")
	p4 := prepare(base, snap2.String())
	layers := p4["layers"].([]any)
	if p4["hit"] != false || len(layers) == 0 || layers[0] != "CANONICAL" || p4["canonical_prompt_id"] != p3["canonical_prompt_id"] {
		t.Fatalf("model change layering: %v", p4)
	}
	if p9Count(t, h, "canonical_prompts", "project_id = '"+pos.ProjectID.String()+"'") != canonBefore || srv.Calls() != 3 ||
		p4["worker_prompt"].(map[string]any)["id"] == p3["worker_prompt"].(map[string]any)["id"] || srv.Requests()[2].Model != "model-B" {
		t.Fatal("Model B did not recompress the reused canonical prompt")
	}
	k3 := p4["cache_key"].(string)
	_, statsBefore, _ := p9HTTP(t, http.MethodGet, base+"/prompt-cache/stats", "")
	_, listBefore, _ := p9HTTP(t, http.MethodGet, base+"/prompt-cache", "")

	// Restart: K1/K2 persisted, valid entries hit, history traceable,
	// metrics reconstruct, no RAM dependency.
	h.Close()
	h2 := boot()
	base2 := h2.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	_, statsAfter, _ := p9HTTP(t, http.MethodGet, base2+"/prompt-cache/stats", "")
	_, listAfter, list := p9HTTP(t, http.MethodGet, base2+"/prompt-cache", "")
	if statsAfter != statsBefore || listAfter != listBefore {
		t.Fatal("cache state differs after restart")
	}
	keys := map[string]bool{}
	for _, en := range list["entries"].([]any) {
		em := en.(map[string]any)
		keys[em["cache_key"].(string)] = em["status"] == "VALID"
	}
	if !keys[k1] || !keys[k2] || !keys[k3] {
		t.Fatalf("K1/K2/K3 not persisted as valid: %v", keys)
	}
	if p5 := prepare(base2, snap2.String()); p5["hit"] != true || p5["cache_key"] != k3 || srv.Calls() != 3 {
		t.Fatal("valid entry missed after restart")
	}
	_, lin, lb := p9HTTP(t, http.MethodGet, base2+"/worker-prompts/"+wp1["id"].(string)+"/lineage", "")
	if lb["context_snapshot_id"] != snap1.String() || lb["canonical_prompt_id"] != p1["canonical_prompt_id"] {
		t.Fatalf("historical K1 lineage not traceable: %s", lin)
	}
	_, _, st := p9HTTP(t, http.MethodGet, base2+"/prompt-cache/stats", "")
	if st["hits"] != float64(2) || st["misses"] != float64(3) || st["canonical_hits"] != float64(1) || st["valid"] != float64(3) {
		t.Fatalf("metrics after restart: %v", st)
	}
}
