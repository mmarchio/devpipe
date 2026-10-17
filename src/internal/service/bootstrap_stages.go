package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/precompute"
	"overseer/internal/project"
	"overseer/internal/resilience"
	"overseer/internal/role"
	"overseer/internal/state"
	"overseer/internal/statefile"
	"overseer/internal/wbs"
)

// Bootstrap outcomes.
const (
	OutcomeBootstrapped   = "BOOTSTRAPPED"
	OutcomeResumed        = "RESUMED"
	OutcomeAlreadyRunning = "ALREADY_RUNNING"
	OutcomeActivated      = "ACTIVATED"
)

// BootstrapResult is the outcome of a Bootstrap call. It is returned (with
// whatever is known) alongside an error as well.
type BootstrapResult struct {
	Execution *persistence.ExecutionRecord
	Readiness *execution.ExecutionReadinessReport
	Plan      *execution.BootstrapPlan
	Outcome   string
}

var (
	errSourceChanged = errors.New("bootstrap: source versions changed")
	errActiveExists  = errors.New("bootstrap: an active execution exists")
	// errDrift marks a FINALIZING cross-check mismatch (PLAN Decision 8).
	errDrift = errors.New("bootstrap: configuration drift during bootstrap")
)

func bootErr(code execution.BootstrapErrorCode, stage execution.BootstrapStage, msg string, cause error) *execution.BootstrapError {
	return execution.NewError(code, stage, msg, cause)
}

// ---------------------------------------------------------------------------
// Events + logging (P15-50).

func (s *BootstrapService) appendEvent(ctx context.Context, ex persistence.Executor, projectID uuid.UUID, execID *uuid.UUID, typ execution.EventType, payload map[string]any) error {
	return s.d.Repo.AppendEvent(ctx, ex, &execution.Event{ProjectID: projectID, ExecutionID: execID, Type: typ, Payload: payload, CreatedAt: s.d.Now().UTC()})
}

// logEvent writes the WBS 15.105 structured log line (no secrets, no
// prompt or state-file content).
func (s *BootstrapService) logEvent(rec *persistence.ExecutionRecord, projectID uuid.UUID, msg string, start time.Time, extra ...any) {
	args := []any{"project_id", projectID.String(), "duration", time.Since(start).String()}
	if rec != nil {
		args = append(args, "execution_id", rec.ID.String(), "execution_number", rec.Number, "phase_id", rec.InitialPhaseID.String(),
			"wbs_version", rec.WBSVersion, "state_machine_version", rec.StateMachineVersion, "bootstrap_stage", string(rec.Stage),
			"runtime_state", rec.FirstActionableStateKey, "plan_hash", rec.PlanHash)
	}
	s.d.Logger.Info(msg, append(args, extra...)...)
}

func (s *BootstrapService) inTx(ctx context.Context, fn func(tx persistence.Transaction) error) error {
	return dbrepo.Run(ctx, s.d.Pool, fn)
}

// ---------------------------------------------------------------------------
// Bootstrap (P15-60).

// Bootstrap converts a READY project into an activated execution
// (WBS 15.6; PLAN Decision 11 for idempotency / resume / concurrency).
func (s *BootstrapService) Bootstrap(ctx context.Context, projectID uuid.UUID) (*BootstrapResult, error) {
	res := &BootstrapResult{}
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return res, translate(err)
	}
	mu := s.projectLock(projectID)
	mu.Lock()
	defer mu.Unlock()
	active, err := s.d.Repo.GetActive(ctx, nil, projectID)
	if err == nil {
		return s.handleActive(ctx, res, active)
	}
	if !errors.Is(err, persistence.ErrNotFound) {
		return res, translate(err)
	}
	return s.fresh(ctx, res, projectID, false)
}

func (s *BootstrapService) handleActive(ctx context.Context, res *BootstrapResult, active *persistence.ExecutionRecord) (*BootstrapResult, error) {
	res.Execution = active
	switch active.Status {
	case execution.ExecutionRunning:
		res.Outcome = OutcomeAlreadyRunning
		return res, nil
	case execution.ExecutionReady:
		if ok, err := s.d.Repo.Claim(ctx, active.ID, s.d.Owner, s.d.Now(), s.lease(ctx, active.ProjectID)); err != nil {
			return res, translate(err)
		} else if !ok {
			return res, bootErr(execution.CodeConcurrentBootstrap, active.Stage, "execution is being activated by another instance", nil)
		}
		defer func() { _ = s.d.Repo.Release(context.WithoutCancel(ctx), active.ID, s.d.Owner) }()
		rec, err := s.Activate(ctx, active.ID)
		if rec != nil {
			res.Execution = rec
		}
		res.Outcome = OutcomeActivated
		return res, err
	case execution.ExecutionInitializing:
		ok, err := s.d.Repo.Claim(ctx, active.ID, s.d.Owner, s.d.Now(), s.lease(ctx, active.ProjectID))
		if err != nil {
			return res, translate(err)
		}
		if !ok {
			return res, bootErr(execution.CodeConcurrentBootstrap, active.Stage, fmt.Sprintf("execution #%d is being bootstrapped by another instance", active.Number), nil)
		}
		if err := s.appendEvent(ctx, nil, active.ProjectID, &active.ID, execution.EventBootstrapRequested, map[string]any{"retry": true, "stage": string(active.Stage)}); err != nil {
			return res, translate(err)
		}
		res.Outcome = OutcomeResumed
		return s.run(ctx, res, active, false)
	}
	return res, bootErr(execution.CodeProjectNotReady, "", fmt.Sprintf("execution #%d is %s", active.Number, active.Status), nil)
}

// lease is the project's configured bootstrap claim lease.
func (s *BootstrapService) lease(ctx context.Context, projectID uuid.UUID) time.Duration {
	st, _, err := s.d.Repo.GetSettings(ctx, projectID)
	if err != nil || st.BootstrapLease <= 0 {
		return execution.DefaultSettings().BootstrapLease
	}
	return st.BootstrapLease
}

// fresh runs readiness and creates a new execution (P15-51).
func (s *BootstrapService) fresh(ctx context.Context, res *BootstrapResult, projectID uuid.UUID, rerun bool) (*BootstrapResult, error) {
	start := s.d.Now()
	for attempt := 0; attempt < 3; attempt++ {
		g, err := s.gather(ctx, projectID)
		if err != nil {
			return res, err
		}
		rep := s.readiness(g)
		res.Readiness = rep
		res.Plan = s.plan(g)
		if !rep.Ready {
			code := rep.FirstCode()
			if code == "" {
				code = execution.CodeConfigInvalid
			}
			msgs := make([]string, 0, len(rep.Errors))
			for _, e := range rep.Errors {
				msgs = append(msgs, e.Message)
			}
			_ = s.inTx(ctx, func(tx persistence.Transaction) error {
				if err := s.appendEvent(ctx, tx, projectID, nil, execution.EventBootstrapRequested, map[string]any{"retry": false}); err != nil {
					return err
				}
				return s.appendEvent(ctx, tx, projectID, nil, execution.EventBootstrapFailed, map[string]any{
					"code": string(code), "class": string(execution.ClassConfiguration), "stage": string(execution.StageValidating),
					"errors": len(rep.Errors), "completion_eligible": rep.CompletionEligible})
			})
			s.logEvent(nil, projectID, "bootstrap readiness failed", start, "code", string(code), "errors", len(rep.Errors))
			msg := strings.Join(msgs, "; ")
			if rep.CompletionEligible && msg == "" {
				msg = "all phases are DONE; the project is completion-eligible"
			}
			return res, bootErr(code, execution.StageValidating, msg, nil)
		}
		if s.createHook != nil {
			s.createHook(ctx, attempt)
		}
		rec, err := s.create(ctx, g, res.Plan, rerun)
		switch {
		case errors.Is(err, errSourceChanged):
			continue // concurrent edit: rerun readiness (WBS 15.90/15.128)
		case errors.Is(err, errActiveExists):
			active, aerr := s.d.Repo.GetActive(ctx, nil, projectID)
			if aerr != nil {
				return res, translate(aerr)
			}
			return s.handleActive(ctx, res, active)
		case err != nil:
			return res, bootErr(execution.CodePersistenceFailed, execution.StageSnapshotting, "create execution", err)
		}
		res.Execution, res.Outcome = rec, OutcomeBootstrapped
		s.logEvent(rec, projectID, "execution configuration snapshotted", start)
		return s.run(ctx, res, rec, rerun)
	}
	return res, bootErr(execution.CodeConcurrentBootstrap, execution.StageValidating, "project configuration kept changing during bootstrap", nil)
}

// create commits the execution + configuration snapshot (the snapshot
// boundary, WBS 15.89) under the project bootstrap lock after re-checking
// every source version (WBS 15.90–15.94).
func (s *BootstrapService) create(ctx context.Context, g *gathered, plan *execution.BootstrapPlan, rerun bool) (*persistence.ExecutionRecord, error) {
	content := s.content(g)
	canonical, err := execution.EncodeContent(content)
	if err != nil {
		return nil, err
	}
	hash := execution.HashCanonical(canonical)
	ex, err := execution.NewExecution(execution.NewExecutionInput{ProjectID: g.project.ID, Number: 1,
		StateMachineID: g.machineRec.FamilyID, StateMachineVersion: g.machineRec.Version, StateMachineHash: g.machine.Hash,
		InitialPhaseID: g.facts.Phase.ID, Now: s.d.Now()})
	if err != nil {
		return nil, err
	}
	exp := s.d.Now().Add(g.exec.BootstrapLease).UTC()
	rec := &persistence.ExecutionRecord{Execution: *ex, WBSVersion: g.wbsVersion, WBSHash: g.wbsHash, PlanID: plan.PlanID,
		PlanState: plan.PlanState, InitialStateKey: plan.InitialState, FirstActionableStateKey: plan.FirstActionable,
		InitialRoleKey: plan.Role, InitialWorkUnitID: plan.WorkUnitID, InitialTemplateKey: plan.TemplateKey, PlanHash: plan.Hash(),
		ClaimOwner: s.d.Owner, ClaimExpiresAt: &exp, AppVersion: s.d.Build.AppVersion, SchemaVersion: s.d.Build.SchemaVersion,
		AppConfigHash: s.d.Build.ConfigHash}
	snap := &persistence.ConfigurationSnapshotRecord{ID: uuid.New(), ExecutionID: rec.ID, ProjectID: g.project.ID,
		ProjectVersion: g.project.Version, Canonical: canonical, Hash: hash, CreatedAt: s.d.Now().UTC()}
	err = s.inTx(ctx, func(tx persistence.Transaction) error {
		if err := s.d.Repo.LockProject(ctx, tx, g.project.ID); err != nil {
			return err
		}
		if _, err := s.d.Repo.GetActive(ctx, tx, g.project.ID); err == nil {
			return errActiveExists
		} else if !errors.Is(err, persistence.ErrNotFound) {
			return err
		}
		fresh, err := s.gather(ctx, g.project.ID)
		if err != nil {
			return err
		}
		if d := s.sources(fresh).Diff(plan.Sources); len(d) > 0 {
			s.d.Logger.Info("bootstrap sources changed; rerunning readiness", "project_id", g.project.ID.String(), "changed", strings.Join(d, ","))
			return errSourceChanged
		}
		if err := s.d.Repo.Create(ctx, tx, rec); err != nil {
			if errors.Is(err, persistence.ErrConflict) {
				return errActiveExists
			}
			return err
		}
		snap.ExecutionID = rec.ID
		if err := s.d.Repo.CreateSnapshot(ctx, tx, snap); err != nil {
			return err
		}
		rec.ConfigurationSnapshotID = snap.ID
		id := rec.ID
		if err := s.appendEvent(ctx, tx, g.project.ID, &id, execution.EventBootstrapRequested, map[string]any{"retry": false, "rerun": rerun}); err != nil {
			return err
		}
		if err := s.appendEvent(ctx, tx, g.project.ID, &id, execution.EventReadinessValidated, map[string]any{"warnings": len(s.readiness(g).Warnings)}); err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, g.project.ID, &id, execution.EventConfigurationSnapshotted, map[string]any{
			"number": rec.Number, "configuration_hash": hash, "plan_hash": rec.PlanHash})
	})
	if err != nil {
		return nil, err
	}
	return s.d.Repo.GetByID(ctx, nil, rec.ID)
}

// stageContext is the loaded material a stage needs.
type stageContext struct {
	rec     *persistence.ExecutionRecord
	content execution.ConfigurationContent
}

func (s *BootstrapService) load(ctx context.Context, rec *persistence.ExecutionRecord) (*stageContext, error) {
	snap, err := s.d.Repo.GetSnapshot(ctx, rec.ID)
	if err != nil {
		return nil, err
	}
	c, err := execution.DecodeContent(snap.Canonical)
	if err != nil {
		return nil, err
	}
	return &stageContext{rec: rec, content: c}, nil
}

// run executes the remaining stages, then activates (WBS 15.157 order).
func (s *BootstrapService) run(ctx context.Context, res *BootstrapResult, rec *persistence.ExecutionRecord, rerun bool) (*BootstrapResult, error) {
	start := s.d.Now()
	defer func() { _ = s.d.Repo.Release(context.WithoutCancel(ctx), rec.ID, s.d.Owner) }()
	sc, err := s.load(ctx, rec)
	if err != nil {
		return res, bootErr(execution.CodePersistenceFailed, rec.Stage, "load configuration snapshot", err)
	}
	for rec.Stage != execution.StageComplete {
		if rec.Stage == execution.StageFailed {
			next, err := s.d.Repo.AdvanceStage(ctx, nil, rec.ID, rec.Version, rec.FailedStage, "retry", &persistence.ExecutionMutation{ClearFailure: true})
			if err != nil {
				return res, bootErr(execution.CodeConcurrentBootstrap, rec.Stage, "resume failed stage", err)
			}
			rec = next
		}
		sc.rec = rec
		res.Execution = rec
		if s.stageHook != nil {
			if err := s.stageHook(ctx, rec, rec.Stage); err != nil {
				return s.failed(ctx, res, rec, err, rerun)
			}
		}
		var next *persistence.ExecutionRecord
		switch rec.Stage {
		case execution.StageSnapshotting:
			next, err = s.stageSnapshotting(ctx, sc)
		case execution.StageInitializingFiles:
			next, err = s.stageFiles(ctx, sc)
		case execution.StageInitializingRuntime:
			next, err = s.stageRuntime(ctx, sc)
		case execution.StagePreparingContext:
			next, err = s.stageContext(ctx, sc)
		case execution.StageFinalizing:
			next, err = s.stageFinalize(ctx, sc)
		default:
			err = bootErr(execution.CodePersistenceFailed, rec.Stage, "unexpected stage", nil)
		}
		if err != nil {
			return s.failed(ctx, res, rec, err, rerun)
		}
		rec = next
		res.Execution = rec
		_, _ = s.d.Repo.Claim(ctx, rec.ID, s.d.Owner, s.d.Now(), sc.content.Execution.BootstrapLease)
	}
	s.logEvent(rec, rec.ProjectID, "execution bootstrap completed", start)
	act, err := s.Activate(ctx, rec.ID)
	if act != nil {
		res.Execution = act
	}
	return res, err
}

// failed persists the failure (P15-59) and, for a FINALIZING drift, reruns
// the bootstrap once from readiness (PLAN Decision 8).
func (s *BootstrapService) failed(ctx context.Context, res *BootstrapResult, rec *persistence.ExecutionRecord, cause error, rerun bool) (*BootstrapResult, error) {
	if errors.Is(cause, persistence.ErrConflict) || errors.Is(cause, ErrConflict) {
		// Another instance advanced the execution concurrently: not a
		// bootstrap failure of this execution (nothing is recorded).
		return res, bootErr(execution.CodeConcurrentBootstrap, rec.Stage, "execution advanced concurrently", cause)
	}
	be, ok := execution.AsBootstrapError(cause)
	if !ok {
		be = bootErr(execution.CodePersistenceFailed, rec.Stage, "bootstrap stage failed", cause)
	}
	if be.Stage == "" {
		be.Stage = rec.Stage
	}
	cur, err := s.d.Repo.GetByID(context.WithoutCancel(ctx), nil, rec.ID)
	if err == nil {
		terminal := be.Class == execution.ClassConfiguration
		reason := be.Message
		if be.Cause != nil {
			reason = strings.TrimSpace(reason + ": " + be.Cause.Error())
		}
		var updated *persistence.ExecutionRecord
		_ = s.inTx(context.WithoutCancel(ctx), func(tx persistence.Transaction) error {
			u, err := s.d.Repo.RecordFailure(ctx, tx, cur.ID, cur.Version, rec.Stage, be.Code, be.Class, reason, terminal)
			if err != nil {
				return err
			}
			updated = u
			id := cur.ID
			return s.appendEvent(ctx, tx, cur.ProjectID, &id, execution.EventBootstrapFailed, map[string]any{
				"code": string(be.Code), "class": string(be.Class), "stage": string(rec.Stage), "terminal": terminal})
		})
		if updated != nil {
			res.Execution = updated
		}
	}
	s.logEvent(res.Execution, rec.ProjectID, "execution bootstrap failed", s.d.Now(), "code", string(be.Code), "class", string(be.Class))
	if errors.Is(cause, errDrift) && !rerun {
		return s.fresh(ctx, &BootstrapResult{}, rec.ProjectID, true)
	}
	return res, be
}

// ---------------------------------------------------------------------------
// Stages (P15-52 .. P15-57).

// stageSnapshotting captures the snapshot-time repository baseline
// (WBS 15.19/15.80) and moves to INITIALIZING_FILES.
func (s *BootstrapService) stageSnapshotting(ctx context.Context, sc *stageContext) (*persistence.ExecutionRecord, error) {
	if _, err := s.captureBaseline(ctx, sc, persistence.BaselineSnapshot); err != nil {
		return nil, err
	}
	if err := s.recordIdentity(ctx, sc); err != nil {
		return nil, err
	}
	return s.d.Repo.AdvanceStage(ctx, nil, sc.rec.ID, sc.rec.Version, execution.StageInitializingFiles, "configuration snapshotted", nil)
}

// recordIdentity records the repository identity the execution will work
// in (WBS 18.49) — once; a resumed bootstrap keeps the first record.
func (s *BootstrapService) recordIdentity(ctx context.Context, sc *stageContext) error {
	if s.d.Identifier == nil || s.d.Identities == nil {
		return nil
	}
	id, err := s.d.Identifier.Identify(ctx, sc.content.WorkingDirectory)
	if err != nil {
		e := bootErr(execution.CodeRepositoryInvalid, sc.rec.Stage, "repository identity could not be read", err)
		e.Class = execution.ClassRecoverable
		return e
	}
	if !id.IsGit && requiresGit(sc.content) {
		e := bootErr(execution.CodeRepositoryInvalid, sc.rec.Stage, fmt.Sprintf("the project uses git (checkpoint policy REQUIRED) but %s is not a git repository: "+
			"run `git init` there or turn off \"Use git\" in the project's checkpoint configuration", sc.content.WorkingDirectory), nil)
		e.Class = execution.ClassConfiguration
		return e
	}
	_, _, err = s.d.Identities.Record(ctx, &persistence.RepositoryIdentityRecord{ExecutionID: sc.rec.ID, ProjectID: sc.rec.ProjectID,
		Identity: id, Source: persistence.IdentityFromBootstrap, RecordedAt: s.d.Now().UTC()})
	if err != nil {
		return bootErr(execution.CodePersistenceFailed, sc.rec.Stage, "record repository identity", err)
	}
	// The expected HEAD starts at the bootstrap HEAD (WBS 18.50).
	if _, err := s.d.Identities.LatestExpectedHead(ctx, sc.rec.ID); errors.Is(err, persistence.ErrNotFound) {
		_, err = s.d.Identities.AppendExpectedHead(ctx, &persistence.ExpectedHeadRecord{ExecutionID: sc.rec.ID, ProjectID: sc.rec.ProjectID,
			Head: id.Head, Branch: id.Branch, Source: persistence.HeadFromBootstrap, RecordedAt: s.d.Now().UTC()})
		if err != nil {
			return bootErr(execution.CodePersistenceFailed, sc.rec.Stage, "record expected repository head", err)
		}
	} else if err != nil {
		return bootErr(execution.CodePersistenceFailed, sc.rec.Stage, "read expected repository head", err)
	}
	return nil
}

// requiresGit reports whether a checkpoint failure would block the
// execution: task or hardening-group checkpoints are REQUIRED.
func requiresGit(c execution.ConfigurationContent) bool {
	h := c.HardeningOrDefault()
	return c.EngineSettingsOrDefault().CheckpointPolicy == execution.CheckpointRequired ||
		(h.Enabled && h.GroupCheckpointPolicy == string(execution.CheckpointRequired))
}

// captureBaseline inspects the repository and stores the baseline of the
// kind once (P15-40).
func (s *BootstrapService) captureBaseline(ctx context.Context, sc *stageContext, kind persistence.BaselineKind) (*persistence.BaselineRecord, error) {
	if b, err := s.d.Repo.GetBaseline(ctx, sc.rec.ID, kind); err == nil {
		return b, nil
	}
	snap := execution.RepositorySnapshot{WorkingDirectory: sc.content.WorkingDirectory}
	if s.d.Inspector != nil {
		var err error
		snap, err = s.d.Inspector.Inspect(ctx, sc.content.WorkingDirectory)
		if err != nil {
			e := bootErr(execution.CodeRepositoryInvalid, sc.rec.Stage, "repository inspection failed", err)
			e.Class = execution.ClassRecoverable
			return nil, e
		}
	}
	return s.d.Repo.CreateBaseline(ctx, &persistence.BaselineRecord{ID: uuid.New(), ExecutionID: sc.rec.ID, Kind: kind,
		Snapshot: snap, Hash: snap.Hash(), CapturedAt: s.d.Now().UTC()})
}

// stageFiles initializes the state files (WBS 15.43–15.52).
func (s *BootstrapService) stageFiles(ctx context.Context, sc *stageContext) (*persistence.ExecutionRecord, error) {
	rec := sc.rec
	stage := execution.StageInitializingFiles
	perr := func(msg string, err error) error {
		return bootErr(execution.CodePersistenceFailed, stage, msg, err)
	}
	proj, err := s.d.Projects.Get(ctx, rec.ProjectID.String())
	if err != nil {
		return nil, perr("load project", err)
	}
	ph, err := s.d.Phases.Get(ctx, rec.InitialPhaseID)
	if err != nil {
		return nil, perr("load phase", err)
	}
	baseline, err := s.captureBaseline(ctx, sc, persistence.BaselineSnapshot)
	if err != nil {
		return nil, err
	}
	var units []*wbs.WorkUnit
	planVersion := 0
	if rec.PlanID != nil {
		if units, err = s.d.Units.ListByPlan(ctx, *rec.PlanID); err != nil {
			return nil, perr("list units", err)
		}
		if plans, err := s.d.Plans.ListByPhase(ctx, ph.ID); err == nil {
			for _, p := range plans {
				if p.ID == *rec.PlanID {
					planVersion = p.Version
				}
			}
		}
	}
	done := 0
	for _, u := range units {
		if u.Status == wbs.WorkUnitStatusDONE {
			done++
		}
	}
	pid, phaseID := rec.ProjectID, ph.ID
	write := func(key string, phase *uuid.UUID, content string) error {
		if _, err := s.files.Derive(ctx, pid, key, phase, nil, content); err != nil {
			return perr("initialize "+key, err)
		}
		return nil
	}
	if err := write("PROJECT_STATE", nil, execution.RenderProjectState(execution.ProjectStateInput{
		ProjectName: proj.Name, ProjectID: proj.ID.String(), ExecutionNumber: rec.Number, ExecutionStatus: string(rec.Status),
		PhaseOrder: ph.Order, PhaseName: ph.Name, WorkflowState: rec.FirstActionableStateKey, Repository: baseline.Snapshot})); err != nil {
		return nil, err
	}
	if err := write("PHASE_STATE", &phaseID, execution.RenderPhaseState(execution.PhaseStateInput{
		PhaseID: ph.ID.String(), PhaseOrder: ph.Order, PhaseName: ph.Name, PhaseStatus: string(ph.Status), WBSVersion: rec.WBSVersion,
		PlanStatus: rec.PlanState, PlanVersion: planVersion, DoneUnits: done, TotalUnits: len(units)})); err != nil {
		return nil, err
	}
	completed, err := s.completedUnits(ctx, pid)
	if err != nil {
		return nil, perr("list completed units", err)
	}
	if err := write("TASK_ARCHIVE", nil, statefile.RenderTaskArchive(completed)); err != nil {
		return nil, err
	}
	if err := write("TEST_STATE", &phaseID, execution.RenderTestState(ph.Name, nil)); err != nil {
		return nil, err
	}
	if rec.InitialWorkUnitID != nil {
		u, err := s.d.Units.Get(ctx, *rec.InitialWorkUnitID)
		if err != nil {
			return nil, perr("load work unit", err)
		}
		if _, err := s.d.ActiveTask.DeriveActiveTaskForUnit(ctx, pid, u); err != nil {
			return nil, perr("initialize ACTIVE_TASK", err)
		}
	}
	// Statefile workflow: the team's role files and HERMES.md.
	if err := s.writeTeamFiles(ctx, sc.content); err != nil {
		msg := "team files could not be written to " + wbs.StatefilesDir
		if resilience.IsDiskFull(err) {
			msg += ": disk full (no space left on device); free space and resume the bootstrap"
		}
		return nil, bootErr(execution.CodeMaterializationFailed, stage, msg, err)
	}
	if sc.content.Execution.MaterializeStateFiles && s.d.Materialize != nil {
		if _, err := s.d.Materialize.Materialize(ctx, sc.content.WorkingDirectory, pid.String()); err != nil {
			msg := "state-file materialization failed"
			if resilience.IsDiskFull(err) {
				msg += ": disk full (no space left on device); free space and resume the bootstrap"
			}
			return nil, bootErr(execution.CodeMaterializationFailed, stage, msg, err)
		}
	}
	var out *persistence.ExecutionRecord
	err = s.inTx(ctx, func(tx persistence.Transaction) error {
		var e error
		out, e = s.d.Repo.AdvanceStage(ctx, tx, rec.ID, rec.Version, execution.StageInitializingRuntime, "state files initialized", nil)
		if e != nil {
			return e
		}
		id := rec.ID
		return s.appendEvent(ctx, tx, pid, &id, execution.EventStateFilesInitialized, map[string]any{"materialized": sc.content.Execution.MaterializeStateFiles})
	})
	return out, err
}

func (s *BootstrapService) completedUnits(ctx context.Context, projectID uuid.UUID) ([]*wbs.WorkUnit, error) {
	phases, err := s.d.Phases.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var out []*wbs.WorkUnit
	for _, p := range phases {
		plans, err := s.d.Plans.ListByPhase(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, pl := range plans {
			units, err := s.d.Units.ListByPlan(ctx, pl.ID)
			if err != nil {
				return nil, err
			}
			for _, u := range units {
				if u.Status == wbs.WorkUnitStatusDONE {
					out = append(out, u)
				}
			}
		}
	}
	return out, nil
}

// facts reconstructs the routing facts recorded on the execution.
func recordedFacts(rec *persistence.ExecutionRecord) execution.RoutingFacts {
	f := execution.RoutingFacts{PlanState: rec.PlanState}
	if rec.InitialWorkUnitID != nil {
		f.NextUnit = &wbs.WorkUnit{ID: *rec.InitialWorkUnitID}
	} else if rec.PlanState == execution.PlanCurrent {
		f.AllUnitsDone = true
	}
	return f
}

// stageRuntime binds the runtime at the machine's initial state, records
// the bootstrap entry and applies the control path (WBS 15.53–15.56,
// 15.59) in one transaction.
func (s *BootstrapService) stageRuntime(ctx context.Context, sc *stageContext) (*persistence.ExecutionRecord, error) {
	rec := sc.rec
	stage := execution.StageInitializingRuntime
	m := &sc.content.StateMachine
	path, err := execution.ResolveFirstActionable(m, recordedFacts(rec), sc.content.Execution.MaxBootstrapControlTransitions)
	if err != nil {
		return nil, bootErr(execution.CodeStateMachineInvalid, stage, "control-state resolution", err)
	}
	if path.FirstActionable.Key != rec.FirstActionableStateKey {
		return nil, bootErr(execution.CodeStateMachineInvalid, stage, "control path disagrees with the bootstrap plan", nil)
	}
	mrec, err := s.d.Machines.GetVersion(ctx, rec.StateMachineID, rec.StateMachineVersion)
	if err != nil {
		return nil, bootErr(execution.CodeStateMachineInvalid, stage, "bound machine version missing", err)
	}
	recs, err := s.d.States.ListByMachine(ctx, mrec.ID, rec.StateMachineVersion)
	if err != nil {
		return nil, bootErr(execution.CodePersistenceFailed, stage, "list states", err)
	}
	ids := map[string]uuid.UUID{}
	for _, r := range recs {
		ids[r.Key] = r.ID
	}
	for _, k := range append([]string{path.InitialState.Key, path.FirstActionable.Key}, transitionTargets(path.Transitions)...) {
		if ids[k] == uuid.Nil {
			return nil, bootErr(execution.CodeStateMachineInvalid, stage, "state "+k+" missing from bound machine", nil)
		}
	}
	phaseID := rec.InitialPhaseID
	execID := rec.ID
	var out *persistence.ExecutionRecord
	err = s.inTx(ctx, func(tx persistence.Transaction) error {
		rt, err := s.d.Repo.BindRuntime(ctx, tx, &persistence.RuntimeStateRecord{ProjectID: rec.ProjectID, StateMachineID: rec.StateMachineID,
			StateMachineVersion: rec.StateMachineVersion, CurrentStateID: ids[path.InitialState.Key], PhaseID: &phaseID, ExecutionID: &execID})
		if err != nil {
			return err
		}
		now := s.d.Now().UTC()
		if err := s.d.Repo.AppendHistory(ctx, tx, &persistence.TransitionHistoryRecord{ProjectID: rec.ProjectID, StateMachineID: rec.StateMachineID,
			StateMachineVersion: rec.StateMachineVersion, ToStateID: ids[path.InitialState.Key], Outcome: execution.OutcomeBootstrap,
			PhaseID: &phaseID, RuntimeVersion: rt.Version, ExecutionID: &execID, CreatedAt: now}); err != nil {
			return err
		}
		repo := s.d.Runtime.WithTx(tx)
		for i, t := range path.Transitions {
			next := *rt
			next.CurrentStateID, next.EnteredAt = ids[t.To], now
			var unitID *uuid.UUID
			if i == len(path.Transitions)-1 {
				unitID = rec.InitialWorkUnitID
			}
			next.WorkUnitID = unitID
			if err := repo.UpdateWithVersion(ctx, &next, rt.Version); err != nil {
				return err
			}
			next.Version = rt.Version + 1
			rt = &next
			if err := s.d.Repo.AppendHistory(ctx, tx, &persistence.TransitionHistoryRecord{ProjectID: rec.ProjectID, StateMachineID: rec.StateMachineID,
				StateMachineVersion: rec.StateMachineVersion, FromStateID: ids[t.From], ToStateID: ids[t.To], Outcome: t.Outcome,
				PhaseID: &phaseID, WorkUnitID: unitID, RuntimeVersion: rt.Version, ExecutionID: &execID, CreatedAt: now}); err != nil {
				return err
			}
		}
		fa := ids[path.FirstActionable.Key]
		out, err = s.d.Repo.AdvanceStage(ctx, tx, rec.ID, rec.Version, execution.StagePreparingContext, "runtime initialized",
			&persistence.ExecutionMutation{FirstActionableStateID: &fa})
		if err != nil {
			return err
		}
		return s.appendEvent(ctx, tx, rec.ProjectID, &execID, execution.EventRuntimeInitialized, map[string]any{
			"runtime_state_id": rt.ID.String(), "initial_state": path.InitialState.Key, "first_actionable_state": path.FirstActionable.Key,
			"control_transitions": len(path.Transitions)})
	})
	if err != nil {
		return nil, bootErr(execution.CodePersistenceFailed, stage, "initialize runtime", err)
	}
	return out, nil
}

func transitionTargets(ts []state.TransitionSnapshot) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.From, t.To)
	}
	return out
}

// stageContext creates the initial context snapshot (WBS 15.57/15.71) and
// prepares the initial prompt (WBS 15.72–15.76).
func (s *BootstrapService) stageContext(ctx context.Context, sc *stageContext) (*persistence.ExecutionRecord, error) {
	rec := sc.rec
	stage := execution.StagePreparingContext
	if rec.InitialContextSnapshotID == nil {
		snap, err := s.d.ContextSnaps.CreateExecutionSnapshot(ctx, rec.ProjectID.String())
		if err != nil {
			if errors.Is(err, ErrMissingStateFile) || errors.Is(err, ErrNotFound) {
				return nil, bootErr(execution.CodeStateFileInvalid, stage, "initial context is incomplete", err)
			}
			return nil, bootErr(execution.CodePersistenceFailed, stage, "create initial context snapshot", err)
		}
		if snap.StateKey != rec.FirstActionableStateKey {
			return nil, bootErr(execution.CodeStateMachineInvalid, stage, "context snapshot state disagrees with the first actionable state", nil)
		}
		sid := snap.ID
		execID := rec.ID
		err = s.inTx(ctx, func(tx persistence.Transaction) error {
			u, err := s.d.Repo.SetArtifacts(ctx, tx, rec.ID, rec.Version, &persistence.ExecutionMutation{InitialContextSnapshotID: &sid})
			if err != nil {
				return err
			}
			rec = u
			return s.appendEvent(ctx, tx, rec.ProjectID, &execID, execution.EventContextPrepared, map[string]any{
				"context_snapshot_id": sid.String(), "state": snap.StateKey, "role": snap.RoleSnapshot.RoleKey, "files": len(snap.StateFiles)})
		})
		if err != nil {
			return nil, bootErr(execution.CodePersistenceFailed, stage, "record context snapshot", err)
		}
		sc.rec = rec
	}
	// Initial prompt with the CAPTURED template version (PLAN Decision 16).
	var tmpl *execution.PromptTemplateSnapshot
	for i := range sc.content.PromptTemplates {
		if sc.content.PromptTemplates[i].Key == rec.InitialTemplateKey {
			tmpl = &sc.content.PromptTemplates[i]
		}
	}
	mut := &persistence.ExecutionMutation{}
	// A command state sends its command as is: there is no prompt to
	// prepare ahead.
	if tmpl != nil && s.d.Cache != nil && !execution.IsCommandTemplate(tmpl.Key) {
		pr, err := s.d.Cache.PrepareForContext(ctx, rec.ProjectID, PrepareRequest{SnapshotID: *rec.InitialContextSnapshotID,
			TemplateID: tmpl.TemplateID, TemplateVersion: tmpl.Version})
		if err != nil {
			code := execution.CodePromptInvalid
			if pr != nil && pr.CanonicalPromptID != uuid.Nil || strings.Contains(strings.ToLower(err.Error()), "compress") {
				code = execution.CodeCompressionFailed
			}
			return nil, bootErr(code, stage, "initial prompt preparation failed", err)
		}
		if pr == nil || pr.WorkerPrompt == nil {
			return nil, bootErr(execution.CodeCompressionFailed, stage, "initial prompt has no worker prompt", nil)
		}
		wp := pr.WorkerPrompt.ID
		ck, cn := pr.CacheKey, pr.CanonicalKey
		mut.InitialWorkerPromptID, mut.InitialCacheKey, mut.InitialCanonicalKey = &wp, &ck, &cn
	}
	var out *persistence.ExecutionRecord
	execID := rec.ID
	err := s.inTx(ctx, func(tx persistence.Transaction) error {
		var e error
		out, e = s.d.Repo.AdvanceStage(ctx, tx, rec.ID, rec.Version, execution.StageFinalizing, "initial prompt prepared", mut)
		if e != nil {
			return e
		}
		payload := map[string]any{"template": rec.InitialTemplateKey}
		if mut.InitialCacheKey != nil {
			payload["cache_key"] = *mut.InitialCacheKey
		}
		return s.appendEvent(ctx, tx, rec.ProjectID, &execID, execution.EventPromptPrepared, payload)
	})
	if err != nil {
		return nil, bootErr(execution.CodePersistenceFailed, stage, "record initial prompt", err)
	}
	// Speculative precomputation: scheduled, never awaited, failures
	// isolated (WBS 15.75/15.76).
	if s.d.Precompute != nil {
		s.d.Precompute.OnTrigger(context.WithoutCancel(ctx), rec.ProjectID, precompute.TriggerProjectStart)
	}
	return out, nil
}

// stageFinalize re-validates the working directory, captures the FINAL
// baseline, cross-checks the derived context against the snapshot and
// writes the checkpoint (WBS 15.126/15.127/15.149/15.159).
func (s *BootstrapService) stageFinalize(ctx context.Context, sc *stageContext) (*persistence.ExecutionRecord, error) {
	rec := sc.rec
	stage := execution.StageFinalizing
	canon, errs := execution.CheckWorkingDirectory(sc.content.WorkingDirectory, sc.content.Worker.AllowedRoots)
	if len(errs) > 0 || canon != sc.content.WorkingDirectory {
		msg := "working directory changed"
		if len(errs) > 0 {
			msg = errs[0].Message
		}
		return nil, bootErr(execution.CodeWorkdirInvalid, stage, msg, nil)
	}
	if _, err := s.captureBaseline(ctx, sc, persistence.BaselineFinal); err != nil {
		return nil, err
	}
	if err := s.crossCheck(ctx, sc); err != nil {
		return nil, err
	}
	cp, err := s.buildCheckpoint(ctx, sc)
	if err != nil {
		return nil, err
	}
	var out *persistence.ExecutionRecord
	execID := rec.ID
	err = s.inTx(ctx, func(tx persistence.Transaction) error {
		if err := s.d.Repo.CreateCheckpoint(ctx, tx, &persistence.CheckpointRecord{ID: uuid.New(), Checkpoint: *cp, Hash: cp.Hash(), CreatedAt: s.d.Now().UTC()}); err != nil {
			return err
		}
		u, err := s.d.Repo.AdvanceStage(ctx, tx, rec.ID, rec.Version, execution.StageComplete, "bootstrap checkpoint written", nil)
		if err != nil {
			return err
		}
		u, err = s.d.Repo.UpdateStatus(ctx, tx, u.ID, u.Version, execution.ExecutionReady, "bootstrap complete")
		if err != nil {
			return err
		}
		out = u
		return s.appendEvent(ctx, tx, rec.ProjectID, &execID, execution.EventBootstrapCompleted, map[string]any{"checkpoint_hash": cp.Hash()})
	})
	if err != nil {
		return nil, bootErr(execution.CodePersistenceFailed, stage, "write checkpoint", err)
	}
	return out, nil
}

// crossCheck guards against a hybrid execution (PLAN Decision 8): the
// derived context must agree with the configuration snapshot.
func (s *BootstrapService) crossCheck(ctx context.Context, sc *stageContext) error {
	rec := sc.rec
	drift := func(msg string) error {
		e := bootErr(execution.CodeConfigInvalid, execution.StageFinalizing, msg, errDrift)
		return e
	}
	if rec.InitialContextSnapshotID == nil {
		return drift("initial context snapshot missing")
	}
	cs, err := s.d.SnapshotsRepo.Get(ctx, *rec.InitialContextSnapshotID)
	if err != nil {
		return bootErr(execution.CodePersistenceFailed, execution.StageFinalizing, "load context snapshot", err)
	}
	if cs.StateMachineID != rec.StateMachineID || cs.StateMachineVersion != rec.StateMachineVersion {
		return drift("context snapshot machine binding differs from the execution")
	}
	var rs role.RoleSnapshot
	if err := json.Unmarshal(cs.RoleSnapshotJSON, &rs); err == nil {
		for _, r := range sc.content.Roles {
			if r.RoleKey == rs.RoleKey && r.ConfigurationHash() != rs.ConfigurationHash() {
				return drift("role " + rs.RoleKey + " changed during bootstrap")
			}
		}
	}
	cur, err := s.d.WBSVersions.GetCurrentVersion(ctx, rec.InitialPhaseID.String())
	if err == nil && cur != rec.WBSVersion {
		return drift(fmt.Sprintf("WBS changed during bootstrap (v%d → v%d)", rec.WBSVersion, cur))
	}
	plans, err := s.d.Plans.ListByPhase(ctx, rec.InitialPhaseID)
	if err == nil {
		active := wbs.ActivePlan(rec.InitialPhaseID, plans)
		if (active == nil) != (rec.PlanID == nil) || (active != nil && rec.PlanID != nil && active.ID != *rec.PlanID) {
			return drift("execution plan changed during bootstrap")
		}
	}
	return nil
}

func contextSnapshotHash(cs *persistence.ContextSnapshotRecord) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%d|%s|%d|%s|", cs.ID, cs.RuntimeStateID, cs.RuntimeStateVersion, cs.StateMachineID, cs.StateMachineVersion, cs.StateKey)
	h.Write(cs.RoleSnapshotJSON)
	for _, f := range cs.Files {
		fmt.Fprintf(h, "|%+v", *f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func historyHash(rows []*persistence.TransitionHistoryRecord) string {
	h := sha256.New()
	for _, r := range rows {
		fmt.Fprintf(h, "%d|%s|%s|%s|%d\n", r.Seq, r.FromStateID, r.ToStateID, r.Outcome, r.RuntimeVersion)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildCheckpoint assembles the checkpoint from the current artifacts.
func (s *BootstrapService) buildCheckpoint(ctx context.Context, sc *stageContext) (*execution.BootstrapCheckpoint, error) {
	rec := sc.rec
	perr := func(msg string, err error) error {
		return bootErr(execution.CodePersistenceFailed, execution.StageFinalizing, msg, err)
	}
	snap, err := s.d.Repo.GetSnapshot(ctx, rec.ID)
	if err != nil {
		return nil, perr("load configuration snapshot", err)
	}
	rt, err := s.d.Repo.GetRuntimeByExecution(ctx, nil, rec.ID)
	if err != nil {
		return nil, perr("load runtime", err)
	}
	hist, err := s.d.Repo.ListExecutionHistory(ctx, rec.ID)
	if err != nil {
		return nil, perr("load runtime history", err)
	}
	cp := &execution.BootstrapCheckpoint{ExecutionID: rec.ID, ConfigurationHash: snap.Hash, RuntimeStateID: rt.ID,
		BootstrapHistoryHash: historyHash(hist), BootstrapHistoryCount: len(hist), FirstActionableState: rec.FirstActionableStateKey,
		PlanHash: rec.PlanHash, CacheKey: rec.InitialCacheKey, CanonicalKey: rec.InitialCanonicalKey}
	if rec.InitialContextSnapshotID != nil {
		cs, err := s.d.SnapshotsRepo.Get(ctx, *rec.InitialContextSnapshotID)
		if err != nil {
			return nil, perr("load context snapshot", err)
		}
		cp.ContextSnapshotID, cp.ContextSnapshotHash = cs.ID, contextSnapshotHash(cs)
	}
	if b, err := s.d.Repo.GetBaseline(ctx, rec.ID, persistence.BaselineFinal); err == nil {
		cp.BaselineID, cp.BaselineHash = b.ID, b.Snapshot.Hash()
	}
	if rec.InitialWorkerPromptID != nil {
		h, err := s.d.Repo.WorkerPromptHash(ctx, *rec.InitialWorkerPromptID)
		if err != nil {
			return nil, perr("load worker prompt", err)
		}
		cp.WorkerPromptID, cp.WorkerPromptHash = *rec.InitialWorkerPromptID, h
	}
	keys := append(append([]string(nil), derivedStateFiles...), "ARCHITECTURE")
	for _, k := range keys {
		var phaseID *uuid.UUID
		if k == "PHASE_STATE" || k == "TEST_STATE" {
			p := rec.InitialPhaseID
			phaseID = &p
		}
		ref, ok, err := s.files.Current(ctx, rec.ProjectID, k, phaseID, nil)
		if err != nil {
			return nil, perr("load state file "+k, err)
		}
		if ok {
			cp.StateFiles = append(cp.StateFiles, ref)
		}
	}
	if rec.InitialWorkUnitID != nil {
		ref, ok, err := s.files.Current(ctx, rec.ProjectID, "ACTIVE_TASK", nil, rec.InitialWorkUnitID)
		if err != nil {
			return nil, perr("load ACTIVE_TASK", err)
		}
		if ok {
			cp.StateFiles = append(cp.StateFiles, ref)
		}
	}
	cp.Normalize()
	return cp, nil
}

// VerifyBootstrapCheckpoint recomputes every referenced artifact from the
// database and compares it with the stored checkpoint (WBS 15.150).
func (s *BootstrapService) VerifyBootstrapCheckpoint(ctx context.Context, executionID uuid.UUID) ([]string, error) {
	rec, err := s.d.Repo.GetByID(ctx, nil, executionID)
	if err != nil {
		return nil, translate(err)
	}
	stored, err := s.d.Repo.GetCheckpoint(ctx, executionID)
	if errors.Is(err, persistence.ErrNotFound) {
		return []string{"bootstrap checkpoint missing"}, nil
	}
	if err != nil {
		return nil, translate(err)
	}
	var problems []string
	if stored.Checkpoint.Hash() != stored.Hash {
		problems = append(problems, "checkpoint hash mismatch")
	}
	obs := execution.BootstrapCheckpoint{ExecutionID: rec.ID, FirstActionableState: rec.FirstActionableStateKey, PlanHash: rec.PlanHash,
		CacheKey: rec.InitialCacheKey, CanonicalKey: rec.InitialCanonicalKey}
	if snap, err := s.d.Repo.GetSnapshot(ctx, rec.ID); err == nil {
		obs.ConfigurationHash = execution.HashCanonical(snap.Canonical)
		if obs.ConfigurationHash != snap.Hash {
			problems = append(problems, "configuration snapshot content does not match its hash")
		}
	}
	if rt, err := s.d.Repo.GetRuntimeByExecution(ctx, nil, rec.ID); err == nil {
		obs.RuntimeStateID = rt.ID
	}
	if hist, err := s.d.Repo.ListExecutionHistory(ctx, rec.ID); err == nil {
		n := stored.Checkpoint.BootstrapHistoryCount
		if n > len(hist) {
			n = len(hist)
		}
		obs.BootstrapHistoryHash, obs.BootstrapHistoryCount = historyHash(hist[:n]), n
	}
	if rec.InitialContextSnapshotID != nil {
		if cs, err := s.d.SnapshotsRepo.Get(ctx, *rec.InitialContextSnapshotID); err == nil {
			obs.ContextSnapshotID, obs.ContextSnapshotHash = cs.ID, contextSnapshotHash(cs)
		}
	}
	if b, err := s.d.Repo.GetBaseline(ctx, rec.ID, persistence.BaselineFinal); err == nil {
		obs.BaselineID, obs.BaselineHash = b.ID, b.Snapshot.Hash()
	}
	if rec.InitialWorkerPromptID != nil {
		if h, err := s.d.Repo.WorkerPromptHash(ctx, *rec.InitialWorkerPromptID); err == nil {
			obs.WorkerPromptID, obs.WorkerPromptHash = *rec.InitialWorkerPromptID, h
		}
	}
	var ids []uuid.UUID
	for _, f := range stored.Checkpoint.StateFiles {
		ids = append(ids, f.VersionID)
	}
	hashes, err := s.d.Repo.StateFileVersionHashes(ctx, ids)
	if err != nil {
		return nil, translate(err)
	}
	for _, f := range stored.Checkpoint.StateFiles {
		if h, ok := hashes[f.VersionID]; ok {
			obs.StateFiles = append(obs.StateFiles, execution.FileRef{Type: f.Type, VersionID: f.VersionID, ContentHash: h})
		}
	}
	return append(problems, execution.VerifyCheckpoint(stored.Checkpoint, obs)...), nil
}

// ---------------------------------------------------------------------------
// Activation (P15-58).

// Activate verifies the bootstrap checkpoint and moves project READY →
// RUNNING and execution READY → RUNNING in one transaction (WBS
// 15.151–15.155). An execution already RUNNING is an idempotent no-op.
func (s *BootstrapService) Activate(ctx context.Context, executionID uuid.UUID) (*persistence.ExecutionRecord, error) {
	rec, err := s.d.Repo.GetByID(ctx, nil, executionID)
	if err != nil {
		return nil, translate(err)
	}
	proj, err := s.d.Projects.Get(ctx, rec.ProjectID.String())
	if err != nil {
		return rec, translate(err)
	}
	problems, err := s.VerifyBootstrapCheckpoint(ctx, rec.ID)
	if err != nil {
		return rec, err
	}
	rt, rtErr := s.d.Repo.GetRuntimeByExecution(ctx, nil, rec.ID)
	in := execution.ActivationInput{ExecutionStatus: rec.Status, CheckpointVerified: len(problems) == 0, CheckpointProblems: problems,
		ProjectStatus: string(proj.Status), RuntimeExists: rtErr == nil, RuntimeBound: rtErr == nil && rt.ExecutionID != nil && *rt.ExecutionID == rec.ID,
		FirstActionableKnown: rec.FirstActionableStateID != nil, ContextSnapshotKnown: rec.InitialContextSnapshotID != nil}
	probs, verr := execution.ValidateActivation(in)
	if errors.Is(verr, execution.ErrAlreadyRunning) {
		return rec, nil
	}
	if len(probs) > 0 {
		return rec, bootErr(execution.CodeConfigInvalid, execution.StageComplete, "activation preconditions not met: "+strings.Join(probs, "; "), nil)
	}
	var notify func()
	var out *persistence.ExecutionRecord
	err = s.inTx(ctx, func(tx persistence.Transaction) error {
		_, n, err := s.d.Lifecycle.TransitionTx(ctx, tx, rec.ProjectID, fmt.Sprintf("start (execution #%d)", rec.Number), project.StatusRUNNING,
			func(p *project.Project) (bool, error) {
				if p.Status != project.StatusREADY {
					return false, ErrInvalidInput
				}
				return true, nil
			})
		if err != nil {
			return err
		}
		notify = n
		out, err = s.d.Repo.UpdateStatus(ctx, tx, rec.ID, rec.Version, execution.ExecutionRunning, "execution activated")
		if err != nil {
			return err
		}
		id := rec.ID
		return s.appendEvent(ctx, tx, rec.ProjectID, &id, execution.EventStarted, map[string]any{"number": rec.Number})
	})
	if err != nil {
		return rec, bootErr(execution.CodePersistenceFailed, execution.StageComplete, "activate execution", err)
	}
	if notify != nil {
		notify()
	}
	s.logEvent(out, rec.ProjectID, "execution started", s.d.Now())
	if s.onActivated != nil {
		s.onActivated()
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Startup recovery (P15-61).

// RecoverBootstraps resumes interrupted bootstraps and activates READY
// executions of READY projects (WBS 15.107–15.112). FAILED-stage
// executions wait for an explicit retry.
func (s *BootstrapService) RecoverBootstraps(ctx context.Context) (int, error) {
	list, err := s.d.Repo.ListRecoverable(ctx, s.d.Now())
	if err != nil {
		return 0, translate(err)
	}
	n := 0
	for _, rec := range list {
		mu := s.projectLock(rec.ProjectID)
		mu.Lock()
		n += s.recoverOne(ctx, rec)
		mu.Unlock()
	}
	return n, nil
}

// recoverOne resumes / activates one recoverable execution (1 = done).
func (s *BootstrapService) recoverOne(ctx context.Context, rec *persistence.ExecutionRecord) int {
	{
		ok, err := s.d.Repo.Claim(ctx, rec.ID, s.d.Owner, s.d.Now(), s.lease(ctx, rec.ProjectID))
		if err != nil || !ok {
			return 0
		}
		cur, err := s.d.Repo.GetByID(ctx, nil, rec.ID)
		if err != nil {
			return 0
		}
		res := &BootstrapResult{Execution: cur}
		switch cur.Status {
		case execution.ExecutionInitializing:
			_, err = s.run(ctx, res, cur, false)
		case execution.ExecutionReady:
			_, err = s.Activate(ctx, cur.ID)
			_ = s.d.Repo.Release(ctx, cur.ID, s.d.Owner)
		}
		if err != nil {
			s.d.Logger.Warn("bootstrap recovery failed", "execution_id", cur.ID.String(), "error", err.Error())
			return 0
		}
		return 1
	}
}
