package service

import (
	"context"
	"testing"
)

// T18.207 (fold) — a missing critical requirement makes the instance
// NOT_READY; warnings alone → READY_WITH_WARNINGS.
func TestT18207ReadinessFold(t *testing.T) {
	item := func(st string) ProductionCheck {
		return func(context.Context) []ReadinessItem { return []ReadinessItem{{Name: st, Area: "x", Status: st}} }
	}
	for _, c := range []struct {
		checks []ProductionCheck
		want   string
	}{
		{[]ProductionCheck{item(CheckPass)}, ReadinessReady},
		{[]ProductionCheck{item(CheckPass), item(CheckWarn)}, ReadinessReadyWithWarnings},
		{[]ProductionCheck{item(CheckWarn), item(CheckFail), item(CheckPass)}, ReadinessNotReady},
	} {
		if got := NewProductionReadinessService(c.checks...).Report(context.Background()).Status; got != c.want {
			t.Fatalf("%v: %s, want %s", c.checks, got, c.want)
		}
	}
}
