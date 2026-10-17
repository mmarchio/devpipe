package app

import (
	"os"
	"path/filepath"
	"testing"
)

// T18.113 — docs/metrics.md is the metric catalog the code registers
// (regenerate with OVERSEER_UPDATE_DOCS=1 go test ./internal/app -run TestT18113).
func TestT18113MetricsCatalogDocumented(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "metrics.md")
	want := "# Overseer metrics\n\nGenerated from the registered catalog (`app.MetricsCatalog`); `GET /metrics` exposes these series in the Prometheus text format.\n" +
		"Durable series are recomputed from database rows at every scrape (restart-safe, no double counting); `overseer_http_requests_total` and\n" +
		"`overseer_database_pool_connections` are per process. Labels never carry identifiers.\n\n" + MetricsCatalog().CatalogMarkdown()
	if os.Getenv("OVERSEER_UPDATE_DOCS") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatal("docs/metrics.md differs from the registered catalog; regenerate it")
	}
}
