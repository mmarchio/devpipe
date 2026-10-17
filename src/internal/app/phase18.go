package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/backup"
	"overseer/internal/config"
	"overseer/internal/database"
	"overseer/internal/dbrepo"
	"overseer/internal/gitcheckpoint"
	"overseer/internal/health"
	"overseer/internal/metrics"
	"overseer/internal/persistence"
	"overseer/internal/repoinspect"
	"overseer/internal/resilience"
	"overseer/internal/service"
)

// InstanceID identifies this process (leases, snapshots, diagnostics).
func (a *App) InstanceID() string { return a.instanceID }

// ConfigSnapshot returns the startup configuration snapshot.
func (a *App) ConfigSnapshot() config.Snapshot { return a.configSnapshot }

// SchemaVersion returns the schema version the process runs against.
func (a *App) SchemaVersion() int { return a.schemaVersion }

func newInstanceID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "overseer"
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), uuid.NewString()[:8])
}

// recordConfigSnapshot persists the redacted configuration of this start
// and reports which sections changed since the previous start (WBS 18.3:
// detectable without revealing credentials).
func (a *App) recordConfigSnapshot(ctx context.Context) error {
	if a.instanceID == "" {
		a.instanceID = newInstanceID()
	}
	v, err := database.CurrentVersion(ctx, a.db.Raw())
	if err != nil {
		return err
	}
	a.schemaVersion = v
	a.configSnapshot = a.cfg.Snapshot()
	if v < 112 {
		return nil // schema predates the snapshot table (migrations disabled)
	}
	repo := dbrepo.NewAppConfigSnapshotRepository(a.db.Raw())
	prev, err := repo.Latest(ctx)
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return err
	}
	if prev != nil && prev.ConfigHash != a.configSnapshot.Hash {
		a.lg.WithComponent("config").Warn("configuration changed since the previous start",
			"sections", config.ChangedSections(prev.Sections, a.configSnapshot.Sections), "previous_hash", prev.ConfigHash,
			"config_hash", a.configSnapshot.Hash)
	}
	return repo.Insert(ctx, &persistence.AppConfigSnapshotRecord{ID: uuid.New(), CreatedAt: nowUTC(), InstanceID: a.instanceID,
		Profile: a.cfg.EffectiveProfile(), AppVersion: Version, SchemaVersion: v, ConfigHash: a.configSnapshot.Hash,
		Sections: a.configSnapshot.Sections, Config: a.configSnapshot.Canonical})
}

func nowUTC() time.Time { return time.Now().UTC() }

// seedWorkflowDefaults seeds the global prompt templates (idempotent,
// advisory-locked). The default workflow template is seeded on first use
// by the project initializer: it lives in a system project that should
// not exist before any project does.
func (a *App) seedWorkflowDefaults(ctx context.Context) error {
	raw := a.db.Raw()
	if err := service.NewPromptTemplateService(dbrepo.NewPromptTemplateRepository(raw), dbrepo.NewPromptTemplateVersionRepository(raw)).
		WithTxPool(raw).SeedDefaults(ctx); err != nil {
		return fmt.Errorf("prompt templates: %w", err)
	}
	return nil
}

// availabilityConfig maps provider circuit / concurrency configuration
// (WBS 18.26–18.28, 18.92).
func (a *App) availabilityConfig() service.AvailabilityConfig {
	out := service.AvailabilityConfig{MaxConcurrent: a.cfg.Engine.MaxConcurrentDispatches, ProviderMax: map[string]int{},
		Breakers: map[string]resilience.BreakerConfig{}}
	for name, p := range a.cfg.WorkerProviders {
		out.ProviderMax[name] = p.MaxConcurrent
		out.Breakers[name] = resilience.BreakerConfig{Failures: p.Circuit.Failures, Window: p.Circuit.Window, Cooldown: p.Circuit.Cooldown,
			Successes: p.Circuit.Successes}
	}
	return out
}

// compressionAvailabilityConfig maps compression circuit / concurrency
// configuration (WBS 18.29, 18.94, 18.95).
func (a *App) compressionAvailabilityConfig() service.CompressionAvailabilityConfig {
	out := service.CompressionAvailabilityConfig{MaxConcurrent: a.cfg.Engine.MaxConcurrentCompressions,
		Breakers: map[string]resilience.BreakerConfig{}}
	for name, p := range a.cfg.CompressionProviders {
		out.Breakers[name] = resilience.BreakerConfig{Failures: p.Circuit.Failures, Window: p.Circuit.Window, Cooldown: p.Circuit.Cooldown,
			Successes: p.Circuit.Successes}
	}
	return out
}

// repoInspector inspects project repositories, ignoring the worker's
// reserved paths (Hermes's own role / skill definitions).
func (a *App) repoInspector() *repoinspect.Git {
	g := repoinspect.New()
	g.Exclude, g.Timeout = a.cfg.Engine.ReservedPaths(), a.cfg.Engine.GitTimeout
	return g
}

// checkpointer writes overseer checkpoints, never committing the worker's
// reserved paths.
func (a *App) checkpointer() *gitcheckpoint.Git {
	g := gitcheckpoint.New()
	g.Exclude, g.Timeout, g.RunHooks = a.cfg.Engine.ReservedPaths(), a.cfg.Engine.GitTimeout, a.cfg.Engine.RunGitHooks
	return g
}

// integrity builds the read-only invariant scanner service (WBS 18.39),
// shared by the API, the CLI and the periodic scan.
func (a *App) integrity() *service.IntegrityService {
	if a.integritySvc == nil {
		pool := a.db.Raw()
		a.integritySvc = service.NewIntegrityService(dbrepo.NewIntegrityScanner(pool), dbrepo.NewSettingsRepository(pool), a.checkpointer())
	}
	return a.integritySvc
}

// Integrity returns the invariant scanner service.
func (a *App) Integrity() *service.IntegrityService { return a.integrity() }

// workGate is the process-wide new-work admission gate (decision U9).
func (a *App) workGate() *service.WorkGate {
	if a.gate == nil {
		a.gate = service.NewWorkGate()
	}
	return a.gate
}

// WorkGate returns the new-work admission gate.
func (a *App) WorkGate() *service.WorkGate { return a.workGate() }

// auditVerifier builds the audit chain verifier (WBS 18.108–18.110).
func (a *App) auditVerifier() *service.AuditVerificationService {
	if a.auditVerify == nil {
		a.auditVerify = service.NewAuditVerificationService(dbrepo.NewAuditRepository(a.db.Raw()), a.workGate(),
			a.lg.WithComponent("audit").Logger)
	}
	return a.auditVerify
}

// AuditVerifier returns the audit chain verifier.
func (a *App) AuditVerifier() *service.AuditVerificationService { return a.auditVerifier() }

// verifyAuditInBackground verifies every audit chain once at startup
// without delaying readiness of the HTTP surface; a failure closes the
// work gate (decision U9).
func (a *App) verifyAuditInBackground() {
	ctx, cancel := context.WithCancel(context.Background())
	a.bgCancel = cancel
	v := a.auditVerifier()
	a.alerts().Start(a.cfg.Engine.AlertInterval)
	a.integrityMonitor().Start()
	a.diskMonitor().Start()
	a.bgDone.Add(1)
	go func() {
		defer a.bgDone.Done()
		if _, err := v.Verify(ctx, nil); err != nil && ctx.Err() == nil {
			a.lg.WithComponent("audit").Warn("startup audit verification failed to run", "error", err)
		}
	}()
}

// stopBackground cancels and waits for startup background work.
func (a *App) stopBackground() {
	if a.alertSvc != nil {
		a.alertSvc.Stop()
	}
	if a.integrityMon != nil {
		a.integrityMon.Stop()
	}
	if a.diskMon != nil {
		a.diskMon.Stop()
	}
	if a.bgCancel != nil {
		a.bgCancel()
		a.bgCancel = nil
	}
	a.bgDone.Wait()
}

// restoreGateSchema is the first schema with the full restore gate.
const restoreGateSchema = 122

// restoreGate builds the restored-database startup gate (WBS 18.44–18.46,
// decision U4).
func (a *App) restoreGate() *service.RestoreGateService {
	if a.restoreSvc != nil {
		return a.restoreSvc
	}
	pool := a.db.Raw()
	checks := []service.RestoreCheck{
		{Name: "schema_compatibility", Run: func(ctx context.Context, st *persistence.RestoreState) (bool, any, error) {
			cur, err := database.CurrentVersion(ctx, pool)
			if err != nil {
				return false, nil, err
			}
			report := map[string]any{"restored_schema": st.SchemaVersion, "current_schema": cur, "application_schema": a.schemaVersion}
			return st.SchemaVersion <= cur && cur == a.schemaVersion, report, nil
		}},
		{Name: "integrity_scan", Run: func(ctx context.Context, _ *persistence.RestoreState) (bool, any, error) {
			rep, err := a.integrity().Scan(ctx, nil, nil)
			if err != nil {
				return false, nil, err
			}
			return rep.OK(), rep, nil
		}},
		{Name: "audit_chain", Run: func(ctx context.Context, _ *persistence.RestoreState) (bool, any, error) {
			v, err := a.auditVerifier().Verify(ctx, nil)
			if err != nil {
				return false, nil, err
			}
			return !v.BlocksNewWork, map[string]any{"status": v.Status, "fingerprint": v.Fingerprint, "acknowledged": v.Acknowledged}, nil
		}},
		{Name: "recovery_scan", Run: func(ctx context.Context, _ *persistence.RestoreState) (bool, any, error) {
			// Informational: every unfinished operation is identified and
			// reconciled by the normal recovery path; nothing is dispatched
			// while the gate holds.
			res, err := service.NewStartupRecoveryScan(dbrepo.NewRecoveryScanRepository(pool), a.instanceID, nil).Scan(ctx)
			if err != nil {
				return false, nil, err
			}
			return true, map[string]any{"unfinished_operations": len(res.Facts), "facts": res.Facts, "live_leases": res.LiveLeases}, nil
		}},
	}
	a.restoreSvc = service.NewRestoreGateService(dbrepo.NewRestoreStateRepository(pool), a.workGate(), checks,
		a.lg.WithComponent("restore").Logger)
	return a.restoreSvc
}

// RestoreGate returns the restored-database startup gate.
func (a *App) RestoreGate() *service.RestoreGateService { return a.restoreGate() }

// startRestoreGate runs the restore checks when the database was restored
// and installs the read-only guard and readiness check. It reports whether
// the gate holds the instance (the scheduler must then stay off).
func (a *App) startRestoreGate(ctx context.Context) (bool, error) {
	if a.schemaVersion < restoreGateSchema {
		return false, nil
	}
	g := a.restoreGate()
	held, err := g.Startup(ctx)
	if err != nil {
		return false, fmt.Errorf("restore gate: %w", err)
	}
	if a.router != nil {
		a.router.SetWriteGuard(g.WriteGuard)
		a.router.RegisterReadinessCheck("restore", func(context.Context) (bool, error) { return g.Ready(), nil })
	}
	return held, nil
}

// preMigrationBackup takes the automatic consistent backup before pending
// migrations run (WBS 18.140, decision U5): always in production (backup.dir
// required), otherwise only when backup.dir is configured. The manifest
// records the pre-upgrade schema version. It returns the backup directory
// ("" when skipped).
func (a *App) preMigrationBackup(ctx context.Context, pool *pgxpool.Pool, from int) (string, error) {
	if a.cfg.Backup.Dir == "" {
		if a.cfg.IsProduction() {
			return "", errors.New("migrations are pending and backup.dir is not configured (production takes a backup before migrating)")
		}
		return "", nil
	}
	dir := filepath.Join(a.cfg.Backup.Dir, fmt.Sprintf("pre-migration-v%d-%s", from, time.Now().UTC().Format("20060102T150405.000000000Z")))
	m, err := backup.Backup(ctx, pool, dir, backup.Options{AppVersion: Version, Profile: a.cfg.EffectiveProfile(), Reason: "pre_migration"})
	if err != nil {
		return "", err
	}
	a.lg.WithComponent("migrations").Info("pre-migration backup taken", "dir", dir, "schema_version", m.SchemaVersion,
		"backup_id", m.BackupID.String(), "tables", len(m.Tables))
	return dir, nil
}

// newMetricsRegistry registers every overseer metric (WBS 18.113): the
// durable series (recomputed from the database at scrape time) and the
// process-local ones. pool may be nil (catalog only).
func newMetricsRegistry(pool *pgxpool.Pool) (*metrics.Registry, *dbrepo.DurableMetricsCollector) {
	reg := metrics.NewRegistry()
	durable := dbrepo.NewDurableMetricsCollector(pool, reg)
	reg.Counter(httpRequestsDesc)
	reg.Gauge(poolConnectionsDesc)
	reg.Gauge(alertActiveDesc)
	return reg, durable
}

var (
	httpRequestsDesc = metrics.Desc{Name: "overseer_http_requests_total", Help: "HTTP requests served by this process, by method and status class.",
		Category: "http", Labels: []string{"method", "class"},
		Values: map[string][]string{"method": {"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}, "class": {"1xx", "2xx", "3xx", "4xx", "5xx"}}}
	alertActiveDesc = metrics.Desc{Name: "overseer_alert_active", Help: "1 while an alert condition is active (WBS 18.118).",
		Category: "recovery", Labels: []string{"alert"}, Values: map[string][]string{"alert": alertNames()}}
	poolConnectionsDesc = metrics.Desc{Name: "overseer_database_pool_connections", Help: "Database pool connections of this process by state.",
		Category: "database", Labels: []string{"state"}, Values: map[string][]string{"state": {"idle", "in_use", "max"}}}
)

// MetricsCatalog returns the documented metric catalog (docs/metrics.md).
func MetricsCatalog() *metrics.Registry {
	reg, _ := newMetricsRegistry(nil)
	return reg
}

// metricsExporter builds the /metrics exporter and installs the HTTP
// request observer.
func (a *App) metricsExporter() *service.MetricsExporter {
	if a.metricsExp != nil {
		return a.metricsExp
	}
	pool := a.db.Raw()
	reg, durable := newMetricsRegistry(pool)
	httpReqs := reg.Counter(httpRequestsDesc)
	conns := reg.Gauge(poolConnectionsDesc)
	if a.router != nil {
		a.router.SetRequestObserver(func(method string, status int) {
			httpReqs.Inc(method, fmt.Sprintf("%dxx", status/100))
		})
	}
	a.metricsExp = service.NewMetricsExporter(reg, durable.Collect, func(context.Context, *metrics.Registry) error {
		st := pool.Stat()
		conns.Set(float64(st.IdleConns()), "idle")
		conns.Set(float64(st.AcquiredConns()), "in_use")
		conns.Set(float64(st.MaxConns()), "max")
		return nil
	})
	return a.metricsExp
}

// Metrics returns the metrics exporter.
func (a *App) Metrics() *service.MetricsExporter { return a.metricsExporter() }

// systemHealth builds the per-subsystem health service (WBS 18.117).
func (a *App) systemHealth() *service.SystemHealthService {
	if a.healthSvc != nil {
		return a.healthSvc
	}
	checks := []service.SubsystemCheck{
		func(ctx context.Context) health.Subsystem {
			s := health.Subsystem{Name: "database", Status: health.Healthy, StartupCritical: true}
			if ok, err := a.db.Ping(ctx); err != nil || !ok {
				s.Status, s.Detail = health.Unavailable, "unreachable"
			}
			return s
		},
		func(context.Context) health.Subsystem {
			s := health.Subsystem{Name: "repository_roots", Status: health.Healthy}
			var roots []string
			for _, p := range a.cfg.WorkerProviders {
				roots = append(roots, p.AllowedRoots...)
			}
			if len(roots) == 0 {
				s.Detail = "no allowed roots configured (unconfined)"
				return s
			}
			for _, r := range roots {
				if fi, err := os.Stat(r); err != nil || !fi.IsDir() {
					s.Status, s.Detail = health.Degraded, "allowed root unavailable: "+r
					return s
				}
			}
			return s
		},
		func(context.Context) health.Subsystem {
			s := health.Subsystem{Name: "scheduler", Status: health.Healthy}
			switch {
			case a.workGate().Blocked() != "":
				s.Status, s.Detail = health.Degraded, "new work blocked: "+a.workGate().Blocked()
			case a.cfg.Engine.SchedulerDisabled:
				// An operator choice, not a fault.
				s.Detail = "scheduler disabled by configuration"
			}
			return s
		},
		func(context.Context) health.Subsystem {
			s := health.Subsystem{Name: "audit", Status: health.Healthy}
			v := a.auditVerifier().Last()
			switch {
			case v == nil:
				s.Detail = "not verified yet"
			case v.BlocksNewWork:
				s.Status, s.Detail = health.Unavailable, "audit verification "+string(v.Status)+" (unacknowledged)"
			case v.Status != "VALID":
				s.Status, s.Detail = health.Degraded, "audit verification "+string(v.Status)+" (acknowledged)"
			}
			return s
		},
	}
	for _, pc := range a.providerChecks() {
		if pc.Kind == "database" {
			continue
		}
		pc := pc
		checks = append(checks, func(ctx context.Context) health.Subsystem {
			s := health.Subsystem{Name: pc.Kind + ":" + pc.Name, Status: health.Healthy}
			if pc.Check != nil {
				if err := pc.Check(ctx); err != nil {
					s.Status, s.Detail = health.Unavailable, "unreachable"
				}
			}
			return s
		})
	}
	a.healthSvc = service.NewSystemHealthService(checks...)
	return a.healthSvc
}

// SystemHealth returns the per-subsystem health service.
func (a *App) SystemHealth() *service.SystemHealthService { return a.systemHealth() }

func alertNames() []string {
	var out []string
	for _, id := range health.AlertIDs() {
		out = append(out, string(id))
	}
	return out
}

// alerts builds the alert evaluator (WBS 18.118–18.120, decision U10).
func (a *App) alerts() *service.AlertService {
	if a.alertSvc != nil {
		return a.alertSvc
	}
	gauge := a.metricsExporter().Registry().Gauge(alertActiveDesc)
	gcSvc := a.gc()
	defer func() {
		a.alertSvc.Contribute(func(_ context.Context, f *health.AlertFacts) { f.GCFailed = gcSvc.Failed() })
	}()
	a.alertSvc = service.NewAlertService(dbrepo.NewAlertFactsRepository(a.db.Raw()), a.systemHealth(), a.auditVerifier(),
		service.AlertConfig{StallThreshold: a.cfg.Engine.StallThreshold, OnActive: func(id health.AlertID, active bool) {
			v := 0.0
			if active {
				v = 1
			}
			gauge.Set(v, string(id))
		}}, a.lg.WithComponent("alerts").Logger)
	return a.alertSvc
}

// Alerts returns the alert evaluator.
func (a *App) Alerts() *service.AlertService { return a.alerts() }

// integrityMonitor builds the periodic background integrity scan (WBS
// 18.180; decision: every 15 min, ≤ 50 executions per tick).
func (a *App) integrityMonitor() *service.IntegrityMonitor {
	if a.integrityMon == nil {
		a.integrityMon = service.NewIntegrityMonitor(dbrepo.NewIntegrityScanner(a.db.Raw()).ExecutionsAfter, a.engine.CheckInvariants,
			service.IntegrityMonitorConfig{Interval: a.cfg.Engine.IntegrityScanInterval, Batch: a.cfg.Engine.IntegrityScanBatch},
			a.lg.WithComponent("integrity").Logger)
		a.alerts().Contribute(a.integrityMon.Contribute)
	}
	return a.integrityMon
}

// IntegrityMonitor returns the periodic integrity scan.
func (a *App) IntegrityMonitor() *service.IntegrityMonitor { return a.integrityMonitor() }

// diskMonitor builds the disk usage monitor (WBS 18.97, decision U7).
func (a *App) diskMonitor() *service.DiskMonitor {
	if a.diskMon != nil {
		return a.diskMon
	}
	pool := a.db.Raw()
	paths := func(ctx context.Context) []string {
		out := []string{os.TempDir()}
		if a.cfg.Backup.Dir != "" {
			out = append(out, a.cfg.Backup.Dir)
		}
		for _, p := range a.cfg.WorkerProviders {
			out = append(out, p.AllowedRoots...)
		}
		rows, err := pool.Query(ctx, `SELECT DISTINCT s.canonical::jsonb->>'working_directory' FROM execution_configuration_snapshots s
			JOIN executions e ON e.id = s.execution_id WHERE e.status IN ('RUNNING', 'WAITING', 'PAUSED', 'RECOVERY_REQUIRED')`)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var d *string
			if rows.Scan(&d) == nil && d != nil && *d != "" {
				out = append(out, *d)
			}
		}
		return out
	}
	warn, critical, interval := a.cfg.Disk.Effective()
	a.diskMon = service.NewDiskMonitor(paths, a.workGate(), warn, critical, interval, a.lg.WithComponent("disk").Logger)
	a.systemHealth().Add(a.diskMon.HealthCheck)
	a.alerts().Contribute(a.diskMon.Contribute)
	return a.diskMon
}

// DiskMonitor returns the disk usage monitor.
func (a *App) DiskMonitor() *service.DiskMonitor { return a.diskMonitor() }

// gc builds the garbage collector (WBS 18.99–18.104, decisions U1 / U2).
func (a *App) gc() *service.GCService {
	if a.gcSvc == nil {
		a.gcSvc = service.NewGCService(dbrepo.NewGCRepository(a.db.Raw()), service.GCConfig{CacheRetention: a.cfg.GC.CacheRetention,
			PrecomputeRetention: a.cfg.GC.PrecomputeRetention, ValidationLogRetention: a.cfg.GC.ValidationLogRetention},
			a.lg.WithComponent("gc").Logger)
	}
	return a.gcSvc
}

// GC returns the garbage collector.
func (a *App) GC() *service.GCService { return a.gc() }

// productionReadiness builds the production readiness report (WBS
// 18.207–18.209). Details name settings and resources only (18.208).
func (a *App) productionReadiness() *service.ProductionReadinessService {
	if a.readinessSvc != nil {
		return a.readinessSvc
	}
	pool := a.db.Raw()
	item := func(area, name, status, detail string) service.ReadinessItem {
		return service.ReadinessItem{Area: area, Name: name, Status: status, Detail: detail}
	}
	pass, warn, fail := service.CheckPass, service.CheckWarn, service.CheckFail
	checks := []service.ProductionCheck{
		func(ctx context.Context) []service.ReadinessItem {
			out := []service.ReadinessItem{}
			if ok, err := a.db.Ping(ctx); err != nil || !ok {
				return append(out, item("database", "reachable", fail, "PostgreSQL unreachable"))
			}
			out = append(out, item("database", "reachable", pass, ""))
			if v, err := database.CheckServerVersion(ctx, pool); err != nil {
				out = append(out, item("database", "server_version", fail, err.Error()))
			} else {
				out = append(out, item("database", "server_version", pass, fmt.Sprintf("server_version_num %d (≥ %d)", v, database.MinServerVersionNum)))
			}
			cur, err := database.CurrentVersion(ctx, pool)
			switch {
			case err != nil:
				out = append(out, item("migrations", "schema", fail, "schema version unreadable"))
			case a.appSchema > 0 && cur != a.appSchema:
				out = append(out, item("migrations", "schema", fail, fmt.Sprintf("schema %d, application %d", cur, a.appSchema)))
			default:
				out = append(out, item("migrations", "schema", pass, fmt.Sprintf("schema %d", cur)))
			}
			var last, verified *time.Time
			if err := pool.QueryRow(ctx, `SELECT max(created_at), max(restore_verified_at) FROM backup_records`).Scan(&last, &verified); err != nil {
				out = append(out, item("backup", "recent_backup", fail, "backup records unreadable"))
				return out
			}
			switch {
			case last == nil:
				out = append(out, item("backup", "recent_backup", fail, "no backup recorded (overseer backup --dir …)"))
			case time.Since(*last) > 24*time.Hour:
				out = append(out, item("backup", "recent_backup", warn, "latest backup older than 24h ("+last.UTC().Format(time.RFC3339)+")"))
			default:
				out = append(out, item("backup", "recent_backup", pass, "latest backup "+last.UTC().Format(time.RFC3339)))
			}
			if verified == nil || time.Since(*verified) > 30*24*time.Hour {
				out = append(out, item("backup", "restore_verified", warn, "no restore verified in the last 30 days"))
			} else {
				out = append(out, item("backup", "restore_verified", pass, ""))
			}
			var recovering int
			_ = pool.QueryRow(ctx, `SELECT count(*) FROM executions WHERE status = 'RECOVERY_REQUIRED'`).Scan(&recovering)
			if recovering > 0 {
				out = append(out, item("recovery", "executions_in_recovery", warn, fmt.Sprintf("%d execution(s) require recovery", recovering)))
			} else {
				out = append(out, item("recovery", "executions_in_recovery", pass, ""))
			}
			return out
		},
		func(context.Context) []service.ReadinessItem {
			out := []service.ReadinessItem{}
			if a.cfg.IsProduction() {
				out = append(out, item("security", "profile", pass, "production"))
			} else {
				out = append(out, item("security", "profile", fail, "profile is "+a.cfg.EffectiveProfile()+", not production"))
			}
			for _, e := range a.cfg.ProductionErrors() {
				out = append(out, item("security", "configuration", fail, e.Error()))
			}
			if len(a.cfg.WorkerProviders) == 0 {
				out = append(out, item("workers", "configured", fail, "no worker provider configured"))
			}
			if a.cfg.Engine.MaxConcurrentDispatches == 0 {
				out = append(out, item("limits", "max_concurrent_dispatches", warn, "unlimited (engine.max_concurrent_dispatches)"))
			} else {
				out = append(out, item("limits", "max_concurrent_dispatches", pass, ""))
			}
			if a.restoreSvc != nil && a.restoreSvc.Active() {
				out = append(out, item("restore", "restore_gate", fail, "restored database not enabled yet"))
			}
			v := a.auditVerifier().Last()
			switch {
			case v == nil:
				out = append(out, item("audit", "chain", warn, "not verified yet"))
			case v.BlocksNewWork:
				out = append(out, item("audit", "chain", fail, "verification "+string(v.Status)+" (unacknowledged)"))
			case v.Status != "VALID":
				out = append(out, item("audit", "chain", warn, "verification "+string(v.Status)+" (acknowledged)"))
			default:
				out = append(out, item("audit", "chain", pass, "VALID"))
			}
			return out
		},
		func(ctx context.Context) []service.ReadinessItem {
			out := []service.ReadinessItem{}
			rep := a.systemHealth().Report(ctx)
			for _, s := range rep.Subsystems {
				area := "health"
				switch {
				case strings.HasPrefix(s.Name, "worker:"):
					area = "workers"
				case strings.HasPrefix(s.Name, "compression:"):
					area = "compression"
				case s.Name == "repository_roots":
					area = "repository"
				}
				st := pass
				switch {
				case s.Status == health.Unavailable && (s.StartupCritical || s.Name == "repository_roots" || s.Name == "disk" || s.Name == "audit"):
					st = fail
				case s.Status != health.Healthy:
					st = warn
				}
				out = append(out, item(area, s.Name, st, s.Detail))
			}
			return out
		},
	}
	a.readinessSvc = service.NewProductionReadinessService(checks...)
	return a.readinessSvc
}

// ProductionReadiness returns the production readiness service.
func (a *App) ProductionReadiness() *service.ProductionReadinessService {
	return a.productionReadiness()
}
