package web

import (
	"errors"
	"math"
	"net/http"

	"github.com/google/uuid"

	"overseer/internal/orchestration"
	"overseer/internal/persistence"
	"overseer/internal/service"
)

// summaryPhase is the stable JSON shape of the summary's current phase
// (WBS 3.21), deliberately separate from the domain struct.
type summaryPhase struct {
	ID     string `json:"id"`
	Order  int    `json:"order"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// summaryProgress is the stable JSON shape of the progress block (WBS
// 3.21/3.27). "remaining" completes the WBS 3.27 shape {completed, total,
// remaining, percentage}; percentage is rounded to two decimals.
type summaryProgress struct {
	Completed  int     `json:"completed"`
	Total      int     `json:"total"`
	Remaining  int     `json:"remaining"`
	Percentage float64 `json:"percentage"`
}

// projectSummary is the stable JSON shape of the project summary resource
// (WBS 3.21): project lifecycle state, current phase, and WBS progress.
// phase is null when the project has no current phase.
type projectSummary struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Status   string          `json:"status"`
	Phase    *summaryPhase   `json:"phase"`
	Progress summaryProgress `json:"progress"`
}

type summaryAPI struct {
	projects persistence.ProjectRepository
	progress *orchestration.PhaseProgressService
}

// RegisterSummaryRoutes exposes the summary endpoint (WBS 3.21):
// GET /api/v1/projects/{id}/summary.
func RegisterSummaryRoutes(r *Router, projects persistence.ProjectRepository, progress *orchestration.PhaseProgressService) error {
	api := &summaryAPI{projects: projects, progress: progress}
	if err := r.RegisterAPI(http.MethodGet, "/api/v1/projects/{id}/summary", api.handleSummary); err != nil {
		return err
	}
	return nil
}

// handleSummary returns the project's lifecycle state, current phase, and
// WBS progress (WBS 3.21). Unknown projects are 404. Without an active
// plan, progress is deterministic zero values (T3.21.4).
func (a *summaryAPI) handleSummary(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		WriteError(w, r, NewValidationError("invalid project id"))
		return
	}
	p, err := a.projects.Get(r.Context(), id.String())
	if err != nil {
		WriteError(w, r, summaryServiceError(err))
		return
	}
	snap, err := a.progress.CurrentPhaseProgress(r.Context(), id)
	if err != nil {
		WriteError(w, r, summaryServiceError(err))
		return
	}
	out := projectSummary{
		ID:     p.ID.String(),
		Name:   p.Name,
		Status: p.Status.String(),
	}
	if snap.CurrentPhase != nil {
		cp := snap.CurrentPhase
		out.Phase = &summaryPhase{
			ID:     cp.ID.String(),
			Order:  cp.Order,
			Name:   cp.Name,
			Status: cp.Status.String(),
		}
	}
	out.Progress = summaryProgress{
		Completed:  snap.Progress.Completed,
		Total:      snap.Progress.Total,
		Remaining:  snap.Progress.Remaining,
		Percentage: round2(snap.Progress.Percentage),
	}
	writeJSON(w, http.StatusOK, out)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// summaryServiceError maps the persistence/service error set onto the
// summary surface (404 unknown project, 500 unexpected).
func summaryServiceError(err error) error {
	switch {
	case errors.Is(err, persistence.ErrNotFound), errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("project")
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationError("summary request is invalid")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("summary conflict")
	default:
		return NewInternalError("summary operation failed")
	}
}
