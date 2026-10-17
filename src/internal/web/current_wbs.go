package web

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"overseer/internal/orchestration"
	"overseer/internal/service"
)

// currentWBSPhase is the stable JSON shape of the current phase in the
// current-wbs response (WBS 4.25): order + name only, per the WBS response
// example.
type currentWBSPhase struct {
	Order int    `json:"order"`
	Name  string `json:"name"`
}

// currentWBSWBS is the stable JSON shape of the current WBS section (WBS
// 4.25): the current version number.
type currentWBSWBS struct {
	Version int `json:"version"`
}

// currentWBSProgress is the stable JSON shape of the progress block (WBS
// 4.25): completed/total/percentage with two-decimal rounding (the
// established P4-19/P3-21 derivation convention).
type currentWBSProgress struct {
	Completed  int     `json:"completed"`
	Total      int     `json:"total"`
	Percentage float64 `json:"percentage"`
}

// currentWBSWorkUnit is the stable JSON shape of the current work unit
// section (WBS 4.25): id, name, status.
type currentWBSWorkUnit struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// currentWBSResource is the stable JSON shape of
// GET /api/v1/projects/{id}/current-wbs (WBS 4.25). phase, wbs, and
// current_work_unit are null in the defined empty/no-current response
// (T4.25.5: a project with no current phase is 200 with null sections and
// 0/0/0 progress, not an error).
type currentWBSResource struct {
	Phase           *currentWBSPhase    `json:"phase"`
	WBS             *currentWBSWBS      `json:"wbs"`
	Progress        currentWBSProgress  `json:"progress"`
	CurrentWorkUnit *currentWBSWorkUnit `json:"current_work_unit"`
}

type currentWBSAPI struct {
	readModel *service.WBSReadModelService
	progress  *orchestration.PhaseProgressService
}

// RegisterCurrentWBSRoutes exposes the current-wbs endpoint (WBS 4.25):
// GET /api/v1/projects/{id}/current-wbs. It is read-only: it composes the
// P4-18 WBS read model (phase + current WBS) with the established progress
// derivation (active plan + current eligible work unit).
func RegisterCurrentWBSRoutes(r *Router, readModel *service.WBSReadModelService, progress *orchestration.PhaseProgressService) error {
	api := &currentWBSAPI{readModel: readModel, progress: progress}
	return r.RegisterAPI(http.MethodGet, "/api/v1/projects/{id}/current-wbs", api.handleCurrentWBS)
}

// handleCurrentWBS returns the project's current phase, current WBS
// version, derived progress, and current work unit (WBS 4.25). The
// current work unit is the active plan's first eligible unit in
// ascending sequence order (nil when the active plan has none). A
// completed/phase-less project returns 200 with null phase/wbs/
// current_work_unit and 0/0/0 progress (T4.25.5). Unknown project ids are
// 404 and malformed ids 400 per the established project-scope contract.
func (a *currentWBSAPI) handleCurrentWBS(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		WriteError(w, r, NewValidationError("invalid project id"))
		return
	}
	model, err := a.readModel.WBSReadModel(r.Context(), id.String())
	if err != nil {
		WriteError(w, r, currentWBSServiceError(err))
		return
	}
	snap, err := a.progress.CurrentPhaseProgress(r.Context(), id)
	if err != nil {
		WriteError(w, r, currentWBSServiceError(err))
		return
	}
	out := currentWBSResource{
		Progress: currentWBSProgress{
			Completed:  snap.Progress.Completed,
			Total:      snap.Progress.Total,
			Percentage: round2(snap.Progress.Percentage),
		},
	}
	if model.Phase != nil {
		out.Phase = &currentWBSPhase{Order: model.Phase.Order, Name: model.Phase.Name}
	}
	if model.WBS != nil {
		out.WBS = &currentWBSWBS{Version: model.WBS.Version}
	}
	if snap.CurrentWorkUnit != nil {
		out.CurrentWorkUnit = &currentWBSWorkUnit{
			ID:     snap.CurrentWorkUnit.ID.String(),
			Name:   snap.CurrentWorkUnit.Name,
			Status: snap.CurrentWorkUnit.Status.String(),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// currentWBSServiceError maps the service-level error set onto the
// current-wbs surface (400 malformed id, 404 unknown project, 500
// unexpected).
func currentWBSServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationError("project id is invalid")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("project")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("current WBS conflict")
	default:
		return NewInternalError("current-wbs operation failed")
	}
}
