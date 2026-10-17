package web

import (
	"net/http"
	"time"

	"errors"
	"overseer/internal/project"
	"overseer/internal/service"
)

// settingsResource is the stable JSON representation of project settings
// (WBS 3.15). It is deliberately separate from the domain struct. Future
// provider API keys are not settings fields and never appear here — the
// response exposes only non-secret configuration (T3.15.6).
type settingsResource struct {
	WorkingDirectory    string  `json:"working_directory"`
	WorkerProvider      string  `json:"worker_provider"`
	WorkerModel         string  `json:"worker_model"`
	CompressionProvider string  `json:"compression_provider"`
	CompressionModel    string  `json:"compression_model"`
	ContextWindow       int     `json:"context_window"`
	ContextThreshold    float64 `json:"context_threshold"`
	AutoContinue        bool    `json:"auto_continue"`
	MaxRetries          int     `json:"max_retries"`
	WorkerTimeout       string  `json:"worker_timeout"`
	// Version is the current project version (WBS 3.24); clients echo it
	// back on PUT.
	Version int `json:"version"`
}

func toSettingsResource(s *project.Settings) settingsResource {
	return settingsResource{
		WorkingDirectory:    s.WorkingDirectory,
		WorkerProvider:      s.WorkerProvider,
		WorkerModel:         s.WorkerModel,
		CompressionProvider: s.CompressionProvider,
		CompressionModel:    s.CompressionModel,
		ContextWindow:       s.ContextWindow,
		ContextThreshold:    s.ContextThreshold,
		AutoContinue:        s.AutoContinue,
		MaxRetries:          s.MaxRetries,
		WorkerTimeout:       s.WorkerTimeout.String(),
	}
}

// settingsUpdateRequest is the full-state PUT body (WBS 3.15: PUT semantics
// replace the settings). The field set is closed; unknown fields are 400.
type settingsUpdateRequest struct {
	WorkingDirectory    string  `json:"working_directory"`
	WorkerProvider      string  `json:"worker_provider"`
	WorkerModel         string  `json:"worker_model"`
	CompressionProvider string  `json:"compression_provider"`
	CompressionModel    string  `json:"compression_model"`
	ContextWindow       int     `json:"context_window"`
	ContextThreshold    float64 `json:"context_threshold"`
	AutoContinue        bool    `json:"auto_continue"`
	MaxRetries          int     `json:"max_retries"`
	WorkerTimeout       string  `json:"worker_timeout"`
	// Version is the project version the client read (WBS 3.24); a stale
	// value is a 409 conflict.
	Version int `json:"version"`
}

// requiredSettingsFields must all be present on a settings payload (PUT
// replaces the full state, so a missing key is an incomplete state).
var requiredSettingsFields = map[string]bool{
	"working_directory":    true,
	"worker_provider":      true,
	"worker_model":         true,
	"compression_provider": true,
	"compression_model":    true,
	"context_window":       true,
	"context_threshold":    true,
	"auto_continue":        true,
	"max_retries":          true,
	"worker_timeout":       true,
	"version":              true,
}

var requiredSettingsOrder = []string{
	"working_directory", "worker_provider", "worker_model",
	"compression_provider", "compression_model", "context_window",
	"context_threshold", "auto_continue", "max_retries", "worker_timeout",
	"version",
}

func decodeSettingsUpdate(r *http.Request) (*settingsUpdateRequest, error) {
	var out settingsUpdateRequest
	if err := decodeStrictJSON(r, "invalid_project_settings", requiredSettingsFields, requiredSettingsOrder, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type settingsAPI struct {
	svc *service.SettingsService
}

// RegisterSettingsRoutes exposes the settings REST endpoints (WBS 3.15):
// GET and PUT /api/v1/projects/{id}/settings.
func RegisterSettingsRoutes(r *Router, svc *service.SettingsService) error {
	api := &settingsAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{id}/settings", api.handleGet},
		{http.MethodPut, "/api/v1/projects/{id}/settings", api.handlePut},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func (a *settingsAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	s, err := a.svc.GetSettings(r.Context(), r.PathValue("id"))
	if err != nil {
		WriteError(w, r, settingsServiceError(err))
		return
	}
	out := toSettingsResource(s)
	// The settings response carries the project version so clients can
	// guard their PUT (WBS 3.24); a missing project row would already be a
	// 404 from GetSettings.
	var v int
	if p, perr := a.svc.GetProjectVersion(r.Context(), r.PathValue("id")); perr == nil {
		v = p
	}
	out.Version = v
	writeJSON(w, http.StatusOK, out)
}

func (a *settingsAPI) handlePut(w http.ResponseWriter, r *http.Request) {
	req, err := decodeSettingsUpdate(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	timeout, err := time.ParseDuration(req.WorkerTimeout)
	if err != nil {
		WriteError(w, r, NewValidationFields("invalid_project_settings", "Project settings are invalid.", map[string]string{"worker_timeout": "must be a Go duration, e.g. \"30m\""}))
		return
	}
	// Fetch the current settings first: an unknown project is a 404 even
	// for a valid PUT body, and the existing row is the state the PUT
	// replaces in full.
	cur, err := a.svc.GetSettings(r.Context(), r.PathValue("id"))
	if err != nil {
		WriteError(w, r, settingsServiceError(err))
		return
	}
	st := &project.Settings{
		ProjectID:           cur.ProjectID,
		WorkingDirectory:    req.WorkingDirectory,
		WorkerProvider:      req.WorkerProvider,
		WorkerModel:         req.WorkerModel,
		CompressionProvider: req.CompressionProvider,
		CompressionModel:    req.CompressionModel,
		ContextWindow:       req.ContextWindow,
		ContextThreshold:    req.ContextThreshold,
		AutoContinue:        req.AutoContinue,
		MaxRetries:          req.MaxRetries,
		WorkerTimeout:       timeout,
	}
	updated, err := a.svc.UpdateSettings(r.Context(), st, req.Version)
	if err != nil {
		WriteError(w, r, settingsServiceError(err))
		return
	}
	out := toSettingsResource(updated)
	out.Version = req.Version + 1
	writeJSON(w, http.StatusOK, out)
}

// settingsServiceError maps service-level errors onto web error responses for
// the settings surface (400 / 404 / 409 / 500).
func settingsServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_project_settings", "Project settings are invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("project")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("protected setting change while running")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("settings operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}
