package web

import (
	"errors"
	"net/http"

	"overseer/internal/service"
)

// readinessResource is the stable JSON representation of a project
// readiness report (WBS 3.19). ready is true exactly when no requirement
// failed; checks is a per-requirement status map (each value is "ok" or
// the specific failure code), deliberately separate from the service type.
type readinessResource struct {
	Ready  bool              `json:"ready"`
	Checks map[string]string `json:"checks"`
}

// toReadinessResource maps the service report onto the stable API shape.
// The checks map names each requirement; a failing requirement carries its
// machine-readable failure code, a passing one carries "ok".
func toReadinessResource(rep *service.ReadinessReport) readinessResource {
	checks := map[string]string{
		"working_directory":    "ok",
		"worker_provider":      "ok",
		"worker_model":         "ok",
		"compression_provider": "ok",
		"compression_model":    "ok",
		"context":              "ok",
		"phases":               "ok",
		"current_wbs":          "ok",
	}
	mark := map[string]string{
		service.FailureWorkingDirectoryNotConfigured: "working_directory",
		service.FailureWorkingDirectoryMissing:       "working_directory",
		service.FailureWorkingDirectoryNotADirectory: "working_directory",
		service.FailureWorkerProviderMissing:         "worker_provider",
		service.FailureWorkerModelMissing:            "worker_model",
		service.FailureCompressionProviderMissing:    "compression_provider",
		service.FailureCompressionModelMissing:       "compression_model",
		service.FailureContextConfigurationInvalid:   "context",
		service.FailurePhaseMissing:                  "phases",
		service.FailurePhaseWBSMissing:               "current_wbs",
	}
	for _, code := range rep.Failures {
		if key, ok := mark[code]; ok {
			checks[key] = code
		}
	}
	return readinessResource{Ready: rep.Ready, Checks: checks}
}

type readinessAPI struct {
	svc *service.ReadinessService
}

// RegisterReadinessRoutes exposes the readiness endpoints (WBS 3.19):
// GET /api/v1/projects/{id}/readiness and POST /api/v1/projects/{id}/ready.
func RegisterReadinessRoutes(r *Router, svc *service.ReadinessService) error {
	api := &readinessAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{id}/readiness", api.handleCheck},
		{http.MethodPost, "/api/v1/projects/{id}/ready", api.handleReady},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

// handleCheck evaluates readiness without mutating the project (T3.19.5).
func (a *readinessAPI) handleCheck(w http.ResponseWriter, r *http.Request) {
	rep, err := a.svc.CheckReadiness(r.Context(), r.PathValue("id"))
	if err != nil {
		WriteError(w, r, readinessServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toReadinessResource(rep))
}

// handleReady transitions a readiness-complete DRAFT project to READY
// (T3.19.3); an invalid project is left untouched (T3.19.4).
func (a *readinessAPI) handleReady(w http.ResponseWriter, r *http.Request) {
	rep, err := a.svc.MarkReady(r.Context(), r.PathValue("id"))
	if err != nil {
		if rep != nil {
			writeJSON(w, statusForReadinessError(err), toReadinessResource(rep))
			return
		}
		WriteError(w, r, readinessServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toReadinessResource(rep))
}

// readinessServiceError maps service errors onto the readiness surface.
func readinessServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationError("project not ready")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("project")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("project state conflict")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("readiness operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

// statusForReadinessError maps the readiness transition error set to HTTP
// codes while still returning the structured report body.
func statusForReadinessError(err error) int {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, service.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, service.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
