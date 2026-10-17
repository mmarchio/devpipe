package workerresult_test

import (
	"strings"
	"testing"

	wr "overseer/internal/workerresult"
)

// P16-80 (16.15): a planner's plan travels as an opaque `plan` object,
// parsed through the Phase 13 protocol; malformed → protocol failure.
func TestP1680PlanPayload(t *testing.T) {
	st := settings(wr.ModeStrict, wr.RepairNone)
	plan := `{ "units" : [ {"key":"a","name":"A","kind":"ATOMIC","objective":"o","depends_on":[]} ], "n": 1.50 }`
	content := strings.TrimSuffix(valid, "}") + `,"plan":` + plan + `}`
	r, pe := parse(t, st, content)
	if pe != nil {
		t.Fatalf("plan rejected: %+v", pe)
	}
	if string(r.Plan) != `{"units":[{"key":"a","name":"A","kind":"ATOMIC","objective":"o","depends_on":[]}],"n":1.50}` {
		t.Fatalf("plan = %s", r.Plan)
	}
	plain, _ := parse(t, st, valid)
	if plain.Plan != nil || plain.NormalizedHash == r.NormalizedHash || !wr.Revalidate(r) {
		t.Fatal("plan must be covered by the hash and absent otherwise")
	}
	if !strings.Contains(string(wr.CanonicalJSON(r)), `"plan":{`) || strings.Contains(string(wr.CanonicalJSON(plain)), "plan") {
		t.Fatal("canonical form")
	}
	for _, bad := range []string{`[1,2]`, `"text"`, `{"units":[}`} {
		if _, pe := parse(t, st, strings.TrimSuffix(valid, "}")+`,"plan":`+bad+`}`); pe == nil {
			t.Fatalf("malformed plan %s accepted", bad)
		}
	}
}
