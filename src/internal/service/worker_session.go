package service

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/worker"
)

// WorkerSettings returns the project's worker settings (defaults when not
// stored).
func (s *WorkerService) WorkerSettings(ctx context.Context, projectID uuid.UUID) (worker.Settings, bool, error) {
	st, stored, err := s.d.Settings.Get(ctx, projectID)
	if err != nil {
		return worker.Settings{}, false, translate(err)
	}
	return st, stored, nil
}

// UpdateWorkerSettings validates and stores the project's worker settings.
func (s *WorkerService) UpdateWorkerSettings(ctx context.Context, projectID uuid.UUID, st worker.Settings) error {
	if err := st.Validate(); err != nil {
		return errors.Join(ErrInvalidInput, err)
	}
	if err := s.d.Settings.Upsert(ctx, projectID, st); err != nil {
		return translate(err)
	}
	return nil
}

// NewSession creates a fresh worker session for the project (WBS 12.19-
// 12.21, 12.63, 12.64). Order: the worker creates the session first; only
// after it confirms does one transaction persist the new ACTIVE session
// and mark the previous one RESET. A failed worker call changes nothing; a
// failed transaction claims nothing. Creation only — no cd, no dispatch.
func (s *WorkerService) NewSession(ctx context.Context, projectID uuid.UUID) (*persistence.WorkerSessionRecord, error) {
	pw, err := s.worker(ctx, projectID)
	if err != nil {
		return nil, err
	}
	st, _, err := s.WorkerSettings(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var prev *persistence.WorkerSessionRecord
	if p, err := s.d.Sessions.GetActiveForProject(ctx, projectID); err == nil {
		prev = p
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return nil, translate(err)
	}
	info, err := pw.w.NewSession(ctx, worker.NewSessionRequest{ProjectID: projectID})
	if err != nil {
		we := worker.AsWorkerError(err)
		pw.recordErr(we, s.d.Now())
		return nil, we
	}
	now := s.d.Now()
	rec := &persistence.WorkerSessionRecord{ID: uuid.New(), ProjectID: projectID, Provider: pw.provider, ProviderSessionID: info.ID,
		Status: worker.SessionActive, Policy: st.SessionPolicy, WorkingDirectory: info.WorkingDirectory, Usage: info.Usage,
		CreatedAt: now, LastSeenAt: now}
	var prevID *uuid.UUID
	if prev != nil && st.SessionPolicy == worker.PolicySingleActive {
		prevID = &prev.ID
	}
	if prevID != nil {
		err = s.d.Sessions.ReplaceActive(ctx, rec, prevID, now)
	} else {
		err = s.d.Sessions.Create(ctx, rec)
	}
	if err != nil {
		s.d.Logger.Error("worker session not persisted", "project_id", projectID.String(), "provider_session_id", info.ID, "error", err.Error())
		return nil, translate(err)
	}
	if prevID != nil {
		s.event(ctx, projectID, prevID, nil, worker.EventSessionReset, map[string]any{"provider_session_id": prev.ProviderSessionID, "replaced_by": rec.ID.String()})
	}
	id := rec.ID
	s.event(ctx, projectID, &id, nil, worker.EventSessionCreated, map[string]any{"provider_session_id": info.ID, "policy": string(st.SessionPolicy)})
	return rec, nil
}

// ChangeDirectory sets the active session's working directory (WBS 12.23-
// 12.25, 12.62). An empty path means the project's configured directory.
// The confirmed directory is persisted only after the worker confirms;
// on failure the session stays ACTIVE with its directory unchanged.
func (s *WorkerService) ChangeDirectory(ctx context.Context, projectID uuid.UUID, path string) (*persistence.WorkerSessionRecord, error) {
	if path == "" {
		ps, err := s.d.ProjectSettings.Get(ctx, projectID.String())
		if err != nil {
			return nil, translate(err)
		}
		path = ps.WorkingDirectory
	}
	if err := worker.ValidateDirectoryRequest(path, nil, s.d.LocalDirCheck); err != nil {
		return nil, worker.NewError(worker.CodeInvalidRequest, worker.DeliveryNotSent, err.Error(), err)
	}
	rec, err := s.d.Sessions.GetActiveForProject(ctx, projectID)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, ErrNoActiveSession
	} else if err != nil {
		return nil, translate(err)
	}
	pw, err := s.worker(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if err := s.ensureSession(ctx, pw, rec); err != nil {
		return nil, err
	}
	if err := pw.w.ChangeDirectory(ctx, worker.ChangeDirectoryRequest{ProjectID: projectID, Path: path}); err != nil {
		we := worker.AsWorkerError(err)
		pw.recordErr(we, s.d.Now())
		if we.Code == worker.CodeSessionLost {
			s.markLost(ctx, rec, "provider reports the session no longer exists")
		}
		return nil, we
	}
	info, err := pw.w.Session(ctx)
	if err != nil || info.WorkingDirectory == nil {
		return nil, worker.NewError(worker.CodeProtocol, worker.DeliveryReceived, "worker did not report the confirmed directory", nil)
	}
	now := s.d.Now()
	if err := s.d.Sessions.UpdateWorkingDirectory(ctx, projectID, rec.ID, info.WorkingDirectory, now); err != nil {
		return nil, translate(err)
	}
	id := rec.ID
	s.event(ctx, projectID, &id, nil, worker.EventCWDChanged, map[string]any{"working_directory": *info.WorkingDirectory})
	return s.d.Sessions.Get(ctx, projectID, rec.ID)
}

// ConfirmSession reconciles the project's active session with the worker
// (Phase 14 same-session validation, WBS 14.19): it never creates or
// resets a session and never sends a request. A lost session is marked
// LOST; the returned record is the persisted state afterwards.
func (s *WorkerService) ConfirmSession(ctx context.Context, projectID uuid.UUID) (*persistence.WorkerSessionRecord, worker.Reconciliation, error) {
	rec, err := s.d.Sessions.GetActiveForProject(ctx, projectID)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, worker.ReconcileUnknown, ErrNoActiveSession
	} else if err != nil {
		return nil, worker.ReconcileUnknown, translate(err)
	}
	pw, err := s.worker(ctx, projectID)
	if err != nil {
		return rec, worker.ReconcileUnknown, err
	}
	res, err := s.reconcile(ctx, pw, rec)
	if err != nil {
		return rec, worker.ReconcileUnknown, err
	}
	out, err := s.d.Sessions.Get(ctx, projectID, rec.ID)
	if err != nil {
		return rec, res, translate(err)
	}
	return out, res, nil
}

// Readiness reports whether the project may dispatch (WBS 12.27). It never
// creates or resets a session.
func (s *WorkerService) Readiness(ctx context.Context, projectID uuid.UUID) (worker.SessionReadiness, error) {
	ps, err := s.d.ProjectSettings.Get(ctx, projectID.String())
	if err != nil {
		return worker.SessionReadiness{}, translate(err)
	}
	connected := false
	if pw := s.existing(projectID); pw != nil {
		connected = pw.state().Healthy()
	}
	var status *worker.SessionStatus
	var cwd *string
	if rec, err := s.d.Sessions.GetActiveForProject(ctx, projectID); err == nil {
		st := rec.Status
		status, cwd = &st, rec.WorkingDirectory
	} else if !errors.Is(err, persistence.ErrNotFound) {
		return worker.SessionReadiness{}, translate(err)
	}
	return worker.EvaluateReadiness(connected, status, cwd, ps.WorkingDirectory), nil
}

// Sessions returns the project's session history (oldest first).
func (s *WorkerService) Sessions(ctx context.Context, projectID uuid.UUID) ([]persistence.WorkerSessionRecord, error) {
	out, err := s.d.Sessions.ListForProject(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// Session returns one project session.
func (s *WorkerService) Session(ctx context.Context, projectID, id uuid.UUID) (*persistence.WorkerSessionRecord, error) {
	out, err := s.d.Sessions.Get(ctx, projectID, id)
	if err != nil {
		return nil, translate(err)
	}
	return out, nil
}

// ActiveSession returns the project's ACTIVE session.
func (s *WorkerService) ActiveSession(ctx context.Context, projectID uuid.UUID) (*persistence.WorkerSessionRecord, error) {
	rec, err := s.d.Sessions.GetActiveForProject(ctx, projectID)
	if errors.Is(err, persistence.ErrNotFound) {
		return nil, ErrNoActiveSession
	}
	if err != nil {
		return nil, translate(err)
	}
	return rec, nil
}
