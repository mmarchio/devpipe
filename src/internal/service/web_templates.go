package service

import (
	"context"
	"errors"
	"fmt"
	"overseer/internal/bounds"
	"time"

	"github.com/google/uuid"

	"overseer/internal/dbrepo"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
)

// TemplateView is a prompt template with its current version (WBS 17.48).
type TemplateView struct {
	ID             string    `json:"id"`
	Key            string    `json:"key"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Purpose        string    `json:"purpose"`
	Scope          string    `json:"scope"`
	Enabled        bool      `json:"enabled"`
	CurrentVersion int       `json:"current_version"`
	ContentHash    string    `json:"content_hash"`
	Content        string    `json:"content,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TemplateVersionView is one historical (immutable) template version.
type TemplateVersionView struct {
	Version     int       `json:"version"`
	ContentHash string    `json:"content_hash"`
	Content     string    `json:"content,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// TemplateValidationError lists every problem of a rejected template
// version (WBS 17.49: unknown variables are rejected before activation).
type TemplateValidationError struct {
	Issues []string
}

func (e *TemplateValidationError) Error() string {
	return fmt.Sprintf("template invalid: %v", e.Issues)
}

func (e *TemplateValidationError) Unwrap() error { return ErrInvalidInput }

type projectTemplateLister interface {
	ListForProject(ctx context.Context, projectID uuid.UUID) ([]*persistence.PromptTemplateRecord, error)
}

// visible loads a template visible to the project (global or own).
func (s *PromptTemplateService) visible(ctx context.Context, projectID, templateID uuid.UUID) (*persistence.PromptTemplateRecord, error) {
	rec, err := s.templates.Get(ctx, templateID)
	if err != nil {
		return nil, translate(err)
	}
	if rec.ProjectID != nil && *rec.ProjectID != projectID {
		return nil, ErrNotFound
	}
	return rec, nil
}

func (s *PromptTemplateService) view(ctx context.Context, rec *persistence.PromptTemplateRecord, withContent bool) (TemplateView, error) {
	v := TemplateView{ID: rec.ID.String(), Key: rec.Key, Name: rec.Name, Description: rec.Description, Purpose: rec.Purpose,
		Scope: "GLOBAL", Enabled: rec.Enabled, UpdatedAt: rec.UpdatedAt.UTC()}
	if rec.ProjectID != nil {
		v.Scope = "PROJECT"
	}
	cur, err := s.versions.GetCurrentVersion(ctx, rec.ID)
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return v, translate(err)
	}
	if cur != nil {
		v.CurrentVersion, v.ContentHash = cur.Version, cur.ContentHash
		if withContent {
			v.Content = cur.Content
		}
	}
	return v, nil
}

// List returns the templates loaded dynamically for the project: global
// seeds plus project templates (WBS 17.48).
func (s *PromptTemplateService) List(ctx context.Context, projectID uuid.UUID) ([]TemplateView, error) {
	l, ok := s.templates.(projectTemplateLister)
	if !ok {
		return nil, fmt.Errorf("%w: template listing unavailable", ErrInfrastructure)
	}
	recs, err := l.ListForProject(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	out := make([]TemplateView, 0, len(recs))
	for _, rec := range recs {
		v, err := s.view(ctx, rec, false)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Get returns one template with its current content.
func (s *PromptTemplateService) Get(ctx context.Context, projectID, templateID uuid.UUID) (*TemplateView, error) {
	rec, err := s.visible(ctx, projectID, templateID)
	if err != nil {
		return nil, err
	}
	v, err := s.view(ctx, rec, true)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Versions returns the immutable version history (newest first).
func (s *PromptTemplateService) Versions(ctx context.Context, projectID, templateID uuid.UUID) ([]TemplateVersionView, error) {
	if _, err := s.visible(ctx, projectID, templateID); err != nil {
		return nil, err
	}
	list, err := s.versions.ListVersions(ctx, templateID)
	if err != nil {
		return nil, translate(err)
	}
	out := make([]TemplateVersionView, 0, len(list))
	for i := len(list) - 1; i >= 0; i-- {
		v := list[i]
		out = append(out, TemplateVersionView{Version: v.Version, ContentHash: v.ContentHash, Content: v.Content, CreatedAt: v.CreatedAt.UTC()})
	}
	return out, nil
}

// ValidateContent checks syntax and variables against the registry in
// force; every problem is reported.
func ValidateTemplateContent(content string) []string {
	var issues []string
	ir, err := prompt.ParseTemplate(content)
	if err != nil {
		return []string{"SYNTAX: " + err.Error()}
	}
	for _, p := range prompt.CheckVariables(ir, prompt.TemplateVariableRegistry()) {
		issues = append(issues, "UNKNOWN_VARIABLE: "+p)
	}
	return issues
}

// CreateVersion validates content and appends a new version when the
// caller's expected version is still current (WBS 17.49, 17.140). Executions
// keep the captured template version of their snapshot.
func (s *PromptTemplateService) CreateVersion(ctx context.Context, projectID, templateID uuid.UUID, content string, expectedVersion int) (*TemplateView, error) {
	if err := bounds.Check(bounds.PromptTemplate, content); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if _, err := s.visible(ctx, projectID, templateID); err != nil {
		return nil, err
	}
	if issues := ValidateTemplateContent(content); len(issues) > 0 {
		return nil, &TemplateValidationError{Issues: issues}
	}
	appendCAS := func(versions persistence.PromptTemplateVersionRepository) error {
		cur, err := versions.GetCurrentVersion(ctx, templateID)
		if err != nil && !errors.Is(err, persistence.ErrNotFound) {
			return translate(err)
		}
		have := 0
		if cur != nil {
			have = cur.Version
		}
		if have != expectedVersion {
			return fmt.Errorf("%w: template is at version %d, not %d", ErrConflict, have, expectedVersion)
		}
		if cur != nil && cur.Content == content {
			return nil
		}
		if _, err := versions.AppendVersion(ctx, templateID, content); err != nil {
			return translate(err)
		}
		return nil
	}
	var err error
	if s.txPool == nil {
		err = appendCAS(s.versions)
	} else {
		err = dbrepo.Run(ctx, s.txPool, func(tx persistence.Transaction) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "prompt-template-versions:"+templateID.String()); err != nil {
				return translate(err)
			}
			// The rest runs in the same transaction.
			return appendCAS(s.versions.WithTx(tx))
		})
	}
	if err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, translate(err)
	}
	return s.Get(ctx, projectID, templateID)
}
