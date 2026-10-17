package service

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/readmodel"
)

// ProviderCheck probes one configured provider (built by the composition
// root; endpoints / credentials never leave it).
type ProviderCheck struct {
	Kind  string // "worker" | "compression" | "database"
	Name  string
	Check func(ctx context.Context) error
}

// SystemStatusConfig is the static system facts.
type SystemStatusConfig struct {
	Version            string
	StartedAt          time.Time
	SchedulerEnabled   bool
	SchedulerInterval  time.Duration
	ValidationCapacity int
	// Profile is the runtime profile (WBS 18.1 T18.1.4).
	Profile string
	// SchemaVersion is the schema the process runs against (WBS 18.130).
	SchemaVersion int
	// InstanceID identifies the process.
	InstanceID string
}

// SystemStatusService reports system status from health / readiness
// checks (WBS 17.107–17.109, P17-62). A failing provider is reported as
// UNHEALTHY without marking the process dead (/health stays liveness).
type SystemStatusService struct {
	web    persistence.WebReadRepository
	db     func(ctx context.Context) error
	checks []ProviderCheck
	cfg    SystemStatusConfig
	now    func() time.Time
	audit  *AuditVerificationService
	gate   *WorkGate
}

// WithAudit reports the audit verification and the new-work gate.
func (s *SystemStatusService) WithAudit(v *AuditVerificationService, gate *WorkGate) *SystemStatusService {
	s.audit, s.gate = v, gate
	return s
}

// NewSystemStatusService builds the service.
func NewSystemStatusService(web persistence.WebReadRepository, db func(ctx context.Context) error, checks []ProviderCheck, cfg SystemStatusConfig) *SystemStatusService {
	return &SystemStatusService{web: web, db: db, checks: checks, cfg: cfg, now: time.Now}
}

// Status returns the current system status.
func (s *SystemStatusService) Status(ctx context.Context) (*readmodel.SystemStatus, error) {
	st := &readmodel.SystemStatus{Database: "HEALTHY", SchedulerEnabled: s.cfg.SchedulerEnabled, SchedulerInterval: s.cfg.SchedulerInterval.String(),
		ValidationCapacity: s.cfg.ValidationCapacity, Version: s.cfg.Version, Profile: s.cfg.Profile, SchemaVersion: s.cfg.SchemaVersion, InstanceID: s.cfg.InstanceID, StartedAt: s.cfg.StartedAt.UTC(),
		Uptime: s.now().Sub(s.cfg.StartedAt).Truncate(time.Second).String(), Providers: []readmodel.ProviderHealth{}}
	if s.db != nil {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		if err := s.db(cctx); err != nil {
			st.Database = "UNHEALTHY"
		}
		cancel()
	}
	st.Providers = append(st.Providers, readmodel.ProviderHealth{Kind: "database", Name: "PostgreSQL", Status: st.Database})
	for _, c := range s.checks {
		ph := readmodel.ProviderHealth{Kind: c.Kind, Name: c.Name, Status: "UNKNOWN"}
		if c.Check != nil {
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if err := c.Check(cctx); err != nil {
				ph.Status, ph.Detail = "UNHEALTHY", "unreachable"
			} else {
				ph.Status = "HEALTHY"
			}
			cancel()
		}
		st.Providers = append(st.Providers, ph)
	}
	if st.Database == "HEALTHY" {
		sc, err := s.web.SchedulerCounts(ctx)
		if err != nil {
			return nil, translate(err)
		}
		st.Runnable, st.Waiting, st.ActiveDispatches, st.ActiveValidations = sc.Runnable, sc.Waiting, sc.ActiveDispatches, sc.RunningValidations
		if dc, err := s.web.DashboardCounts(ctx); err == nil {
			st.ActiveExecutions = dc.ActiveExecutions
		}
	}
	if s.audit != nil {
		if v := s.audit.Last(); v != nil {
			st.AuditVerification, st.AuditAcknowledged = string(v.Status), v.Acknowledged
		}
	}
	st.NewWorkBlocked = []string{}
	for _, r := range s.gate.Blocks() {
		st.NewWorkBlocked = append(st.NewWorkBlocked, r)
	}
	sort.Strings(st.NewWorkBlocked)
	return st, nil
}

// MetricsService derives the metrics pages from backend telemetry
// (WBS 17.110–17.114, P17-63 .. P17-65); the browser never estimates.
type MetricsService struct {
	web         persistence.WebReadRepository
	cache       *PromptCacheService
	compression *CompressionService
	precompute  *PrecomputeService
}

// NewMetricsService builds the service; the Phase 9–11 services may be nil
// (their metrics are then reported unknown).
func NewMetricsService(web persistence.WebReadRepository, cache *PromptCacheService, compression *CompressionService, precompute *PrecomputeService) *MetricsService {
	return &MetricsService{web: web, cache: cache, compression: compression, precompute: precompute}
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func metric(name string, v float64, unit string) readmodel.Metric {
	return readmodel.Metric{Name: name, Value: round(v, 3), Unit: unit, Known: true}
}

func ratio(name string, num, den int, unit string) readmodel.Metric {
	if den == 0 {
		return readmodel.Metric{Name: name, Unit: unit}
	}
	return metric(name, float64(num)/float64(den), unit)
}

func optional(name string, v *float64, scale float64, unit string) readmodel.Metric {
	if v == nil {
		return readmodel.Metric{Name: name, Unit: unit}
	}
	return metric(name, *v*scale, unit)
}

// Project returns all metric groups of a project.
func (s *MetricsService) Project(ctx context.Context, projectID uuid.UUID) (*readmodel.Metrics, error) {
	x, err := s.web.ProjectMetrics(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	m := &readmodel.Metrics{}
	cacheHit := readmodel.Metric{Name: "Prompt cache hit rate", Unit: "ratio"}
	if s.cache != nil {
		if st, err := s.cache.Stats(ctx, projectID); err == nil && st.Lookups > 0 {
			cacheHit = metric("Prompt cache hit rate", st.HitRate, "ratio")
		}
	}
	reduction := readmodel.Metric{Name: "Compression reduction", Unit: "ratio"}
	canonical := readmodel.Metric{Name: "Canonical tokens (avg)", Unit: "tokens"}
	workerTok := readmodel.Metric{Name: "Worker prompt tokens (avg)", Unit: "tokens"}
	compRatio := readmodel.Metric{Name: "Compression ratio", Unit: "ratio"}
	if s.compression != nil {
		if cm, err := s.compression.ProjectMetrics(ctx, projectID); err == nil && cm != nil && cm.Runs > 0 {
			reduction = metric("Compression reduction", cm.AverageReduction, "ratio")
			canonical = metric("Canonical tokens (avg)", cm.AverageCanonicalTok, "tokens")
			workerTok = metric("Worker prompt tokens (avg)", cm.AverageCompressedTok, "tokens")
			if cm.AverageCanonicalTok > 0 {
				compRatio = metric("Compression ratio", cm.AverageCompressedTok/cm.AverageCanonicalTok, "ratio")
			}
		}
	}
	precomp := readmodel.Metric{Name: "Precomputation utilization", Unit: "ratio"}
	if s.precompute != nil {
		if pm, err := s.precompute.Metrics(ctx, projectID); err == nil && pm.Overall.Created > 0 {
			precomp = metric("Precomputation utilization", pm.Overall.Utilization, "ratio")
		}
	}
	single := ratio("Single-invocation completion rate", x.SingleInvocationDone, x.UnitsDone, "ratio")
	m.Execution = []readmodel.Metric{
		metric("Completed units", float64(x.UnitsDone), "units"),
		optional("Average unit duration", x.AvgUnitSeconds, 1, "seconds"),
		single,
		ratio("Continuations per unit", x.Continuations, x.UnitsDispatched, "per unit"),
		ratio("Validation failure rate", x.ValidationFails, x.ValidationRuns, "ratio"),
		metric("Fix attempts", float64(x.FixAttempts), "attempts"),
		cacheHit,
		reduction,
	}
	m.Context = []readmodel.Metric{
		optional("Average context at completion", x.AvgContextAtCompletion, 100, "percent"),
		metric("Session resets", float64(x.Resets), "resets"),
		metric("Continuations", float64(x.Continuations), "continuations"),
		single,
	}
	m.Planning = []readmodel.Metric{
		metric("Atomic units", float64(x.UnitsByKind["ATOMIC"]), "units"),
		metric("Composition units", float64(x.UnitsByKind["COMPOSITION"]), "units"),
		metric("Integration units", float64(x.UnitsByKind["INTEGRATION"]), "units"),
		metric("Validation units", float64(x.UnitsByKind["VALIDATION"]), "units"),
		optional("Average dependencies per unit", x.AvgDependenciesPerUnit, 1, "dependencies"),
		metric("Plan amendments", float64(x.PlanAmendments), "amendments"),
	}
	m.Prompt = []readmodel.Metric{canonical, workerTok, compRatio, cacheHit, precomp}
	phases, err := s.web.PhaseMetrics(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	for _, p := range phases {
		pm := readmodel.PhaseMetrics{PhaseID: p.PhaseID.String(), Order: p.Order, Name: p.Name, Status: p.Status, Units: p.Units,
			PlanAttempts: p.PlanAttempts, Continuations: p.Continuations, ValidationAttempts: p.ValidationAttempts, Fixes: p.Fixes}
		if p.DurationSeconds != nil {
			pm.DurationSeconds = round(*p.DurationSeconds, 1)
		}
		m.Phases = append(m.Phases, pm)
	}
	sort.SliceStable(m.Phases, func(i, j int) bool { return m.Phases[i].Order < m.Phases[j].Order })
	return m, nil
}
