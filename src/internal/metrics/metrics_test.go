package metrics

import (
	"strings"
	"testing"
)

func testRegistry() *Registry {
	r := NewRegistry()
	r.Counter(Desc{Name: "overseer_worker_dispatches_total", Help: "Worker dispatches by outcome.", Category: "worker", Labels: []string{"provider", "outcome"},
		Values: map[string][]string{"outcome": {"completed", "failed", "timeout", "delivery_unknown"}}}).Inc("hermes", "completed")
	r.Gauge(Desc{Name: "overseer_executions_active", Help: "Active executions.", Category: "workflow"}).Set(3)
	h := r.Histogram(Desc{Name: "overseer_validation_duration_seconds", Help: "Validation durations.", Category: "validation", Labels: []string{"scope"},
		Buckets: []float64{1, 5}})
	h.Observe(0.5, "unit")
	h.Observe(3, "unit")
	h.Observe(9, "unit")
	return r
}

// T18.113 — names / types stable and documented (golden exposition).
func TestT18113Exposition(t *testing.T) {
	var b strings.Builder
	if err := testRegistry().WriteText(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP overseer_executions_active Active executions.
# TYPE overseer_executions_active gauge
overseer_executions_active 3
# HELP overseer_metrics_label_rejections_total Label values rejected as high-cardinality or undeclared.
# TYPE overseer_metrics_label_rejections_total counter
# HELP overseer_validation_duration_seconds Validation durations.
# TYPE overseer_validation_duration_seconds histogram
overseer_validation_duration_seconds_bucket{scope="unit",le="1"} 1
overseer_validation_duration_seconds_bucket{scope="unit",le="5"} 2
overseer_validation_duration_seconds_bucket{scope="unit",le="+Inf"} 3
overseer_validation_duration_seconds_sum{scope="unit"} 12.5
overseer_validation_duration_seconds_count{scope="unit"} 3
# HELP overseer_worker_dispatches_total Worker dispatches by outcome.
# TYPE overseer_worker_dispatches_total counter
overseer_worker_dispatches_total{provider="hermes",outcome="completed"} 1
`
	if b.String() != want {
		t.Fatalf("exposition:\n%s", b.String())
	}
	if md := testRegistry().CatalogMarkdown(); !strings.Contains(md, "| `overseer_worker_dispatches_total` | counter | worker | provider, outcome |") {
		t.Fatal(md)
	}
}

// T18.114 — project / work-unit UUIDs never become label values.
func TestT18114CardinalityControl(t *testing.T) {
	r := testRegistry()
	c := r.Counter(Desc{Name: "overseer_worker_dispatches_total", Help: "Worker dispatches by outcome.", Category: "worker", Labels: []string{"provider", "outcome"}})
	c.Inc("1b4e28ba-2fa1-11d2-883f-0016d3cca427", "completed")
	c.Inc("hermes", "exploded") // outside the closed set
	var b strings.Builder
	_ = r.WriteText(&b)
	if strings.Contains(b.String(), "1b4e28ba") || strings.Contains(b.String(), "exploded") {
		t.Fatal("high-cardinality / undeclared label exposed")
	}
	if r.Value("overseer_metrics_label_rejections_total", "overseer_worker_dispatches_total") != 2 {
		t.Fatal("rejections not counted")
	}
	for _, v := range []string{"a1b2c3d4e5f6a7b8c9d0e1f2a3b4", strings.Repeat("x", 65)} {
		if ValidLabelValue(v) {
			t.Fatalf("%q accepted", v)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("bad name accepted")
		}
	}()
	r.Counter(Desc{Name: "dispatches", Help: "x", Category: "worker"})
}

func TestHistogramAggregation(t *testing.T) {
	r := testRegistry()
	sum, n := r.HistogramSum("overseer_validation_duration_seconds", "unit")
	if sum != 12.5 || n != 3 || r.Value("overseer_validation_duration_seconds", "unit") != 3 {
		t.Fatalf("%v %v", sum, n)
	}
}
