package service

import (
	"context"
	"errors"

	"time"

	"github.com/google/uuid"

	"overseer/internal/execution"
	"overseer/internal/persistence"
	"overseer/internal/wbs"
)

// ExecutionView is the API read model of one execution.
type ExecutionView struct {
	Execution *persistence.ExecutionRecord
	Config    *persistence.ConfigurationSnapshotRecord
}

// Executions lists a project's executions newest first (WBS 15.97).
func (s *BootstrapService) Executions(ctx context.Context, projectID uuid.UUID, limit, offset int) ([]*persistence.ExecutionRecord, int, error) {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return nil, 0, translate(err)
	}
	out, total, err := s.d.Repo.ListByProject(ctx, projectID, limit, offset)
	return out, total, translateNil(err)
}

// Execution returns one project-scoped execution (WBS 15.97 isolation).
func (s *BootstrapService) Execution(ctx context.Context, projectID, id uuid.UUID) (*persistence.ExecutionRecord, error) {
	rec, err := s.d.Repo.Get(ctx, nil, projectID, id)
	return rec, translateNil(err)
}

// ActiveExecution returns the project's active execution (ErrNotFound when
// none).
func (s *BootstrapService) ActiveExecution(ctx context.Context, projectID uuid.UUID) (*persistence.ExecutionRecord, error) {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return nil, translateNil(err)
	}
	rec, err := s.d.Repo.GetActive(ctx, nil, projectID)
	return rec, translateNil(err)
}

// BootstrapStatus is the stage + history read model (WBS 15.98).
type BootstrapStatus struct {
	Execution *persistence.ExecutionRecord
	History   []persistence.StageTransition
	Events    []execution.Event
}

// BootstrapStatusOf returns an execution's bootstrap stage and history.
func (s *BootstrapService) BootstrapStatusOf(ctx context.Context, projectID, id uuid.UUID) (*BootstrapStatus, error) {
	rec, err := s.Execution(ctx, projectID, id)
	if err != nil {
		return nil, err
	}
	h, err := s.d.Repo.ListStageHistory(ctx, id)
	if err != nil {
		return nil, translateNil(err)
	}
	ev, err := s.d.Repo.ListEvents(ctx, projectID, &id)
	if err != nil {
		return nil, translateNil(err)
	}
	return &BootstrapStatus{Execution: rec, History: h, Events: ev}, nil
}

// Configuration returns the execution's immutable configuration snapshot
// (WBS 15.99). Secrets are never captured (WBS 15.42), so the decoded
// content is the safe view.
func (s *BootstrapService) Configuration(ctx context.Context, projectID, id uuid.UUID) (*persistence.ConfigurationSnapshotRecord, *execution.ConfigurationContent, error) {
	if _, err := s.Execution(ctx, projectID, id); err != nil {
		return nil, nil, err
	}
	snap, err := s.d.Repo.GetSnapshot(ctx, id)
	if err != nil {
		return nil, nil, translateNil(err)
	}
	c, err := execution.DecodeContent(snap.Canonical)
	if err != nil {
		return nil, nil, ErrInfrastructure
	}
	return snap, &c, nil
}

// RuntimeForExecution returns the runtime bound to the execution; a
// historical execution whose runtime was rebound gets ErrNotFound
// (WBS 15.55).
func (s *BootstrapService) RuntimeForExecution(ctx context.Context, projectID, id uuid.UUID) (*persistence.RuntimeStateRecord, error) {
	if _, err := s.Execution(ctx, projectID, id); err != nil {
		return nil, err
	}
	rt, err := s.d.Repo.GetRuntimeByExecution(ctx, nil, id)
	return rt, translateNil(err)
}

// ActiveExecutionView builds the active-execution panel (WBS 15.103).
func (s *BootstrapService) ActiveExecutionView(ctx context.Context, projectID uuid.UUID) (*persistence.ExecutionRecord, *execution.ActiveExecutionView, error) {
	rec, err := s.ActiveExecution(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	in := execution.ActiveExecutionInput{Execution: rec.Execution, Role: rec.InitialRoleKey, BootstrapStage: rec.Stage,
		WorkflowState: rec.FirstActionableStateKey}
	phaseID := rec.InitialPhaseID
	if rt, err := s.d.Repo.GetRuntimeByExecution(ctx, nil, rec.ID); err == nil {
		if st, err := s.d.States.Get(ctx, rt.CurrentStateID); err == nil {
			in.WorkflowState = st.Key
		}
		if rt.PhaseID != nil {
			phaseID = *rt.PhaseID
		}
	}
	if ph, err := s.d.Phases.Get(ctx, phaseID); err == nil {
		in.PhaseOrder, in.PhaseName = ph.Order, ph.Name
		if plans, err := s.d.Plans.ListByPhase(ctx, ph.ID); err == nil {
			if active := wbs.ActivePlan(ph.ID, plans); active != nil {
				if units, err := s.d.Units.ListByPlan(ctx, active.ID); err == nil {
					in.TotalUnits = len(units)
					for _, u := range units {
						if u.Status == wbs.WorkUnitStatusDONE {
							in.DoneUnits++
						}
					}
				}
			}
		}
	}
	if snap, err := s.d.Repo.GetSnapshot(ctx, rec.ID); err == nil {
		in.ConfigHash = snap.Hash
	}
	in.Dispatches, in.Continuations, in.SessionResets, in.NoProgress, _ = s.d.Repo.ActivityCounts(ctx, projectID)
	v := execution.NewActiveExecutionView(in)
	return rec, &v, nil
}

// Metrics derives bootstrap metrics from committed events (WBS 15.106).
func (s *BootstrapService) Metrics(ctx context.Context, projectID uuid.UUID) (execution.Metrics, error) {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return execution.Metrics{}, translate(err)
	}
	ev, err := s.d.Repo.ListEvents(ctx, projectID, nil)
	if err != nil {
		return execution.Metrics{}, translate(err)
	}
	return execution.AggregateMetrics(ev), nil
}

// IsNotFound reports a not-found service error.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// translateNil is translate that keeps a nil error nil.
func translateNil(err error) error {
	if err == nil {
		return nil
	}
	return translate(err)
}

// AppendEngineEventForTest appends a step-intent event (concurrent
// sequence tests).
func (s *BootstrapService) AppendEngineEventForTest(ctx context.Context, projectID, executionID uuid.UUID, tag string) error {
	return s.d.Repo.AppendEvent(ctx, nil, &execution.Event{ProjectID: projectID, ExecutionID: &executionID, Type: execution.EventStepIntent,
		Payload: map[string]any{"tag": tag}, CreatedAt: time.Now().UTC()})
}
