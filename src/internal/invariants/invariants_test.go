package invariants

import (
	"encoding/json"
	"testing"
)

func TestCatalogComplete(t *testing.T) {
	seen := map[ID]bool{}
	for _, d := range Catalog() {
		if d.Description == "" || (d.Severity != Critical && d.Severity != Warning) || seen[d.ID] {
			t.Fatalf("bad definition %+v", d)
		}
		seen[d.ID] = true
	}
	if len(seen) != 14 {
		t.Fatalf("%d invariants", len(seen))
	}
}

func TestReportDeterministic(t *testing.T) {
	a := []Violation{{Invariant: OneCurrentUnit, ExecutionID: "e2", EntityIDs: []string{"u2", "u1"}, Detail: "2 units"},
		{Invariant: DoneRequiresValidation, ExecutionID: "e1", EntityIDs: []string{"u9"}, Detail: "no PASS"},
		{Invariant: OneCurrentUnit, ExecutionID: "e2", EntityIDs: []string{"u1", "u2"}, Detail: "2 units"}}
	b := []Violation{a[1], a[0]}
	ra, rb := NewReport("system", []ID{OneCurrentUnit, DoneRequiresValidation}, a), NewReport("system", []ID{DoneRequiresValidation, OneCurrentUnit}, b)
	ja, _ := json.Marshal(ra)
	jb, _ := json.Marshal(rb)
	if string(ja) != string(jb) || len(ra.Violations) != 2 || ra.OK() || len(ra.Critical()) != 2 || ra.Violations[0].Severity != Critical {
		t.Fatalf("%s\n%s", ja, jb)
	}
	if !NewReport("x", nil, nil).OK() {
		t.Fatal("empty report not OK")
	}
	if NewReport("x", nil, []Violation{{Invariant: CurrentPlanMatchesWBS}}).Violations[0].Severity != Warning {
		t.Fatal("severity fill")
	}
}
