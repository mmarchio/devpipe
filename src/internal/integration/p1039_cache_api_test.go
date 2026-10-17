package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compressionprovider/providertest"
)

// P10-40..P10-44, P10-55, P10-56 (WBS 10.39-10.43, 10.61, 10.63-10.66): the
// prompt-cache REST surface over the full application, with the fake
// OpenAI-compatible provider whose key is a sentinel that must never leak.

func p10PrepareBody(snap, tmpl uuid.UUID, version int) string {
	return `{"snapshot_id":"` + snap.String() + `","template_id":"` + tmpl.String() + `","template_version":` + itoa(version) + `}`
}

// p10APISetup boots the app with the fake provider, builds the position
// and configures compression (target 4,000 in a 64k context).
func p10APISetup(t *testing.T, h *Harness) (p9Position, string) {
	t.Helper()
	ctx := context.Background()
	pos := p9Rig(t, h, p9DefaultSpec())
	if _, err := h.DBExec(ctx, "UPDATE project_settings SET compression_provider = 'fake', compression_model = 'model-A', context_window = 64000 WHERE project_id = '"+pos.ProjectID.String()+"'"); err != nil {
		t.Fatal(err)
	}
	base := h.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	if code, raw, _ := p9HTTP(t, http.MethodPut, base+"/settings/compression", `{"mode":"AUTO","target_tokens":4000,"max_attempts":2}`); code != 200 {
		t.Fatalf("compression settings: %s", raw)
	}
	return pos, base
}

func TestP1039CacheAPI(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	h := p9APIHarness(t, srv)
	ctx := context.Background()
	pos, base := p10APISetup(t, h)
	prep := func(snap uuid.UUID) map[string]any {
		code, raw, body := p9HTTP(t, http.MethodPost, base+"/worker-prompts/prepare", p10PrepareBody(snap, pos.TemplateID, 1))
		if code != 200 {
			t.Fatalf("prepare: %d %s", code, raw)
		}
		if strings.Contains(raw, p9SentinelKey) {
			t.Fatal("secret in prepare response")
		}
		return body
	}
	first := prep(pos.SnapID)
	second := prep(pos.SnapID)
	if first["hit"] != false || second["hit"] != true || srv.Calls() != 1 {
		t.Fatalf("prepare via API: %v %v calls=%d", first["hit"], second["hit"], srv.Calls())
	}
	entry := second["entry"].(map[string]any)
	entryID := entry["id"].(string)
	if code, _, body := p9HTTP(t, http.MethodPost, base+"/worker-prompts/prepare", `{"snapshot_id":"x"}`); code != 400 || body["error"] == nil {
		t.Fatal("invalid prepare body accepted")
	}

	// --- inspection (WBS 10.39) ---
	code, raw, body := p9HTTP(t, http.MethodGet, base+"/prompt-cache", "")
	if code != 200 || body["count"] != float64(1) {
		t.Fatalf("T10.39.1 list: %s", raw)
	}
	for q, want := range map[string]float64{
		"?state=IMPLEMENT": 1, "?state=PLAN": 0, "?status=VALID": 1, "?status=FAILED": 0, "?source=ON_DEMAND": 1,
		"?source=PRECOMPUTED": 0, "?work_unit=" + pos.UnitID.String(): 1, "?phase=" + pos.PhaseID.String(): 1, "?layer=WORKER": 1,
	} {
		if _, _, b := p9HTTP(t, http.MethodGet, base+"/prompt-cache"+q, ""); b["count"] != want {
			t.Fatalf("T10.39.3 filter %s: %v", q, b["count"])
		}
	}
	if code, _, _ := p9HTTP(t, http.MethodGet, base+"/prompt-cache?status=BOGUS", ""); code != 400 {
		t.Fatal("bad filter accepted")
	}
	code, raw, body = p9HTTP(t, http.MethodGet, base+"/prompt-cache/"+entryID, "")
	if code != 200 || body["id"] != entryID || body["hit_count"] != float64(1) || body["metadata"] == nil {
		t.Fatalf("T10.39.2 get: %s", raw)
	}
	if strings.Contains(raw, p9SentinelKey) || strings.Contains(raw, p9AC1) {
		t.Fatal("T10.63.4 / T10.65.4 entry carries secrets or prompt content")
	}
	other, _, _ := p705Rig(t, h)
	otherBase := h.BaseURL + "/api/v1/projects/" + other.String()
	if code, _, _ := p9HTTP(t, http.MethodGet, otherBase+"/prompt-cache/"+entryID, ""); code != 404 {
		t.Fatal("T10.39.4/T10.63.1/T10.63.2 foreign project read the entry")
	}
	if _, _, b := p9HTTP(t, http.MethodGet, otherBase+"/prompt-cache", ""); b["count"] != float64(0) {
		t.Fatal("T10.63.3 foreign project lists entries")
	}
	if code, _, _ := p9HTTP(t, http.MethodPost, otherBase+"/worker-prompts/prepare", p10PrepareBody(pos.SnapID, pos.TemplateID, 1)); code != 404 {
		t.Fatal("T10.26.1 foreign project prepared another project's snapshot")
	}

	// --- explain (WBS 10.40/10.61): ACTIVE_TASK v4 → v5 ---
	e := &p10Env{h: h, pos: pos}
	for i := 0; i < 3; i++ { // ACTIVE_TASK v1 → v4
		e.writeFile(t, "ACTIVE_TASK", "ACTIVE_TASK v"+itoa(i+2)+": implement work unit 9.17.")
	}
	snapV4 := e.snapshot(t)
	prep(snapV4)
	e.writeFile(t, "ACTIVE_TASK", "ACTIVE_TASK v5: implement work unit 9.17 with the new validator.")
	snapV5 := e.snapshot(t)
	eventsBefore := p9Count(t, h, "prompt_cache_events", "project_id = '"+pos.ProjectID.String()+"'")
	code, raw, body = p9HTTP(t, http.MethodPost, base+"/prompt-cache/explain", p10PrepareBody(snapV5, pos.TemplateID, 1))
	if code != 200 || body["result"] != "MISS" {
		t.Fatalf("explain: %d %s", code, raw)
	}
	changed := map[string]map[string]any{}
	for _, c := range body["changed"].([]any) {
		cm := c.(map[string]any)
		changed[cm["name"].(string)] = cm
	}
	if c := changed["file:ACTIVE_TASK"]; c == nil || c["before"] != "ACTIVE_TASK v4" || c["after"] != "ACTIVE_TASK v5" {
		t.Fatalf("T10.40.2/10.61 ACTIVE_TASK change: %s", raw)
	}
	if changed["context_snapshot"] == nil || len(changed) != 2 {
		t.Fatalf("10.61 changed set: %v", changed)
	}
	unchanged := map[string]bool{}
	for _, u := range body["unchanged"].([]any) {
		unchanged[u.(string)] = true
	}
	for _, n := range []string{"role", "template", "response_protocol", "compression_config", "file:ARCHITECTURE"} {
		if !unchanged[n] {
			t.Fatalf("10.61 %s not reported unchanged: %s", n, raw)
		}
	}
	if strings.Contains(raw, p9SentinelKey) {
		t.Fatal("T10.40.3 secret in explanation")
	}
	if p9Count(t, h, "prompt_cache_events", "project_id = '"+pos.ProjectID.String()+"'") != eventsBefore {
		t.Fatal("T10.40.4 explain mutated the cache")
	}
	_, raw, body = p9HTTP(t, http.MethodPost, base+"/prompt-cache/explain", p10PrepareBody(snapV4, pos.TemplateID, 1))
	if body["result"] != "HIT" || len(body["changed"].([]any)) != 0 {
		t.Fatalf("T10.40.1 hit explanation: %s", raw)
	}

	// --- invalidation (WBS 10.41/10.42) ---
	code, raw, body = p9HTTP(t, http.MethodPost, base+"/prompt-cache/"+entryID+"/invalidate", `{"reason":"corrupt output"}`)
	if code != 200 || body["status"] != "INVALIDATED" || body["invalidation_reason"] != "corrupt output" {
		t.Fatalf("T10.41.1/T10.41.2: %s", raw)
	}
	if code, _, _ := p9HTTP(t, http.MethodPost, base+"/prompt-cache/"+entryID+"/invalidate", `{}`); code != 409 {
		t.Fatal("re-invalidating an invalidated entry must conflict")
	}
	if code, _, _ := p9HTTP(t, http.MethodPost, otherBase+"/prompt-cache/"+entryID+"/invalidate", `{}`); code != 404 {
		t.Fatal("foreign project invalidated the entry")
	}
	if b := prep(pos.SnapID); b["hit"] != false {
		t.Fatal("T10.41.3 invalidated entry hit")
	}
	code, _, body = p9HTTP(t, http.MethodPost, base+"/prompt-cache/invalidate", `{"reason":"systematic bug"}`)
	if code != 200 || body["invalidated"].(float64) < 2 {
		t.Fatalf("T10.42.1 project invalidation: %v", body)
	}

	// --- stats / activity / maintenance (WBS 10.64/10.65/10.58/10.36) ---
	code, raw, body = p9HTTP(t, http.MethodGet, base+"/prompt-cache/stats", "")
	if code != 200 || body["hits"] != float64(1) || body["lookups"] != float64(4) || body["valid"] != float64(0) ||
		body["invalidated"].(float64) < 3 || body["hit_rate"] != 0.25 || body["compression_calls_avoided"] != float64(1) {
		t.Fatalf("T10.64.1-T10.64.3 stats: %s", raw)
	}
	_, raw, empty := p9HTTP(t, http.MethodGet, otherBase+"/prompt-cache/stats", "")
	if empty["lookups"] != float64(0) || empty["hit_rate"] != float64(0) {
		t.Fatalf("T10.64.4 empty stats: %s", raw)
	}
	code, raw, body = p9HTTP(t, http.MethodGet, base+"/prompt-cache/activity?limit=50", "")
	acts := body["activity"].([]any)
	if code != 200 || len(acts) == 0 {
		t.Fatalf("T10.65.1 activity: %s", raw)
	}
	var prev string
	sawMissReason := false
	for i, a := range acts {
		am := a.(map[string]any)
		at := am["at"].(string)
		if i > 0 && at > prev {
			t.Fatal("T10.65.2 activity not newest-first")
		}
		prev = at
		if am["event"] == "CACHE_MISS" && strings.Contains(am["reason"].(string), "ACTIVE_TASK") {
			sawMissReason = true
		}
	}
	if !sawMissReason || strings.Contains(raw, p9AC1) || strings.Contains(raw, p9SentinelKey) {
		t.Fatalf("T10.65.3/T10.65.4 activity reasons/content: %s", raw)
	}
	if code, _, body := p9HTTP(t, http.MethodPost, base+"/prompt-cache/integrity-scan", ""); code != 200 || body["corrupt"] == nil {
		t.Fatal("integrity scan endpoint")
	}
	if code, _, body := p9HTTP(t, http.MethodPost, base+"/prompt-cache/gc", ""); code != 200 || body["deleted"] != float64(0) {
		t.Fatal("gc endpoint (RETAIN_ALL)")
	}
	if code, _, body := p9HTTP(t, http.MethodGet, base+"/prompt-cache/fragments", ""); code != 200 || body["count"].(float64) < 3 {
		t.Fatal("fragments endpoint")
	}
	code, _, body = p9HTTP(t, http.MethodGet, base+"/settings/prompt-cache", "")
	if code != 200 || body["cache_same_session_continuation"] != false || body["retention_policy"] != "RETAIN_ALL" {
		t.Fatal("cache settings defaults")
	}
	if code, _, _ := p9HTTP(t, http.MethodPut, base+"/settings/prompt-cache", `{"retention_policy":"RETAIN_DAYS","retention_value":0}`); code != 400 {
		t.Fatal("invalid retention accepted")
	}
	if code, _, body := p9HTTP(t, http.MethodPut, base+"/settings/prompt-cache", `{"retention_policy":"RETAIN_DAYS","retention_value":30}`); code != 200 || body["retention_value"] != float64(30) {
		t.Fatal("cache settings update")
	}
	if code, _, _ := p9HTTP(t, http.MethodDelete, base+"/prompt-cache/stats", ""); code != 405 {
		t.Fatal("read-only endpoint accepted DELETE")
	}

	// --- audit events (WBS 10.66) ---
	var evs string
	if err := h.DBScan(ctx, "SELECT string_agg(DISTINCT event_type, ',') FROM prompt_cache_events WHERE project_id = '"+pos.ProjectID.String()+"'", &evs); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"CACHE_LOOKUP", "CACHE_HIT", "CACHE_MISS", "CACHE_STORE", "CACHE_INVALIDATE", "CACHE_BUILD_STARTED", "CACHE_BUILD_COMPLETED"} {
		if !strings.Contains(evs, typ) {
			t.Fatalf("T10.66 %s not emitted (%s)", typ, evs)
		}
	}
	var leaks int
	if err := h.DBScan(ctx, `SELECT
		(SELECT count(*) FROM prompt_cache_events WHERE reason LIKE '%`+p9SentinelKey+`%' OR cache_key LIKE '%`+p9SentinelKey+`%') +
		(SELECT count(*) FROM prompt_cache_entries WHERE metadata::text LIKE '%`+p9SentinelKey+`%' OR error_message LIKE '%`+p9SentinelKey+`%')`, &leaks); err != nil {
		t.Fatal(err)
	}
	if leaks != 0 {
		t.Fatal("T10.66.5/T10.63.4 secret in cache metadata or events")
	}
}

// P10-54 (WBS 10.60): two overseer instances on one PostgreSQL race the
// same uncached prompt — one authoritative entry, both receive a valid
// worker prompt, no partial build left behind.
func TestP1060MultiInstance(t *testing.T) {
	srv := providertest.New(providertest.Reply{Delay: 300 * time.Millisecond})
	defer srv.Close()
	hA, _, boot := p9RestartRig(t, srv)
	hB := boot()
	pos, baseA := p10APISetup(t, hA)
	baseB := hB.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	type out struct {
		code int
		body map[string]any
	}
	results := make([]out, 4)
	var wg sync.WaitGroup
	for i := range results {
		base := baseA
		if i%2 == 1 {
			base = baseB
		}
		wg.Add(1)
		go func(i int, base string) {
			defer wg.Done()
			code, _, body := p9HTTP(t, http.MethodPost, base+"/worker-prompts/prepare", p10PrepareBody(pos.SnapID, pos.TemplateID, 1))
			results[i] = out{code, body}
		}(i, base)
	}
	wg.Wait()
	var wid string
	for i, r := range results {
		if r.code != 200 {
			t.Fatalf("instance call %d: %d %v", i, r.code, r.body)
		}
		id := r.body["worker_prompt"].(map[string]any)["id"].(string)
		if wid == "" {
			wid = id
		}
		if id != wid {
			t.Fatal("instances received different worker prompts")
		}
	}
	where := "project_id = '" + pos.ProjectID.String() + "'"
	if p9Count(t, hA, "prompt_cache_entries", where+" AND status = 'VALID'") != 1 || p9Count(t, hA, "prompt_cache_entries", where+" AND status = 'BUILDING'") != 0 {
		t.Fatal("duplicate authoritative entries or stranded build")
	}
	if srv.Calls() != 1 {
		t.Logf("redundant provider work across instances: %d calls (allowed)", srv.Calls())
	}
}

// P10-53 (WBS 10.59): entries, hashes, hits and metrics survive a restart;
// valid entries still hit; nothing depends on in-memory state.
func TestP1059RestartPersistence(t *testing.T) {
	srv := providertest.New()
	defer srv.Close()
	h, _, boot := p9RestartRig(t, srv)
	pos, base := p10APISetup(t, h)
	e := &p10Env{h: h, pos: pos}
	snaps := []uuid.UUID{pos.SnapID}
	for i := 2; i <= 4; i++ {
		e.writeFile(t, "ACTIVE_TASK", "ACTIVE_TASK v"+itoa(i)+": implement work unit 9.17.")
		snaps = append(snaps, e.snapshot(t))
	}
	for round := 0; round < 3; round++ {
		for _, s := range snaps {
			if code, raw, _ := p9HTTP(t, http.MethodPost, base+"/worker-prompts/prepare", p10PrepareBody(s, pos.TemplateID, 1)); code != 200 {
				t.Fatalf("prepare: %s", raw)
			}
		}
	}
	_, statsBefore, sb := p9HTTP(t, http.MethodGet, base+"/prompt-cache/stats", "")
	_, listBefore, _ := p9HTTP(t, http.MethodGet, base+"/prompt-cache", "")
	if sb["hits"] != float64(8) || sb["misses"] != float64(4) || sb["valid"] != float64(4) {
		t.Fatalf("fixture stats: %s", statsBefore)
	}
	calls := srv.Calls()
	h.Close()
	h2 := boot()
	base2 := h2.BaseURL + "/api/v1/projects/" + pos.ProjectID.String()
	_, statsAfter, _ := p9HTTP(t, http.MethodGet, base2+"/prompt-cache/stats", "")
	_, listAfter, _ := p9HTTP(t, http.MethodGet, base2+"/prompt-cache", "")
	if statsAfter != statsBefore || listAfter != listBefore {
		t.Fatal("entries / hashes / metrics differ after restart")
	}
	for _, s := range snaps {
		_, raw, b := p9HTTP(t, http.MethodPost, base2+"/worker-prompts/prepare", p10PrepareBody(s, pos.TemplateID, 1))
		if b["hit"] != true {
			t.Fatalf("valid entry missed after restart: %s", raw)
		}
	}
	if srv.Calls() != calls {
		t.Fatal("restart caused recompression")
	}
	var list map[string]any
	_ = json.Unmarshal([]byte(listAfter), &list)
	if list["count"] != float64(4) {
		t.Fatal("entry count after restart")
	}
}
