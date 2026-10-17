package access

import (
	"strings"
	"testing"
)

// P17-13 (WBS 17.122 / 17.57 / 17.105) — secrets are removed by key and by
// value; token counts and secret references survive.
func TestP1713Redactor(t *testing.T) {
	r := NewRedactor("sk-live-ABCDEF123456", "", "abc")
	in := map[string]any{
		"api_key":            "sk-live-ABCDEF123456",
		"auth_token":         "tok",
		"password":           "pw",
		"api_key_configured": true,
		"api_key_env":        "OVERSEER_KEY",
		"token_estimate":     float64(120),
		"max_tokens":         float64(10),
		"nested":             []any{map[string]any{"Authorization": "Bearer x", "note": "key sk-live-ABCDEF123456 leaked"}},
		"empty_secret":       "",
	}
	out := r.Value(in).(map[string]any)
	for _, k := range []string{"api_key", "auth_token", "password"} {
		if out[k] != Redacted {
			t.Fatalf("%s not redacted: %v", k, out[k])
		}
	}
	if out["api_key_configured"] != true || out["api_key_env"] != "OVERSEER_KEY" || out["token_estimate"] != float64(120) || out["max_tokens"] != float64(10) {
		t.Fatalf("non-secret changed: %v", out)
	}
	n := out["nested"].([]any)[0].(map[string]any)
	if n["Authorization"] != Redacted || strings.Contains(n["note"].(string), "sk-live") {
		t.Fatalf("nested: %v", n)
	}
	if out["empty_secret"] != "" {
		t.Fatal("empty secret should stay empty")
	}
	if in["api_key"] != "sk-live-ABCDEF123456" {
		t.Fatal("input mutated")
	}
	if r.String("abc") != "abc" {
		t.Fatal("short secret must not be used")
	}
}
