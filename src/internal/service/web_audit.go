package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"overseer/internal/access"
	"overseer/internal/audit"
	"overseer/internal/persistence"
	"overseer/internal/readmodel"
)

// AuditService records and reads human / API-originated control-plane
// actions (WBS 17.103–17.106, P17-39 / P17-41).
type AuditService struct {
	repo     persistence.AuditRepository
	projects persistence.ProjectRepository
	redact   *access.Redactor
	now      func() time.Time
}

// NewAuditService builds the service; redact may be nil (no known secret
// values; secret-like keys are still redacted).
func NewAuditService(repo persistence.AuditRepository, projects persistence.ProjectRepository, redact *access.Redactor) *AuditService {
	return &AuditService{repo: repo, projects: projects, redact: redact, now: time.Now}
}

// RecordAudit validates, redacts and persists one event. The project name
// is captured at record time so the history survives renames / deletes.
func (s *AuditService) RecordAudit(ctx context.Context, e audit.Event) error {
	if e.At.IsZero() {
		e.At = s.now().UTC()
	}
	if e.Metadata != nil {
		if m, ok := s.redact.Value(e.Metadata).(map[string]any); ok {
			e.Metadata = m
		}
	}
	e.Reason = s.redact.String(e.Reason)
	if e.ProjectID != nil && e.ProjectName == "" && s.projects != nil {
		if p, err := s.projects.Get(ctx, e.ProjectID.String()); err == nil {
			e.ProjectName = p.Name
		}
	}
	if err := e.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.repo.AppendAudit(ctx, nil, &e); err != nil {
		return translate(err)
	}
	return nil
}

// List returns one page (WBS 17.104): filtering and paging are server-side.
func (s *AuditService) List(ctx context.Context, f audit.Filter) (readmodel.AuditPage, error) {
	if err := f.Validate(); err != nil {
		return readmodel.AuditPage{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	rows, err := s.repo.ListAudit(ctx, f)
	if err != nil {
		return readmodel.AuditPage{}, translate(err)
	}
	page := readmodel.AuditPage{Rows: make([]readmodel.AuditRow, 0, len(rows))}
	for i := range rows {
		page.Rows = append(page.Rows, s.row(&rows[i]))
	}
	if len(rows) == f.Limit && len(rows) > 0 {
		page.NextCursor = audit.EncodeCursor(rows[len(rows)-1].Seq)
	}
	return page, nil
}

// Get returns one event's detail (WBS 17.105, 17.142).
func (s *AuditService) Get(ctx context.Context, id uuid.UUID) (*readmodel.AuditRow, error) {
	e, err := s.repo.GetAudit(ctx, id)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, translate(err)
	}
	row := s.row(e)
	return &row, nil
}

func idStr(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func (s *AuditService) row(e *audit.Event) readmodel.AuditRow {
	md, _ := s.redact.Value(e.Metadata).(map[string]any)
	if md == nil {
		md = map[string]any{}
	}
	return readmodel.AuditRow{ID: e.ID.String(), Seq: e.Seq, At: e.At.UTC(), ProjectID: idStr(e.ProjectID), ProjectName: e.ProjectName,
		ExecutionID: idStr(e.ExecutionID), PhaseID: idStr(e.PhaseID), WorkUnitID: idStr(e.WorkUnitID),
		ActorID: e.Actor.ID, ActorName: e.Actor.Name, ActorType: string(e.Actor.Type), Action: string(e.Action),
		ResourceKind: e.ResourceKind, ResourceID: e.ResourceID, Result: string(e.Result), HTTPStatus: e.HTTPStatus,
		Reason: s.redact.String(e.Reason), RequestID: e.RequestID, Method: e.Method, Path: e.Path, Metadata: md}
}
