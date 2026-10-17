// Package metrics is the overseer's metrics registry (Phase 18, WBS
// 18.113 / 18.114): counters, gauges and histograms with documented,
// stable names and closed label sets, exposed in the Prometheus text
// format. High-cardinality identifiers (UUIDs) are never accepted as label
// values. Stdlib only.
package metrics

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Kind is a metric type.
type Kind string

const (
	KindCounter   Kind = "counter"
	KindGauge     Kind = "gauge"
	KindHistogram Kind = "histogram"
)

// Categories (WBS 18.113).
var Categories = []string{"workflow", "worker", "prompt", "compression", "cache", "precompute", "validation", "repository",
	"database", "recovery", "http", "scheduler"}

// Desc documents a metric.
type Desc struct {
	Name     string
	Help     string
	Category string
	Kind     Kind
	Labels   []string
	// Values optionally closes a label's value set.
	Values  map[string][]string
	Buckets []float64 // histograms (seconds by default)
}

// DefaultBuckets are latency buckets in seconds.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}

// InvalidLabel replaces a rejected label value.
const InvalidLabel = "invalid"

var (
	nameRE = regexp.MustCompile(`^overseer_[a-z][a-z0-9_]*$`)
	uuidRE = regexp.MustCompile(`(?i)[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}`)
	hexRE  = regexp.MustCompile(`(?i)^[0-9a-f]{24,}$`)
)

type series struct {
	labels []string
	value  float64
	counts []uint64
	sum    float64
	count  uint64
}

type metric struct {
	desc   Desc
	mu     sync.Mutex
	series map[string]*series
}

// Registry holds metrics.
type Registry struct {
	mu       sync.Mutex
	metrics  map[string]*metric
	rejected *Counter
}

// NewRegistry builds an empty registry (with the label-rejection counter).
func NewRegistry() *Registry {
	r := &Registry{metrics: map[string]*metric{}}
	r.rejected = r.Counter(Desc{Name: "overseer_metrics_label_rejections_total", Help: "Label values rejected as high-cardinality or undeclared.",
		Category: "http", Labels: []string{"metric"}})
	return r
}

func (r *Registry) register(d Desc) *metric {
	if !nameRE.MatchString(d.Name) || d.Help == "" {
		panic(fmt.Sprintf("metrics: invalid descriptor %q", d.Name))
	}
	ok := false
	for _, c := range Categories {
		ok = ok || c == d.Category
	}
	if !ok {
		panic(fmt.Sprintf("metrics: %s: unknown category %q", d.Name, d.Category))
	}
	if d.Kind == KindHistogram && len(d.Buckets) == 0 {
		d.Buckets = DefaultBuckets
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.metrics[d.Name]; ok {
		if m.desc.Kind != d.Kind || strings.Join(m.desc.Labels, ",") != strings.Join(d.Labels, ",") {
			panic(fmt.Sprintf("metrics: %s re-registered with a different shape", d.Name))
		}
		return m
	}
	m := &metric{desc: d, series: map[string]*series{}}
	r.metrics[d.Name] = m
	return m
}

// ValidLabelValue reports whether v may be used as a label value: short,
// not UUID / long-hex shaped (WBS 18.114).
func ValidLabelValue(v string) bool {
	return len(v) <= 64 && !uuidRE.MatchString(v) && !hexRE.MatchString(v)
}

func (r *Registry) sanitize(m *metric, values []string) []string {
	out := make([]string, len(m.desc.Labels))
	for i := range out {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		allowed := m.desc.Values[m.desc.Labels[i]]
		valid := ValidLabelValue(v)
		if valid && len(allowed) > 0 {
			valid = false
			for _, a := range allowed {
				valid = valid || a == v
			}
		}
		if !valid {
			v = InvalidLabel
			if r.rejected != nil && m.desc.Name != r.rejected.m.desc.Name {
				r.rejected.Inc(m.desc.Name)
			}
		}
		out[i] = v
	}
	return out
}

func (m *metric) get(labels []string) *series {
	k := strings.Join(labels, "\x00")
	s := m.series[k]
	if s == nil {
		s = &series{labels: labels}
		if m.desc.Kind == KindHistogram {
			s.counts = make([]uint64, len(m.desc.Buckets))
		}
		m.series[k] = s
	}
	return s
}

// Counter is a monotonically increasing metric.
type Counter struct {
	r *Registry
	m *metric
}

// Counter registers (or returns) a counter.
func (r *Registry) Counter(d Desc) *Counter {
	d.Kind = KindCounter
	return &Counter{r: r, m: r.register(d)}
}

// Inc adds 1.
func (c *Counter) Inc(labels ...string) { c.Add(1, labels...) }

// Add adds v ≥ 0.
func (c *Counter) Add(v float64, labels ...string) {
	if v < 0 {
		return
	}
	l := c.r.sanitize(c.m, labels)
	c.m.mu.Lock()
	c.m.get(l).value += v
	c.m.mu.Unlock()
}

// SetTotal sets the counter to a total derived from durable state (a
// collector recomputing it from database rows at scrape time, so restarts
// neither reset nor double count it — WBS 18.124). v < 0 is ignored.
func (c *Counter) SetTotal(v float64, labels ...string) {
	if v < 0 {
		return
	}
	l := c.r.sanitize(c.m, labels)
	c.m.mu.Lock()
	c.m.get(l).value = v
	c.m.mu.Unlock()
}

// Gauge is a settable metric.
type Gauge struct {
	r *Registry
	m *metric
}

// Gauge registers (or returns) a gauge.
func (r *Registry) Gauge(d Desc) *Gauge {
	d.Kind = KindGauge
	return &Gauge{r: r, m: r.register(d)}
}

// Set sets the value.
func (g *Gauge) Set(v float64, labels ...string) {
	l := g.r.sanitize(g.m, labels)
	g.m.mu.Lock()
	g.m.get(l).value = v
	g.m.mu.Unlock()
}

// Add adds v (may be negative).
func (g *Gauge) Add(v float64, labels ...string) {
	l := g.r.sanitize(g.m, labels)
	g.m.mu.Lock()
	g.m.get(l).value += v
	g.m.mu.Unlock()
}

// Histogram observes distributions.
type Histogram struct {
	r *Registry
	m *metric
}

// Histogram registers (or returns) a histogram.
func (r *Registry) Histogram(d Desc) *Histogram {
	d.Kind = KindHistogram
	return &Histogram{r: r, m: r.register(d)}
}

// Observe records v.
func (h *Histogram) Observe(v float64, labels ...string) {
	l := h.r.sanitize(h.m, labels)
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	s := h.m.get(l)
	for i, b := range h.m.desc.Buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
}

// SetCumulative replaces a series with an aggregate derived from durable
// state: counts[i] = observations ≤ Buckets[i] (cumulative, one per
// bucket), plus the total sum and count.
func (h *Histogram) SetCumulative(counts []uint64, sum float64, count uint64, labels ...string) {
	if len(counts) != len(h.m.desc.Buckets) {
		return
	}
	l := h.r.sanitize(h.m, labels)
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	s := h.m.get(l)
	copy(s.counts, counts)
	s.sum, s.count = sum, count
}

// Buckets returns the histogram's bucket bounds.
func (h *Histogram) Buckets() []float64 { return append([]float64(nil), h.m.desc.Buckets...) }

// Value returns a counter / gauge series value (tests, read models).
func (r *Registry) Value(name string, labels ...string) float64 {
	r.mu.Lock()
	m := r.metrics[name]
	r.mu.Unlock()
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.series[strings.Join(labels, "\x00")]; s != nil {
		if m.desc.Kind == KindHistogram {
			return float64(s.count)
		}
		return s.value
	}
	return 0
}

// HistogramSum returns a histogram series' sum and count.
func (r *Registry) HistogramSum(name string, labels ...string) (float64, uint64) {
	r.mu.Lock()
	m := r.metrics[name]
	r.mu.Unlock()
	if m == nil {
		return 0, 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.series[strings.Join(labels, "\x00")]; s != nil {
		return s.sum, s.count
	}
	return 0, 0
}

// Catalog returns every descriptor sorted by name (WBS 18.113).
func (r *Registry) Catalog() []Desc {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Desc, 0, len(r.metrics))
	for _, m := range r.metrics {
		out = append(out, m.desc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(v)
}

func labelText(names, values []string, extra ...string) string {
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		parts = append(parts, n+`="`+escape(values[i])+`"`)
	}
	parts = append(parts, extra...)
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func num(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// WriteText writes the Prometheus text exposition, deterministically
// ordered by metric name then label values.
func (r *Registry) WriteText(w io.Writer) error {
	var b strings.Builder
	for _, d := range r.Catalog() {
		r.mu.Lock()
		m := r.metrics[d.Name]
		r.mu.Unlock()
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", d.Name, escape(d.Help), d.Name, d.Kind)
		m.mu.Lock()
		keys := make([]string, 0, len(m.series))
		for k := range m.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := m.series[k]
			if d.Kind != KindHistogram {
				fmt.Fprintf(&b, "%s%s %s\n", d.Name, labelText(d.Labels, s.labels), num(s.value))
				continue
			}
			for i, bk := range d.Buckets {
				fmt.Fprintf(&b, "%s_bucket%s %d\n", d.Name, labelText(d.Labels, s.labels, `le="`+num(bk)+`"`), s.counts[i])
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", d.Name, labelText(d.Labels, s.labels, `le="+Inf"`), s.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", d.Name, labelText(d.Labels, s.labels), num(s.sum))
			fmt.Fprintf(&b, "%s_count%s %d\n", d.Name, labelText(d.Labels, s.labels), s.count)
		}
		m.mu.Unlock()
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// CatalogMarkdown renders the documented catalog (docs/metrics.md).
func (r *Registry) CatalogMarkdown() string {
	var b strings.Builder
	b.WriteString("| Metric | Type | Category | Labels | Description |\n| --- | --- | --- | --- | --- |\n")
	for _, d := range r.Catalog() {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", d.Name, d.Kind, d.Category, strings.Join(d.Labels, ", "), d.Help)
	}
	return b.String()
}
