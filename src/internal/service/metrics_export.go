package service

import (
	"context"
	"io"

	"overseer/internal/metrics"
)

// MetricsExporter renders the Prometheus exposition (WBS 18.113): durable
// series are refreshed from the database first, then process-local
// series (HTTP, connection pool) are added by the collectors.
type MetricsExporter struct {
	reg        *metrics.Registry
	collectors []func(ctx context.Context, reg *metrics.Registry) error
}

// NewMetricsExporter builds the exporter over reg.
func NewMetricsExporter(reg *metrics.Registry, collectors ...func(ctx context.Context, reg *metrics.Registry) error) *MetricsExporter {
	return &MetricsExporter{reg: reg, collectors: collectors}
}

// Registry returns the registry (catalog, tests).
func (m *MetricsExporter) Registry() *metrics.Registry { return m.reg }

// Write refreshes every collector and writes the text exposition.
func (m *MetricsExporter) Write(ctx context.Context, w io.Writer) error {
	for _, c := range m.collectors {
		if err := c(ctx, m.reg); err != nil {
			return translate(err)
		}
	}
	return m.reg.WriteText(w)
}
