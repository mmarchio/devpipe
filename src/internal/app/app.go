package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"overseer/internal/access"
	"overseer/internal/backup"
	"overseer/internal/compressionprovider"
	"overseer/internal/config"
	"overseer/internal/database"
	"overseer/internal/dbrepo"
	"overseer/internal/faults"
	"overseer/internal/gateway/hermes"
	"overseer/internal/logging"
	"overseer/internal/orchestration"
	"overseer/internal/precompute"
	"overseer/internal/service"
	"overseer/internal/valrun"
	"overseer/internal/web"
	"overseer/internal/worker"

	"sync/atomic"

	"github.com/google/uuid"

	"overseer/internal/project"
)

// Options tune bootstrap behavior.
type Options struct {
	// MigrationsDir is the directory containing versioned migrations.
	// Migrations run automatically at startup unless DisableMigrations.
	MigrationsDir string
	// DisableMigrations skips automatic migration execution.
	DisableMigrations bool
	// TemplatesDir / StaticDir for the web layer.
	TemplatesDir string
	StaticDir    string
	// LogWriter receives the structured log (default os.Stderr; tests
	// capture it to scan for secrets).
	LogWriter io.Writer
}

// DefaultOptions returns the repository-standard layout.
func DefaultOptions() Options {
	return Options{
		MigrationsDir: "migrations",
	}
}

// App is the application lifecycle: it owns every component in startup
// order and tears them down in reverse order.
type App struct {
	// Phase 18: instance identity, startup configuration snapshot and the
	// schema version the process runs against.
	instanceID     string
	secrets        *secretSource
	integritySvc   *service.IntegrityService
	gate           *service.WorkGate
	auditVerify    *service.AuditVerificationService
	restoreSvc     *service.RestoreGateService
	metricsExp     *service.MetricsExporter
	healthSvc      *service.SystemHealthService
	alertSvc       *service.AlertService
	integrityMon   *service.IntegrityMonitor
	diskMon        *service.DiskMonitor
	gcSvc          *service.GCService
	readinessSvc   *service.ProductionReadinessService
	appSchema      int
	bgCancel       context.CancelFunc
	bgDone         sync.WaitGroup
	configSnapshot config.Snapshot
	schemaVersion  int

	cfg    config.Config
	opts   Options
	lg     *logging.Logger
	db     *database.Pool
	srv    *web.Server
	router *web.Router
	ready  atomic.Bool

	compressionSvc *service.CompressionService
	promptCacheSvc *service.PromptCacheService
	precomputeSvc  *service.PrecomputeService
	workerSvc      *service.WorkerService
	resultSvc      *service.ResultService
	contSvc        *service.ContinuationService
	engine         *service.Engine
	lifecycleSvc   *service.LifecycleService
	bootstrapSvc   *service.BootstrapService

	// providerTestsOK holds the kind/provider/model keys whose latest
	// console connection test succeeded (readiness session checks pass).
	providerTestsOK sync.Map

	// Phase 17 console.
	redactor  *access.Redactor
	auditSvc  *service.AuditService
	feed      *service.LiveEventFeed
	console   *web.ConsoleAPI
	startedAt time.Time
}

// Console returns the Phase 17 console services; nil before Start.
func (a *App) Console() *web.ConsoleAPI { return a.console }

// Router returns the HTTP router (tests mount it on httptest servers).
func (a *App) Router() *web.Router { return a.router }

// Bootstrap returns the execution bootstrap service (Phase 15); nil before
// Start.
func (a *App) Bootstrap() *service.BootstrapService { return a.bootstrapSvc }

// Continuations returns the continuation service (Phase 14); nil before
// Start.
func (a *App) Continuations() *service.ContinuationService { return a.contSvc }

// Lifecycle returns the project lifecycle service; nil before Start.
func (a *App) Lifecycle() *service.LifecycleService { return a.lifecycleSvc }

// Engine returns the Phase 16 execution engine; nil before Start.
func (a *App) Engine() *service.Engine { return a.engine }

// Results returns the worker result service (Phase 13); nil before Start.
func (a *App) Results() *service.ResultService { return a.resultSvc }

// Worker returns the worker service (Phase 12); nil before Start.
func (a *App) Worker() *service.WorkerService { return a.workerSvc }

// Precompute returns the precompute service (Phase 11); nil before Start.
func (a *App) Precompute() *service.PrecomputeService { return a.precomputeSvc }

// PromptCache returns the prompt-cache service (Phase 10); nil before Start.
func (a *App) PromptCache() *service.PromptCacheService { return a.promptCacheSvc }

// Compression returns the prompt-compression service (Phase 9); nil before
// Start.
func (a *App) Compression() *service.CompressionService { return a.compressionSvc }

// New builds an App from validated configuration with the standard layout.
func New(cfg config.Config) *App {
	return &App{cfg: cfg, opts: resolveOptions(DefaultOptions()), secrets: newSecretSource()}
}

// NewWith builds an App with explicit bootstrap options.
func NewWith(cfg config.Config, opts Options) *App {
	return &App{cfg: cfg, opts: resolveOptions(opts), secrets: newSecretSource()}
}

// resolveOptions anchors relative directories at the module root so the
// app works regardless of the process working directory.
func resolveOptions(opts Options) Options {
	root, err := moduleRoot()
	if err != nil {
		return opts
	}
	if opts.MigrationsDir != "" && !filepath.IsAbs(opts.MigrationsDir) {
		opts.MigrationsDir = filepath.Join(root, opts.MigrationsDir)
	}
	if opts.TemplatesDir != "" && !filepath.IsAbs(opts.TemplatesDir) {
		opts.TemplatesDir = filepath.Join(root, opts.TemplatesDir)
	}
	if opts.StaticDir != "" && !filepath.IsAbs(opts.StaticDir) {
		opts.StaticDir = filepath.Join(root, opts.StaticDir)
	}
	return opts
}

func moduleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for d := wd; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		if parent := filepath.Dir(d); parent == d {
			return "", os.ErrNotExist
		}
	}
}

// Config returns the configuration the application runs with.
func (a *App) Config() config.Config { return a.cfg }

// IsReady reports whether all components initialized (readiness gate).
func (a *App) IsReady() bool { return a.ready.Load() }

// DB exposes the initialized pool (nil before a successful Start). Used by
// lifecycle tests to verify cleanup.
func (a *App) DB() *database.Pool { return a.db }

// Start initializes components in WBS 1.6 order:
// logging -> database -> migrations -> web (renderer/router/server) -> READY.
// It returns on the first failure and closes every component initialized
// before it (T1.6.3). It returns once READY; the server keeps serving in a
// background goroutine until Stop is called.
func (a *App) Start(ctx context.Context) error {
	// 0. Fault injection (WBS 18.141): only the test profile accepts an
	// OVERSEER_FAULTS spec (real-process forced-termination tests).
	if a.cfg.Faults != "" {
		if a.cfg.EffectiveProfile() != config.ProfileTest {
			return fmt.Errorf("app: fault injection requires the test profile")
		}
		actions, err := faults.Parse(a.cfg.Faults)
		if err != nil {
			return fmt.Errorf("app: %w", err)
		}
		reg := faults.NewRegistry()
		for _, act := range actions {
			reg.Arm(act)
		}
		// "exit" terminates this process at the point (forced termination).
		reg.SetExit(os.Exit)
		faults.Enable(reg)
	}
	// 1. Logging (config already validated by the caller).
	// Every configured secret is resolved once up front (and thereby
	// remembered), so the log redacts them from the first record on
	// (WBS 18.75); rotated values are remembered as they are read.
	static := a.secretValues()
	logOut := a.opts.LogWriter
	if logOut == nil {
		logOut = os.Stderr
	}
	lg, err := logging.NewRedacting(a.cfg.Logging, logOut, func() []string { return append(a.secrets.Values(), static...) })
	if err != nil {
		return fmt.Errorf("app: init logging: %w", err)
	}
	a.lg = lg
	logging.LogStartup(lg, a.cfg)

	// 2. Database.
	maxC, minC, life, idle, appName := a.cfg.Database.PoolSettings()
	db, err := database.NewWithOptions(ctx, a.cfg.DSN(), lg, database.PoolOptions{MaxConns: maxC, MinConns: minC,
		MaxConnLifetime: life, MaxConnIdleTime: idle, ApplicationName: appName})
	if err != nil {
		return fmt.Errorf("app: init database: %w", err)
	}
	a.db = db
	// Supported PostgreSQL: 17 or newer (the schema relies on it).
	if _, err := database.CheckServerVersion(ctx, db.Raw()); err != nil {
		db.Close()
		return fmt.Errorf("app: %w", err)
	}

	// 3. Migrations (automatic unless disabled; failure stops startup).
	if a.opts.MigrationsDir != "" {
		migs, err := database.LoadMigrations(a.opts.MigrationsDir)
		switch {
		case err != nil && !a.opts.DisableMigrations:
			db.Close()
			return fmt.Errorf("app: load migrations: %w", err)
		case err == nil:
			// Schema compatibility (WBS 18.130 / 18.134): never run against
			// a newer schema; an older one is migrated (or refused when
			// migrations are disabled).
			a.appSchema = database.MaxVersion(migs)
			dbVersion, err := database.CheckCompatibility(ctx, db.Raw(), migs, !a.opts.DisableMigrations)
			if err != nil {
				db.Close()
				return fmt.Errorf("app: %w", err)
			}
			var preMigration string
			if !a.opts.DisableMigrations && dbVersion > 0 && dbVersion < database.MaxVersion(migs) {
				// Backup before migrating (WBS 18.140, decision U5); a
				// failure stops startup with the schema untouched.
				if preMigration, err = a.preMigrationBackup(ctx, db.Raw(), dbVersion); err != nil {
					db.Close()
					return fmt.Errorf("app: pre-migration backup: %w", err)
				}
			}
			if !a.opts.DisableMigrations {
				if pending, _ := database.PendingProgress(ctx, db.Raw()); len(pending) > 0 {
					for _, p := range pending {
						lg.WithComponent("migrations").Warn("resuming interrupted non-transactional migration", "version", p.Version,
							"name", p.Name, "steps_done", p.StepsDone, "steps_total", p.StepsTotal, "status", p.Status, "last_error", p.LastError)
					}
				}
				if err := database.Migrate(ctx, db.Raw(), migs); err != nil {
					db.Close()
					return fmt.Errorf("app: migrate: %w", err)
				}
				if preMigration != "" {
					if err := backup.RecordManifest(ctx, db.Raw(), preMigration); err != nil {
						db.Close()
						return fmt.Errorf("app: record pre-migration backup: %w", err)
					}
				}
			}
		}
	}

	// 3a. Configuration snapshot (WBS 18.3 / 18.4 / 18.206).
	if err := a.recordConfigSnapshot(ctx); err != nil {
		db.Close()
		return fmt.Errorf("app: record configuration snapshot: %w", err)
	}

	// 3b. Role defaults (WBS 5.2): seed the five GLOBAL default roles as
	// data, idempotently (a restart re-runs this without duplicating).
	roleSvc := service.NewRoleService(
		dbrepo.NewRoleRepository(db.Raw()),
		dbrepo.NewSystemPromptRepository(db.Raw()),
	).WithInstructions(dbrepo.NewInstructionRepository(db.Raw())).
		WithAssignments(dbrepo.NewRoleInstructionAssignmentRepository(db.Raw())).
		WithInstructionVersions(dbrepo.NewInstructionVersionRepository(db.Raw())).
		WithStateFiles(dbrepo.NewRoleStateFileRepository(db.Raw())).
		WithAllowedStates(dbrepo.NewRoleAllowedStateRepository(db.Raw())).
		WithTxPool(db.Raw())
	if err := roleSvc.SeedDefaultRoles(ctx); err != nil {
		db.Close()
		return fmt.Errorf("app: seed role defaults: %w", err)
	}

	// 3c. State-file type defaults (WBS 7.2): seed the six GLOBAL default
	// state-file types as data, idempotently (a restart re-runs this
	// without duplicating).
	stateFileSvc := service.NewStateFileService(
		dbrepo.NewStateFileTypeRepository(db.Raw()),
		dbrepo.NewStateFileRepository(db.Raw()),
		dbrepo.NewStateFileVersionRepository(db.Raw()),
	).WithPhases(dbrepo.NewPhaseRepository(db.Raw())).
		WithUnits(dbrepo.NewWorkUnitRepository(db.Raw())).
		WithTxPool(db.Raw())
	if err := stateFileSvc.SeedDefaultFileTypes(ctx); err != nil {
		db.Close()
		return fmt.Errorf("app: seed state-file type defaults: %w", err)
	}

	// 3d. Global prompt templates (WBS 8.2) and the default workflow
	// template (WBS 6.3), seeded idempotently so a fresh installation can
	// take a project to READY (Phase 17 open decision → P18-20).
	if err := a.seedWorkflowDefaults(ctx); err != nil {
		db.Close()
		return fmt.Errorf("app: seed workflow defaults: %w", err)
	}

	// 4. Web: renderer, router, middleware, readiness wiring. The Phase 17
	// security chain (sessions, authentication, CSRF, limits) runs inside
	// the router; audit and the configuration-version guard wrap every
	// route registered below.
	a.redactor = access.NewRedactor(a.secretValues()...)
	router := web.NewRouter(a.routerConfig())
	a.router = router
	auditRepo := dbrepo.NewAuditRepository(db.Raw())
	// Link audit rows written before the hash chain existed (P18-56,
	// Decision 7). Appends link any remainder themselves, so a failure here
	// is logged, not fatal.
	if n, err := auditRepo.ChainPending(ctx); err != nil {
		lg.WithComponent("audit").Warn("audit chain backfill failed; appends will link pending rows", "error", err)
	} else if n > 0 {
		lg.WithComponent("audit").Info("audit chain backfilled", "events", n)
	}
	a.auditSvc = service.NewAuditService(auditRepo, dbrepo.NewProjectRepository(db.Raw()), a.redactor)
	router.SetAudit(web.AuditMiddleware(a.auditSvc, lg))
	router.SetConfigGuard(dbrepo.NewConfigVersionRepository(db.Raw()))
	if a.opts.TemplatesDir != "" {
		rnd, err := web.NewRenderer(a.opts.TemplatesDir)
		if err != nil {
			db.Close()
			return fmt.Errorf("app: init renderer: %w", err)
		}
		router.SetRenderer(rnd)
	}
	router.RegisterReadinessCheck("database", func(ctx context.Context) (bool, error) {
		return db.Ping(ctx)
	})
	// Schema compatibility (WBS 18.116): a schema newer than this binary
	// (another instance upgraded it) makes this instance not ready.
	router.RegisterReadinessCheck("schema", func(ctx context.Context) (bool, error) {
		if a.appSchema == 0 {
			return true, nil
		}
		v, err := database.CurrentVersion(ctx, db.Raw())
		if err != nil {
			return false, err
		}
		return v <= a.appSchema, nil
	})
	// Project service + REST API (WBS 3.14): the service uses the same
	// pool for atomic operations (P3-02).
	svc := service.NewProjectService(
		dbrepo.NewProjectRepository(db.Raw()),
		dbrepo.NewSettingsRepository(db.Raw()),
		db.Raw(),
	).WithDefaultContextThreshold(a.cfg.Projects.ContextThreshold)
	if err := web.RegisterProjectRoutes(router, svc); err != nil {
		db.Close()
		return fmt.Errorf("app: register project routes: %w", err)
	}
	// Role service + REST API (WBS 5.21): list/get/create/PATCH/DELETE.
	if err := web.RegisterRoleRoutes(router, roleSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register role routes: %w", err)
	}
	// Role system-prompt REST API (WBS 5.22): GET current / PUT new version /
	// GET versions, with expected-version conflict.
	if err := web.RegisterRolePromptRoutes(router, roleSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register role prompt routes: %w", err)
	}
	// Instruction CRUD REST API (WBS 5.23): list/create/get/PUT (versioned) /
	// DELETE (referenced protected).
	if err := web.RegisterInstructionRoutes(router, roleSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register instruction routes: %w", err)
	}
	// Role -> instruction assignment REST API (WBS 5.24): GET (ordered) / POST
	// (create, duplicate + cross-project rejected) / DELETE.
	if err := web.RegisterAssignmentRoutes(router, roleSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register assignment routes: %w", err)
	}
	// Allowed-state configuration REST API (WBS 5.25): PUT replaces the
	// role's allowed states atomically.
	if err := web.RegisterAllowedStateRoutes(router, roleSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register allowed-state routes: %w", err)
	}
	// State-file requirement REST API (WBS 5.26): PUT stores the role's
	// required state files (ordered); duplicates rejected.
	if err := web.RegisterStateFileRoutes(router, roleSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register state-file routes: %w", err)
	}
	// State-file CRUD REST API (WBS 7.38): project-scoped list, current
	// content, managed update, version-history reads. Reuses the P7-08
	// service (with the project repository for the scope check).
	if err := web.RegisterStateFileCRUDRoutes(router, stateFileSvc.WithProjects(dbrepo.NewProjectRepository(db.Raw()))); err != nil {
		db.Close()
		return fmt.Errorf("app: register state-file CRUD routes: %w", err)
	}
	// Context-preview + snapshot-read REST API (WBS 7.37/7.40): the
	// non-persisting context preview and the read-only historical snapshot
	// list/get. Composes the P7-13 scope-resolution + P7-18 role pipeline
	// (no parallel assembly path) and the P7-18 snapshot repository.
	rawPool := db.Raw()
	scopeResSvc := service.NewStateFileScopeResolutionService(
		service.NewStateFileRequirementResolutionService(
			dbrepo.NewRoleRepository(rawPool),
			dbrepo.NewRoleStateFileRepository(rawPool),
			dbrepo.NewStateMachineRepository(rawPool),
			dbrepo.NewWorkflowStateRepository(rawPool),
			dbrepo.NewStateFileRequirementRepository(rawPool),
		),
		dbrepo.NewStateFileTypeRepository(rawPool),
		dbrepo.NewStateFileRepository(rawPool),
		dbrepo.NewPhaseRepository(rawPool),
		dbrepo.NewPlanRepository(rawPool),
		dbrepo.NewWorkUnitRepository(rawPool),
	)
	snapshotRoleSvc := service.NewRoleService(
		dbrepo.NewRoleRepository(rawPool),
		dbrepo.NewSystemPromptRepository(rawPool),
	).WithInstructions(dbrepo.NewInstructionRepository(rawPool)).
		WithAssignments(dbrepo.NewRoleInstructionAssignmentRepository(rawPool)).
		WithInstructionVersions(dbrepo.NewInstructionVersionRepository(rawPool)).
		WithStateFiles(dbrepo.NewRoleStateFileRepository(rawPool)).
		WithAllowedStates(dbrepo.NewRoleAllowedStateRepository(rawPool)).
		WithTxPool(rawPool)
	contextPreviewSvc := service.NewContextPreviewService(
		dbrepo.NewRuntimeStateRepository(rawPool),
		dbrepo.NewWorkflowStateRepository(rawPool),
		dbrepo.NewStateRoleAssignmentRepository(rawPool),
		dbrepo.NewRoleRepository(rawPool),
		snapshotRoleSvc,
		scopeResSvc,
	).WithFileVersions(dbrepo.NewStateFileVersionRepository(rawPool)).
		WithSnapshots(dbrepo.NewContextSnapshotRepository(rawPool)).
		WithTxPool(rawPool)
	if err := web.RegisterContextSnapshotRoutes(router, contextPreviewSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register context-snapshot routes: %w", err)
	}
	// Prompt-preview REST API (WBS 8.35): renders the canonical prompt for
	// the project's current position without persisting an execution and
	// without calling an LLM. Reuses the P7-18 read pipeline (role + scope
	// + version capture) and the P8-38 composition core.
	promptPreviewSvc := service.NewPromptPreviewService(
		dbrepo.NewRuntimeStateRepository(rawPool),
		dbrepo.NewWorkflowStateRepository(rawPool),
		dbrepo.NewStateRoleAssignmentRepository(rawPool),
		dbrepo.NewRoleRepository(rawPool),
		snapshotRoleSvc,
		scopeResSvc,
		dbrepo.NewStateFileVersionRepository(rawPool),
		dbrepo.NewPromptTemplateVersionRepository(rawPool),
		dbrepo.NewPromptTemplateRepository(rawPool),
		dbrepo.NewResponseProtocolRepository(rawPool),
		dbrepo.NewWorkUnitRepository(rawPool),
		dbrepo.NewPlanRepository(rawPool),
	).WithTxPool(rawPool)
	if err := web.RegisterPromptPreviewRoutes(router, promptPreviewSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register prompt-preview routes: %w", err)
	}
	// P8-41 (WBS 8.36): prompt history read surface (list + by-id, filters,
	// project isolation).
	promptHistorySvc := service.NewPromptHistoryService(dbrepo.NewCanonicalPromptRepository(rawPool))
	if err := web.RegisterPromptHistoryRoutes(router, promptHistorySvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register prompt-history routes: %w", err)
	}
	// P8-42 (WBS 8.37): prompt provenance read surface (a prompt's source
	// components at their pinned versions/hashes).
	promptProvenanceSvc := service.NewPromptProvenanceService(
		dbrepo.NewCanonicalPromptRepository(rawPool),
		dbrepo.NewContextSnapshotRepository(rawPool),
		dbrepo.NewSystemPromptRepository(rawPool),
		dbrepo.NewResponseProtocolRepository(rawPool),
		dbrepo.NewWorkflowStateRepository(rawPool),
		rawPool,
	)
	if err := web.RegisterPromptProvenanceRoutes(router, promptProvenanceSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register prompt-provenance routes: %w", err)
	}
	// Phase 9 (WBS 9.35/9.45-9.48): prompt compression. The default
	// compression instruction is seeded idempotently; providers come from the
	// app configuration (credentials resolved from the environment at call
	// time). With no provider configured, preparation bypasses or falls back
	// to the canonical prompt — boot is unaffected.
	if _, err := dbrepo.NewCompressionInstructionRepository(rawPool).EnsureDefault(ctx); err != nil {
		db.Close()
		return fmt.Errorf("app: seed compression instruction: %w", err)
	}
	a.compressionSvc = service.NewCompressionService(
		dbrepo.NewCanonicalPromptRepository(rawPool),
		dbrepo.NewCanonicalPromptComponentRepository(rawPool),
		dbrepo.NewSettingsRepository(rawPool),
		dbrepo.NewCompressionSettingsRepository(rawPool),
		dbrepo.NewCompressionInstructionRepository(rawPool),
		dbrepo.NewCompressionRepository(rawPool),
		compressionprovider.NewFactory(a.cfg.CompressionProviders).WithResolver(a.secrets.Resolve).WithDialControl(endpointDialControl(a.cfg)),
	).WithTxPool(rawPool).WithAvailability(a.compressionAvailabilityConfig())
	if err := web.RegisterCompressionRoutes(router, a.compressionSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register compression routes: %w", err)
	}
	// Phase 10 (WBS 10.16): context-level prompt preparation through the
	// prompt cache. The P8-38 composer (with Phase 9 component persistence)
	// is wired here; PostgreSQL is the authoritative cache store.
	composerSvc := service.NewPromptComposerService(
		dbrepo.NewContextSnapshotRepository(rawPool),
		dbrepo.NewPromptTemplateVersionRepository(rawPool),
		dbrepo.NewWorkflowStateRepository(rawPool),
		dbrepo.NewResponseProtocolRepository(rawPool),
		dbrepo.NewStateInstructionRepository(rawPool),
		dbrepo.NewPromptTemplateRepository(rawPool),
		dbrepo.NewWorkUnitRepository(rawPool),
		dbrepo.NewPlanRepository(rawPool),
		dbrepo.NewCanonicalPromptRepository(rawPool),
	).WithTxPool(rawPool).
		WithWBSVersions(dbrepo.NewWBSVersionRepository(rawPool)).
		WithSettings(dbrepo.NewSettingsRepository(rawPool)).
		WithComponents(dbrepo.NewCanonicalPromptComponentRepository(rawPool))
	a.promptCacheSvc = service.NewPromptCacheService(
		composerSvc, a.compressionSvc,
		dbrepo.NewSettingsRepository(rawPool),
		dbrepo.NewPromptCacheRepository(rawPool),
		dbrepo.NewPromptCacheEventRepository(rawPool),
		dbrepo.NewPromptFragmentRepository(rawPool),
		dbrepo.NewPromptCacheSettingsRepository(rawPool),
		dbrepo.NewCanonicalPromptRepository(rawPool),
		dbrepo.NewCanonicalPromptComponentRepository(rawPool),
		dbrepo.NewCompressionRepository(rawPool),
	)
	if err := web.RegisterPromptCacheRoutes(router, a.promptCacheSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register prompt-cache routes: %w", err)
	}
	// Phase 11 (WBS 11.1-11.57): future-state prompt precomputation. It
	// coordinates the Phase 6-10 components and never performs workflow
	// actions; the precomputed artifacts are reused only through the
	// authoritative Phase 10 lookup.
	a.promptCacheSvc.WithPrecompute(dbrepo.NewPrecomputedTemplateRepository(rawPool), dbrepo.NewTransitionLatencyRepository(rawPool))
	precomputeSnapshotSvc := service.NewExecutionSnapshotService(
		dbrepo.NewRuntimeStateRepository(rawPool),
		dbrepo.NewWorkflowStateRepository(rawPool),
		dbrepo.NewStateRoleAssignmentRepository(rawPool),
		dbrepo.NewRoleRepository(rawPool),
		snapshotRoleSvc,
		scopeResSvc,
	).WithSnapshots(dbrepo.NewContextSnapshotRepository(rawPool)).
		WithFileVersions(dbrepo.NewStateFileVersionRepository(rawPool)).
		WithTxPool(rawPool)
	providers := a.cfg.CompressionProviders
	a.precomputeSvc = service.NewPrecomputeService(service.PrecomputeDeps{
		Settings:     dbrepo.NewPrecomputeSettingsRepository(rawPool),
		Jobs:         dbrepo.NewPrecomputeJobRepository(rawPool),
		Projections:  dbrepo.NewProjectionRepository(rawPool),
		Manifests:    dbrepo.NewFragmentManifestRepository(rawPool),
		Templates:    dbrepo.NewPrecomputedTemplateRepository(rawPool),
		Latencies:    dbrepo.NewTransitionLatencyRepository(rawPool),
		Events:       dbrepo.NewPromptCacheEventRepository(rawPool),
		Fragments:    dbrepo.NewPromptFragmentRepository(rawPool),
		CacheEntries: dbrepo.NewPromptCacheRepository(rawPool),
		Projects:     dbrepo.NewProjectRepository(rawPool),
		Runtime:      dbrepo.NewRuntimeStateRepository(rawPool),
		States:       dbrepo.NewWorkflowStateRepository(rawPool),
		Transitions:  dbrepo.NewStateTransitionRepository(rawPool),
		PromptTmpl:   dbrepo.NewPromptTemplateRepository(rawPool),
		TmplVer:      dbrepo.NewPromptTemplateVersionRepository(rawPool),
		Snapshots:    dbrepo.NewContextSnapshotRepository(rawPool),
		FileVersion:  dbrepo.NewStateFileVersionRepository(rawPool),
		WBSVersions:  dbrepo.NewWBSVersionRepository(rawPool),
		Phases:       dbrepo.NewPhaseRepository(rawPool),
		Plans:        dbrepo.NewPlanRepository(rawPool),
		Units:        dbrepo.NewWorkUnitRepository(rawPool),
		SnapshotSvc:  precomputeSnapshotSvc,
		ScopeRes:     scopeResSvc,
		ActiveTask: service.NewActiveTaskService(stateFileSvc, dbrepo.NewPhaseRepository(rawPool),
			dbrepo.NewPlanRepository(rawPool), dbrepo.NewWorkUnitRepository(rawPool)),
		Composer:    composerSvc,
		Cache:       a.promptCacheSvc,
		Compression: a.compressionSvc,
		ProviderLocal: func(name string) bool {
			p, ok := providers[name]
			return ok && precompute.IsLocalURL(p.BaseURL)
		},
	})
	if _, err := a.precomputeSvc.Recover(ctx); err != nil {
		db.Close()
		return fmt.Errorf("app: recover precompute jobs: %w", err)
	}
	if err := web.RegisterPrecomputeRoutes(router, a.precomputeSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register precompute routes: %w", err)
	}
	// Worker transport + sessions (Phase 12). Only this composition root
	// knows the concrete Hermes adapter (WBS 12.90).
	workerRegistry := worker.NewProviderRegistry()
	hermes.Register(workerRegistry, a.secrets.Resolve, hermes.WithDialControl(endpointDialControl(a.cfg)))
	workerProviders := a.cfg.WorkerProviders
	a.workerSvc = service.NewWorkerService(service.WorkerDeps{
		Factory: workerRegistry,
		Providers: func(name string) (worker.WorkerProvider, json.RawMessage, time.Duration, bool) {
			p, ok := workerProviders[name]
			if !ok || p.Kind != config.WorkerKindHermesGateway {
				return "", nil, 0, false
			}
			return worker.ProviderHermesGateway, hermesProviderConfig(p), p.SessionTimeout, true
		},
		ProjectSettings: dbrepo.NewSettingsRepository(rawPool),
		Settings:        dbrepo.NewWorkerSettingsRepository(rawPool),
		Sessions:        dbrepo.NewWorkerSessionRepository(rawPool),
		Dispatches:      dbrepo.NewWorkerDispatchRepository(rawPool),
		Responses:       dbrepo.NewWorkerResponseRepository(rawPool),
		Leases:          dbrepo.NewWorkerLeaseRepository(rawPool),
		Events:          dbrepo.NewWorkerEventRepository(rawPool),
		Prompts:         dbrepo.NewCompressionRepository(rawPool),
		Activity:        a.precomputeSvc,
		Logger:          a.lg.WithComponent("worker").Logger,
	}).WithAvailability(a.availabilityConfig())
	// Worker result protocol (Phase 13): parse stored raw responses.
	protoRepo := dbrepo.NewResponseProtocolRepository(rawPool)
	a.resultSvc = service.NewResultService(service.ResultDeps{
		Settings:   dbrepo.NewResultProtocolSettingsRepository(rawPool),
		Results:    dbrepo.NewWorkerResultRepository(rawPool),
		Attempts:   dbrepo.NewParseAttemptRepository(rawPool),
		Dispatches: dbrepo.NewWorkerDispatchRepository(rawPool),
		Responses:  dbrepo.NewWorkerResponseRepository(rawPool),
		Sessions:   dbrepo.NewWorkerSessionRepository(rawPool),
		Units:      dbrepo.NewWorkUnitRepository(rawPool),
		Protocols:  protoRepo,
		Lookup:     protoRepo,
		Events:     dbrepo.NewWorkerEventRepository(rawPool),
		Projects:   dbrepo.NewProjectRepository(rawPool),
		Logger:     a.lg.WithComponent("worker_result").Logger,
	})
	a.workerSvc.WithCompletionObserver(a.resultSvc)
	// Context & continuation controller (Phase 14): consumes Phase 13
	// results and Phase 12 failures, drives continuation through the
	// managed runner.
	a.contSvc = service.NewContinuationService(service.ContinuationDeps{
		Repo:            dbrepo.NewContinuationRepository(rawPool),
		Settings:        dbrepo.NewContinuationSettingsRepository(rawPool),
		ProjectSettings: dbrepo.NewSettingsRepository(rawPool),
		Projects:        dbrepo.NewProjectRepository(rawPool),
		Runtime:         dbrepo.NewRuntimeStateRepository(rawPool),
		Snapshots:       dbrepo.NewContextSnapshotRepository(rawPool),
		SnapshotSvc:     precomputeSnapshotSvc,
		Canonical:       dbrepo.NewCanonicalPromptRepository(rawPool),
		Prompts:         dbrepo.NewCompressionRepository(rawPool),
		Templates:       dbrepo.NewPromptTemplateRepository(rawPool),
		TemplateVers:    dbrepo.NewPromptTemplateVersionRepository(rawPool),
		Units:           dbrepo.NewWorkUnitRepository(rawPool),
		Dispatches:      dbrepo.NewWorkerDispatchRepository(rawPool),
		Sessions:        dbrepo.NewWorkerSessionRepository(rawPool),
		Responses:       dbrepo.NewWorkerResponseRepository(rawPool),
		Attempts:        dbrepo.NewParseAttemptRepository(rawPool),
		Events:          dbrepo.NewWorkerEventRepository(rawPool),
		Cache:           a.promptCacheSvc,
		Worker:          a.workerSvc,
		Results:         a.resultSvc,
		Owner:           a.workerSvc.Owner(),
		Logger:          a.lg.WithComponent("continuation").Logger,
	})
	a.resultSvc.WithOutcomeObserver(a.contSvc)
	a.workerSvc.WithFailureObserver(a.contSvc)
	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	if _, err := a.workerSvc.Recover(rctx); err != nil {
		rcancel()
		db.Close()
		return fmt.Errorf("app: recover worker sessions: %w", err)
	}
	if _, err := a.resultSvc.RecoverUnparsed(rctx); err != nil {
		rcancel()
		db.Close()
		return fmt.Errorf("app: recover unparsed worker responses: %w", err)
	}
	if _, err := a.contSvc.Recover(rctx); err != nil {
		rcancel()
		db.Close()
		return fmt.Errorf("app: recover continuations: %w", err)
	}
	rcancel()
	if !a.cfg.Continuation.Disabled {
		a.contSvc.Start(a.cfg.Continuation.PollInterval, a.cfg.Continuation.Workers)
	}
	if err := web.RegisterWorkerRoutes(router, a.workerSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register worker routes: %w", err)
	}
	if err := web.RegisterWorkerResultRoutes(router, a.resultSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register worker result routes: %w", err)
	}
	if err := web.RegisterContinuationRoutes(router, a.contSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register continuation routes: %w", err)
	}
	// Role preview REST API (WBS 5.27): read-model view of a role
	// (system prompt, ordered instructions, allowed states, state files,
	// configuration hash) without composing a task prompt.
	if err := web.RegisterPreviewRoutes(router, roleSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register preview routes: %w", err)
	}
	// Settings service + REST API (WBS 3.15): same repositories and pool.
	setSvc := service.NewSettingsService(
		dbrepo.NewSettingsRepository(db.Raw()),
		dbrepo.NewProjectRepository(db.Raw()),
		db.Raw(),
	)
	if err := web.RegisterSettingsRoutes(router, setSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register settings routes: %w", err)
	}
	// Materialization REST API (WBS 7.39): controlled materialization into
	// the project's working directory + per-file drift status. Reuses the
	// P7-14/P7-16 materialization/drift services (the metadata repository
	// wired, per the P7-15 pattern) — no parallel drift path.
	matSvc := service.NewStateFileMaterializeService(
		dbrepo.NewStateFileTypeRepository(db.Raw()),
		dbrepo.NewStateFileRepository(db.Raw()),
		dbrepo.NewStateFileVersionRepository(db.Raw()),
	).WithMaterializedMetadata(dbrepo.NewMaterializedStateFileRepository(db.Raw()))
	if err := web.RegisterStateFileMaterializeRoutes(router, matSvc, stateFileSvc, setSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register materialize routes: %w", err)
	}
	// WBS document repository (WBS 3.18): versioned phase WBS. Shared by
	// the WBS service and the phase service (WBS 4.6 transactional
	// deletion of associated WBS data).
	wbsRepo := dbrepo.NewDocumentRepository(db.Raw())
	// Phase service + REST API (WBS 3.16): server-assigned append ordering.
	phaseSvc := service.NewPhaseService(
		dbrepo.NewPhaseRepository(db.Raw()),
		dbrepo.NewProjectRepository(db.Raw()),
		dbrepo.NewPlanRepository(db.Raw()),
		wbsRepo,
		dbrepo.NewWorkUnitRepository(db.Raw()),
		db.Raw(),
	).WithWBSVersions(
		dbrepo.NewWBSVersionRepository(db.Raw()),
	)
	if err := web.RegisterPhaseRoutes(router, phaseSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register phase routes: %w", err)
	}
	// WBS document service + REST API (WBS 3.18).
	versionRepo := dbrepo.NewWBSVersionRepository(db.Raw())
	wbsSvc := service.NewWBSDocumentService(
		dbrepo.NewPhaseRepository(db.Raw()),
		dbrepo.NewProjectRepository(db.Raw()),
		wbsRepo,
		versionRepo,
		db.Raw(),
	).WithPlans(dbrepo.NewPlanRepository(db.Raw()))
	if err := web.RegisterWBSRoutes(router, wbsSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register WBS routes: %w", err)
	}
	// Readiness service + REST API (WBS 3.19): readiness report + ready op.
	// Lifecycle service (constructed here before the readiness service so
	// the readiness transition records its audit event, WBS 3.7).
	lifecycleSvc := service.NewLifecycleService(
		dbrepo.NewProjectRepository(db.Raw()),
		dbrepo.NewStatusEventRepository(db.Raw()),
		db.Raw(),
	).WithStatusHook(func(ctx context.Context, projectID uuid.UUID, status project.ProjectStatus) {
		a.precomputeSvc.OnProjectStatus(ctx, projectID, status)
		a.contSvc.OnProjectStatus(ctx, projectID, status)
		if a.engine != nil {
			a.engine.OnProjectStatus(ctx, projectID, status)
		}
	})
	a.lifecycleSvc = lifecycleSvc
	readySvc := service.NewReadinessService(
		dbrepo.NewProjectRepository(db.Raw()),
		dbrepo.NewSettingsRepository(db.Raw()),
		dbrepo.NewPhaseRepository(db.Raw()),
		wbsRepo,
	).WithLifecycle(lifecycleSvc)
	if err := web.RegisterReadinessRoutes(router, readySvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register readiness routes: %w", err)
	}
	// Phase 15 (WBS 15.1-15.168): execution bootstrap. The service has no
	// worker transport dependency (WBS 15.77/15.78): only read-only
	// settings / provider-configuration lookups.
	if err := a.startBootstrap(ctx, router, lifecycleSvc, roleSvc, stateFileSvc, scopeResSvc, precomputeSnapshotSvc, matSvc, wbsRepo); err != nil {
		db.Close()
		return err
	}
	// Phase 16 (WBS 16.1-16.181): the autonomous execution engine.
	if err := a.startEngine(ctx, router, lifecycleSvc, stateFileSvc, precomputeSnapshotSvc, setSvc); err != nil {
		db.Close()
		return err
	}
	// Phase 17 (WBS 17.1-17.200): console read models, live feed and the
	// console API. The engine never depends on any of it.
	if err := a.startConsole(router, setSvc, svc, matSvc, roleSvc); err != nil {
		db.Close()
		return err
	}
	// Lifecycle REST API (WBS 3.20): start/pause/resume/cancel.
	if err := web.RegisterLifecycleRoutes(router, lifecycleSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register lifecycle routes: %w", err)
	}
	// Summary endpoint (WBS 3.21): lifecycle state + current phase +
	// WBS progress, sourced from the Phase 2 progress primitive.
	progressSvc := orchestration.NewPhaseProgressService(
		dbrepo.NewProjectRepository(db.Raw()),
		dbrepo.NewPhaseRepository(db.Raw()),
		dbrepo.NewPlanRepository(db.Raw()),
		dbrepo.NewWorkUnitRepository(db.Raw()),
	)
	// Current-wbs endpoint (WBS 4.25): composes the P4-18 WBS read model with
	// the established progress derivation; read-only.
	readModelSvc := service.NewWBSReadModelService(
		dbrepo.NewPhaseRepository(db.Raw()),
		dbrepo.NewProjectRepository(db.Raw()),
		dbrepo.NewWBSVersionRepository(db.Raw()),
		dbrepo.NewPlanRepository(db.Raw()),
		db.Raw(),
	)
	if err := web.RegisterCurrentWBSRoutes(router, readModelSvc, progressSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register current-wbs routes: %w", err)
	}
	if err := web.RegisterSummaryRoutes(router, dbrepo.NewProjectRepository(db.Raw()), progressSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register summary routes: %w", err)
	}
	// State-machine CRUD REST API (WBS 6.35): current machine, versioned
	// edits, and historical version reads. Composes the P6-17 versioning
	// and P6-19 snapshot services (no parallel definition path).
	pool := db.Raw()
	machineSnapshotSvc := service.NewStateMachineSnapshotService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewStateInstructionRepository(pool),
		dbrepo.NewStateRoleAssignmentRepository(pool),
		dbrepo.NewStateFileRequirementRepository(pool),
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewSystemPromptRepository(pool),
		dbrepo.NewTransitionConditionRepository(pool),
	).WithTxPool(pool)
	machineEditSvc := service.NewStateMachineVersioningService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
	).WithTxPool(pool)
	machineSvc := service.NewStateMachineCRUDService(
		dbrepo.NewStateMachineRepository(pool),
		machineSnapshotSvc,
		machineEditSvc,
	).WithValidation(service.NewStateMachineValidationService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewStateRoleAssignmentRepository(pool),
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewRoleAllowedStateRepository(pool),
	))
	if err := web.RegisterStateMachineRoutes(router, machineSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register state-machine routes: %w", err)
	}
	// State-configuration REST API (WBS 6.36): state create/update/disable,
	// role assignment, instruction/state-file/template configuration, and
	// destructive delete with the historical-reference protection. Composes
	// the P6-07/P6-08/P6-09 repositories — no parallel assignment paths.
	stateConfigSvc := service.NewStateConfigService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewStateInstructionRepository(pool),
		dbrepo.NewStateRoleAssignmentRepository(pool),
		dbrepo.NewStateFileRequirementRepository(pool),
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewTransitionHistoryRepository(pool),
		dbrepo.NewRuntimeStateRepository(pool),
	).WithTxPool(pool)
	if err := web.RegisterStateConfigRoutes(router, stateConfigSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register state-config routes: %w", err)
	}
	// Transition-configuration REST API (WBS 6.37): transition
	// create/update/disable, condition and priority configuration.
	// Composes the P6-05/P6-06/P6-27 repositories — no parallel path.
	transitionConfigSvc := service.NewTransitionConfigService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewTransitionConditionRepository(pool),
	).WithTxPool(pool)
	if err := web.RegisterTransitionConfigRoutes(router, transitionConfigSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register transition-config routes: %w", err)
	}
	// Runtime-state REST API (WBS 6.40): read-only runtime state
	// (current state, phase, work unit, version). No mutation surface —
	// the P6-20 Transition and P6-22 Initialize ops remain the only writers.
	runtimeReadSvc := service.NewRuntimeStateAPIService(
		dbrepo.NewRuntimeStateRepository(pool),
		dbrepo.NewPhaseRepository(pool),
		dbrepo.NewPlanRepository(pool),
		dbrepo.NewWorkUnitRepository(pool),
	).WithTxPool(pool)
	if err := web.RegisterRuntimeStateRoutes(router, runtimeReadSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register runtime-state routes: %w", err)
	}
	// Transition-preview REST API (WBS 6.41): evaluate an outcome over
	// the project's current runtime state WITHOUT applying it — the
	// debugging surface for workflow configuration. Read-only: no
	// writes on the path (runtime row, version, history untouched).
	previewSvc := service.NewTransitionPreviewService(
		dbrepo.NewRuntimeStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
	).WithTxPool(pool)
	if err := web.RegisterTransitionPreviewRoutes(router, previewSvc); err != nil {
		db.Close()
		return fmt.Errorf("app: register transition-preview routes: %w", err)
	}
	handler := web.ApplyMiddleware(router, lg)

	// 5. HTTP server: bind last so a bind failure can trigger DB cleanup.
	rh, rd, wr, idleT, maxHdr := a.cfg.Server.Timeouts()
	srv := web.NewServer(web.ServerConfig{ReadHeaderTimeout: rh, ReadTimeout: rd, WriteTimeout: wr, IdleTimeout: idleT, MaxHeaderBytes: maxHdr}, handler)
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx, a.cfg.Addr()) }()

	// Wait for a successful bind (BoundAddr is set before Serve starts).
	deadline := time.Now().Add(5 * time.Second)
	for srv.BoundAddr() == "" {
		select {
		case err := <-done:
			db.Close()
			return fmt.Errorf("app: http bind: %w", err)
		case <-ctx.Done():
			db.Close()
			return fmt.Errorf("app: http bind cancelled")
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			db.Close()
			return fmt.Errorf("app: http bind timed out at %s", a.cfg.Addr())
		}
	}
	// Catch an immediate serve-loop failure after the bind.
	select {
	case err := <-done:
		db.Close()
		return fmt.Errorf("app: http server: %w", err)
	case <-time.After(50 * time.Millisecond):
	}
	a.srv = srv

	// 6. READY only after the server is bound and serving.
	router.SetReady(true)
	a.ready.Store(true)
	a.lg.WithComponent("app").Info("ready", "addr", a.cfg.Addr())
	_ = done
	return nil
}

// Shutdown performs the graceful-shutdown sequence (WBS 1.12): mark
// not-ready, stop accepting + drain HTTP within the configured timeout, then
// close services and the database. It is idempotent and bounded by the
// configured shutdown timeout, so a stuck component cannot block exit (T1.12.6).
func (a *App) Shutdown(ctx context.Context) error {
	timeout := a.cfg.Shutdown.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// Shutdown order (WBS 18.125, decision U6):
	// 1. not ready, unsafe HTTP methods refused (503) while draining;
	a.ready.Store(false)
	if a.router != nil {
		a.router.SetReady(false)
		a.router.SetDraining(true)
	}
	// 2. monitors stop; worker calls cut short from now on stay
	//    reconcilable (never FAILED for our own cancellation, WBS 18.126);
	a.stopBackground()
	if a.workerSvc != nil {
		a.workerSvc.BeginShutdown()
	}
	// 3. the scheduler stops starting steps; running steps get up to
	//    shutdown.worker_drain, then are cancelled; held driver leases are
	//    released so another instance can take over at once;
	if a.engine != nil {
		dctx, dcancel := context.WithTimeout(ctx, a.cfg.Shutdown.EffectiveWorkerDrain())
		a.engine.Shutdown(dctx)
		dcancel()
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// 4. precompute / speculative work stops;
	if a.precomputeSvc != nil {
		a.precomputeSvc.Shutdown(sctx) // cancel + drain precompute jobs
	}
	// 5. live streams end (an open SSE response would otherwise hold the
	//    HTTP drain, WBS 17.156), then the bounded HTTP drain;
	if a.feed != nil {
		a.feed.Stop()
	}
	var err error
	if a.srv != nil {
		err = a.srv.Stop(sctx) // stops accepting, drains in-flight
		a.srv = nil
	}
	// 6. the continuation runner and the worker transport drain, so final
	//    dispatch statuses persist (WBS 14.62 / 12.67);
	if a.contSvc != nil {
		a.contSvc.Shutdown(sctx)
	}
	if a.workerSvc != nil {
		_ = a.workerSvc.Close(sctx)
	}
	// 7. the database closes last.
	if a.db != nil {
		a.db.Close()
	}
	if a.lg != nil {
		a.lg.WithComponent("app").Info("shutdown complete")
	}
	return err
}

// Stop tears down in reverse dependency order: HTTP server, then database.
// It is idempotent (T1.6.5).
func (a *App) Stop(ctx context.Context) error {
	a.stopBackground()
	a.ready.Store(false)
	if a.feed != nil {
		a.feed.Stop() // open streams end before the HTTP drain
	}
	var err error
	if a.srv != nil {
		err = a.srv.Stop(ctx)
		a.srv = nil
	}
	if a.contSvc != nil {
		a.contSvc.Shutdown(ctx)
	}
	if a.workerSvc != nil {
		_ = a.workerSvc.Close(ctx)
	}
	if a.db != nil {
		a.db.Close()
		a.db = nil
	}
	return err
}

// startBootstrap wires the Phase 15 bootstrap service, its routes and the
// startup recovery of interrupted bootstraps (after continuation recovery).
func (a *App) startBootstrap(ctx context.Context, router *web.Router, lifecycleSvc *service.LifecycleService, roleSvc *service.RoleService,
	stateFileSvc *service.StateFileService, scopeResSvc *service.StateFileScopeResolutionService, snapSvc *service.ExecutionSnapshotService,
	matSvc *service.StateFileMaterializeService, _ *dbrepo.DocumentRepository) error {
	pool := a.db.Raw()
	machineSnaps := service.NewStateMachineSnapshotService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewStateInstructionRepository(pool),
		dbrepo.NewStateRoleAssignmentRepository(pool),
		dbrepo.NewStateFileRequirementRepository(pool),
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewSystemPromptRepository(pool),
		dbrepo.NewTransitionConditionRepository(pool),
	).WithTxPool(pool)
	validation := service.NewStateMachineValidationService(
		dbrepo.NewStateMachineRepository(pool),
		dbrepo.NewWorkflowStateRepository(pool),
		dbrepo.NewStateTransitionRepository(pool),
		dbrepo.NewStateRoleAssignmentRepository(pool),
		dbrepo.NewRoleRepository(pool),
		dbrepo.NewRoleAllowedStateRepository(pool),
	)
	protoRepo := dbrepo.NewResponseProtocolRepository(pool)
	workerProviders := a.cfg.WorkerProviders
	compProviders := a.cfg.CompressionProviders
	a.bootstrapSvc = service.NewBootstrapService(service.BootstrapDeps{
		Build:           service.BuildInfo{AppVersion: Version, SchemaVersion: a.schemaVersion, ConfigHash: a.configSnapshot.Hash},
		Pool:            pool,
		Repo:            dbrepo.NewExecutionRepository(pool),
		Projects:        dbrepo.NewProjectRepository(pool),
		ProjectSettings: dbrepo.NewSettingsRepository(pool),
		Phases:          dbrepo.NewPhaseRepository(pool),
		WBSVersions:     dbrepo.NewWBSVersionRepository(pool),
		Plans:           dbrepo.NewPlanRepository(pool),
		Units:           dbrepo.NewWorkUnitRepository(pool),
		Runtime:         dbrepo.NewRuntimeStateRepository(pool),
		Machines:        dbrepo.NewStateMachineRepository(pool),
		States:          dbrepo.NewWorkflowStateRepository(pool),
		MachineSnaps:    machineSnaps,
		Validation:      validation,
		Assignments:     dbrepo.NewStateRoleAssignmentRepository(pool),
		Roles:           dbrepo.NewRoleRepository(pool),
		RoleSvc:         roleSvc,
		Templates:       dbrepo.NewPromptTemplateRepository(pool),
		TemplateVers:    dbrepo.NewPromptTemplateVersionRepository(pool),
		Protocols:       protoRepo,
		ProtocolFind:    protoRepo,
		ScopeRes:        scopeResSvc,
		StateFiles:      stateFileSvc,
		ActiveTask: service.NewActiveTaskService(stateFileSvc, dbrepo.NewPhaseRepository(pool),
			dbrepo.NewPlanRepository(pool), dbrepo.NewWorkUnitRepository(pool)),
		Materialize:   matSvc,
		ContextSnaps:  snapSvc,
		SnapshotsRepo: dbrepo.NewContextSnapshotRepository(pool),
		Cache:         a.promptCacheSvc,
		Compression:   a.compressionSvc,
		Precompute:    a.precomputeSvc,
		Worker:        a.workerSvc,
		Results:       a.resultSvc,
		Continuation:  a.contSvc,
		Lifecycle:     lifecycleSvc,
		Inspector:     a.repoInspector(),
		Identifier:    a.repoInspector(),
		Identities:    dbrepo.NewRepositoryIdentityStore(pool),
		WorkerProvider: func(name string) (service.WorkerProviderInfo, bool) {
			p, ok := workerProviders[name]
			if !ok {
				return service.WorkerProviderInfo{}, false
			}
			return service.WorkerProviderInfo{Kind: string(p.Kind), Endpoint: p.Endpoint, AuthSecretEnv: p.AuthSecretEnv,
				ConnectTimeout: p.ConnectTimeout, SessionTimeout: p.SessionTimeout, MaxResponseBytes: p.MaxResponseBytes,
				AllowedRoots: append([]string(nil), p.AllowedRoots...), Capabilities: []string{"sessions", "cwd", "usage"}}, true
		},
		CompressionProvider: func(name string) (service.CompressionProviderInfo, bool) {
			p, ok := compProviders[name]
			if !ok {
				return service.CompressionProviderInfo{}, false
			}
			return service.CompressionProviderInfo{Kind: string(p.Kind), BaseURL: p.BaseURL, APIKeyEnv: p.APIKeyEnv}, true
		},
		WorkerSessionProbe: func(ctx context.Context, projectID uuid.UUID) error {
			return a.workerSvc.ValidateConfiguration(ctx, projectID, worker.DefaultWorkflowRequirements)
		},
		CompressionSessionProbe: a.compressionSessionProbe,
		ProviderTestPassed:      a.providerTestPassed,
		Owner:                   a.workerSvc.Owner(),
		Logger:                  a.lg.WithComponent("bootstrap").Logger,
	})
	if err := web.RegisterExecutionRoutes(router, a.bootstrapSvc); err != nil {
		return fmt.Errorf("app: register execution routes: %w", err)
	}
	if !a.cfg.Bootstrap.RecoveryDisabled {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := a.bootstrapSvc.RecoverBootstraps(rctx); err != nil {
			return fmt.Errorf("app: recover bootstraps: %w", err)
		}
	}
	return nil
}

// startEngine wires the Phase 16 execution engine, its routes, the startup
// reconciliation of in-flight engine state and the scheduler.
func (a *App) startEngine(ctx context.Context, router *web.Router, lifecycleSvc *service.LifecycleService,
	stateFileSvc *service.StateFileService, snapSvc *service.ExecutionSnapshotService, setSvc *service.SettingsService) error {
	pool := a.db.Raw()
	engRepo := dbrepo.NewEngineRepository(pool)
	a.engine = service.NewEngine(service.EngineDeps{
		Recovery:        dbrepo.NewRecoveryStore(pool),
		Pool:            pool,
		Execs:           dbrepo.NewExecutionRepository(pool),
		Repo:            engRepo,
		Plans:           dbrepo.NewPlanRepository(pool),
		Units:           dbrepo.NewWorkUnitRepository(pool),
		Phases:          dbrepo.NewPhaseRepository(pool),
		Projects:        dbrepo.NewProjectRepository(pool),
		ProjectSettings: dbrepo.NewSettingsRepository(pool),
		WBSVersions:     dbrepo.NewWBSVersionRepository(pool),
		Machines:        dbrepo.NewStateMachineRepository(pool),
		States:          dbrepo.NewWorkflowStateRepository(pool),
		Runtime:         dbrepo.NewRuntimeStateRepository(pool),
		Dispatches:      dbrepo.NewWorkerDispatchRepository(pool),
		Prompts:         dbrepo.NewCompressionRepository(pool),
		CommandPrompts:  dbrepo.NewCompressionRepository(pool),
		TemplateVers:    dbrepo.NewPromptTemplateVersionRepository(pool),
		ContextSnaps:    snapSvc,
		Cache:           a.promptCacheSvc,
		Worker:          a.workerSvc,
		Results:         a.resultSvc,
		Continuations:   a.contSvc,
		ActiveTask: service.NewActiveTaskService(stateFileSvc, dbrepo.NewPhaseRepository(pool),
			dbrepo.NewPlanRepository(pool), dbrepo.NewWorkUnitRepository(pool)),
		StateFiles:     stateFileSvc,
		Lifecycle:      lifecycleSvc,
		Runner:         valrun.Runner{ExtraEnv: a.cfg.Engine.ValidationEnvAllow},
		MaxValidations: a.cfg.Engine.EffectiveMaxValidations(),
		Invariants:     dbrepo.NewIntegrityScanner(pool),
		Checkpointer:   a.checkpointer(),
		Inspector:      a.repoInspector(),
		Identifier:     a.repoInspector(),
		Identities:     dbrepo.NewRepositoryIdentityStore(pool),
		Owner:          a.workerSvc.Owner(),
		Logger:         a.lg.WithComponent("engine").Logger,
		Gate:           a.workGate(),
		GC: func(ctx context.Context) error {
			_, err := a.gc().Run(ctx)
			return err
		},
	})
	a.bootstrapSvc.SetEngineSettings(engRepo)
	// Schedule immediately on activation and when auto-continue is turned
	// on, instead of waiting for the next scheduler tick.
	a.bootstrapSvc.SetActivationHook(a.engine.Kick)
	setSvc.SetUpdateHook(func(st *project.Settings) {
		if st.AutoContinue {
			a.engine.Kick()
		}
	})
	if err := web.RegisterEngineRoutes(router, a.engine, lifecycleSvc); err != nil {
		return fmt.Errorf("app: register engine routes: %w", err)
	}
	if !a.cfg.Engine.RecoveryDisabled {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := a.engine.RecoverExecutions(rctx); err != nil {
			return fmt.Errorf("app: recover executions: %w", err)
		}
	}
	// Restored database (WBS 18.46, decision U4): checks run now; the
	// instance stays read-only with the scheduler off until an operator
	// enables execution.
	held, err := a.startRestoreGate(ctx)
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	a.verifyAuditInBackground()
	if !a.cfg.Engine.SchedulerDisabled {
		start := func() { a.engine.Start(a.cfg.Engine.Interval, a.cfg.Engine.Workers) }
		if held {
			a.restoreGate().OnEnable(start)
		} else {
			start()
		}
	}
	return nil
}

// hermesProviderConfig renders a configured worker provider as the Hermes
// adapter's opaque provider configuration (secret by reference only).
func hermesProviderConfig(p config.WorkerProviderConfig) json.RawMessage {
	m := map[string]any{"endpoint": p.Endpoint}
	if ref := workerSecretRef(p); ref != "" {
		m["auth_secret_ref"] = ref
	}
	if p.ConnectTimeout > 0 {
		m["connect_timeout"] = p.ConnectTimeout.String()
	}
	if p.MaxResponseBytes > 0 {
		m["max_response_bytes"] = p.MaxResponseBytes
	}
	if p.Reconnect.MaxAttempts > 0 || p.Reconnect.InitialDelay > 0 || p.Reconnect.MaxDelay > 0 {
		r := map[string]any{"max_attempts": p.Reconnect.MaxAttempts}
		if p.Reconnect.InitialDelay > 0 {
			r["initial_delay"] = p.Reconnect.InitialDelay.String()
		}
		if p.Reconnect.MaxDelay > 0 {
			r["max_delay"] = p.Reconnect.MaxDelay.String()
		}
		m["reconnect"] = r
	}
	if p.TLS != nil {
		m["tls"] = map[string]string{"ca_file": p.TLS.CAFile, "server_name": p.TLS.ServerName, "min_version": p.TLS.MinVersion}
	}
	if len(p.AllowedRoots) > 0 {
		m["allowed_roots"] = p.AllowedRoots
	}
	b, _ := json.Marshal(m)
	return b
}
