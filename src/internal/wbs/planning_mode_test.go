package wbs

import "testing"

// P17-22 (WBS 17.61) — the planning configuration persists exactly.
func TestP1722PlanningConfigurationRoundTrip(t *testing.T) {
	cases := []PlanningConfiguration{
		{Mode: PlanningAtomic, Atomic: DefaultAtomicPolicy(), PreferSmallFunctions: true},
		{Mode: PlanningAtomic, Atomic: AtomicPolicy{RequireIndependentlyTestable: true, SeparateIntegration: true}},
		{Mode: PlanningFeature},
		{Mode: PlanningComponent, PreferSmallFunctions: true},
		{Mode: PlanningCustom, CustomPolicy: "Split by bounded context", Atomic: AtomicPolicy{RequireAcceptanceCriteria: true}},
	}
	for _, c := range cases {
		if p := c.Validate(); len(p) != 0 {
			t.Fatalf("%+v: %v", c, p)
		}
		pol, gran, at := c.Encode()
		if got := DecodePlanningConfiguration(pol, gran, at); got != c {
			t.Fatalf("round trip: %+v -> %+v", c, got)
		}
	}
	bad := []PlanningConfiguration{
		{Mode: "AGILE"},
		{Mode: PlanningCustom},
		{Mode: PlanningFeature, CustomPolicy: "x"},
		{Mode: PlanningFeature, Atomic: DefaultAtomicPolicy()},
	}
	for _, c := range bad {
		if len(c.Validate()) == 0 {
			t.Fatalf("invalid accepted: %+v", c)
		}
	}
	if got := DecodePlanningConfiguration("", "", DefaultAtomicPolicy()); got.Mode != PlanningAtomic {
		t.Fatalf("legacy default: %+v", got)
	}
}
