package hermestest

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func post(t *testing.T, url, session, rid, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Overseer-Session-ID", session)
	req.Header.Set("X-Request-ID", rid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// WBS 12.68: configured scenarios are deterministic.
func TestP1231ScenariosDeterministic(t *testing.T) {
	run := func() []any {
		g := New()
		defer g.Close()
		u := g.URL + "/v1/chat/completions"
		const id = "hs-1"
		g.Script(Behavior{Status: 429, ErrorCode: "rate_limited", RetryAfter: 2}, Behavior{})
		var out []any
		st, e := post(t, u, id, "r1", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":false}`)
		out = append(out, st, e["error"].(map[string]any)["code"])
		if st, _ := post(t, u, id, "cd", `{"model":"m","messages":[{"role":"user","content":"change working directory to /ok"}],"stream":false}`); st != 200 {
			t.Fatalf("cd %d", st)
		}
		st, m := post(t, u, id, "r2", `{"model":"m","messages":[{"role":"user","content":"change working directory to /ok"},{"role":"assistant","content":"ok"},{"role":"user","content":"x"}],"stream":false}`)
		msg := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		out = append(out, st, msg["content"], m["usage"].(map[string]any)["total_tokens"], *g.SessionCWD(id))
		g.KillSession(id)
		st, _ = post(t, u, id, "r3", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":false}`)
		st2, _ := post(t, g.URL+"/v1/sessions", id, "r4", `{}`)
		return append(out, st, st2, len(g.Messages()))
	}
	a, b := run(), run()
	want := "[429 rate_limited 200 OK r2 1000 /ok 404 404 2]"
	if fmtAny(a) != want || fmtAny(b) != want {
		t.Fatalf("scenarios not deterministic:\n%v\n%v", fmtAny(a), fmtAny(b))
	}
}

func fmtAny(v []any) string {
	b, _ := json.Marshal(v)
	s := strings.NewReplacer(`"`, "", ",", " ").Replace(string(b))
	return s
}
