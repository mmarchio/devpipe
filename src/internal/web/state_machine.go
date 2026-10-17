// P6-25 (WBS 6.35): state-machine CRUD REST API. GET current machine,
// PUT (publishes a new version, stale expected_version conflicts), GET
// version list, GET version detail. Stable JSON DTOs; the domain snapshot
// is never serialized directly.
package web

import (
	"errors"
	"net/http"
	"strconv"

	"overseer/internal/service"
)

// machineStateResource is one state of the machine definition view.
type machineStateResource struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Terminal bool   `json:"terminal"`
	Enabled  bool   `json:"enabled"`
}

// machineTransitionResource is one transition of the machine definition
// view, addressed by state key (never row ids).
type machineTransitionResource struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Outcome string `json:"outcome"`
}

// machineResource is the stable JSON view of one machine version
// (WBS 6.35): the definition plus its version and configuration hash.
type machineResource struct {
	MachineID    string                      `json:"machine_id"`
	Name         string                      `json:"name"`
	Version      int                         `json:"version"`
	Hash         string                      `json:"hash"`
	InitialState string                      `json:"initial_state"`
	States       []machineStateResource      `json:"states"`
	Transitions  []machineTransitionResource `json:"transitions"`
}

func toMachineResource(m *service.CurrentMachine) machineResource {
	out := machineResource{
		MachineID:    m.MachineID.String(),
		Name:         m.Name,
		Version:      m.Version,
		Hash:         m.Hash,
		InitialState: m.InitialState.Key,
		States:       make([]machineStateResource, 0, len(m.States)),
		Transitions:  make([]machineTransitionResource, 0, len(m.Transitions)),
	}
	for _, s := range m.States {
		out.States = append(out.States, machineStateResource{
			Key: s.Key, Name: s.Name, Kind: s.Kind,
			Terminal: s.Terminal, Enabled: s.Enabled,
		})
	}
	for _, tr := range m.Transitions {
		out.Transitions = append(out.Transitions, machineTransitionResource{
			From: tr.From, To: tr.To, Outcome: tr.Outcome,
		})
	}
	return out
}

// machineValidationIssue is one structured validation issue (WBS 6.38).
// It mirrors the P6-16 report field-for-field; the service's stable
// ordering is preserved verbatim.
type machineValidationIssue struct {
	Code     string `json:"code"`
	StateKey string `json:"state_key,omitempty"`
	Detail   string `json:"detail"`
}

// machineValidationResource is the stable JSON view of the validation
// report (WBS 6.38): {valid, errors[]} — one structured issue per
// finding, all at once, never null (empty array when clean).
type machineValidationResource struct {
	Valid  bool                     `json:"valid"`
	Errors []machineValidationIssue `json:"errors"`
}
type machineVersionResource struct {
	Version   int    `json:"version"`
	MachineID string `json:"machine_id"`
	Enabled   bool   `json:"enabled"`
}

// machineEditRequest is the PUT state-machine payload (WBS 6.35).
type machineEditRequest struct {
	InitialState string `json:"initial_state"`
	States       []struct {
		Key  string `json:"key"`
		Kind string `json:"kind"`
	} `json:"states"`
	Transitions []struct {
		From    string `json:"from"`
		Outcome string `json:"outcome"`
		To      string `json:"to"`
	} `json:"transitions"`
	ExpectedVersion *int `json:"expected_version"`
}

// allowedMachineFields is the closed field set for the PUT payload.
var allowedMachineFields = map[string]bool{
	"initial_state": true, "states": true, "transitions": true, "expected_version": true,
}

func machineServiceError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_state_machine", "State machine definition is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("state machine")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("state machine version conflict (stale expected_version)")
	case errors.Is(err, service.ErrInfrastructure):
		return NewInternalError("state machine operation failed")
	default:
		return NewInternalError("unexpected error")
	}
}

type machineAPI struct {
	svc *service.StateMachineCRUDService
}

// RegisterStateMachineRoutes exposes the state-machine CRUD endpoints
// (WBS 6.35):
//
//	GET  /api/v1/projects/{project}/state-machine
//	PUT  /api/v1/projects/{project}/state-machine
//	GET  /api/v1/projects/{project}/state-machine/versions
//	GET  /api/v1/projects/{project}/state-machine/versions/{version}
func RegisterStateMachineRoutes(r *Router, svc *service.StateMachineCRUDService) error {
	api := &machineAPI{svc: svc}
	routes := []struct {
		method  string
		pattern string
		h       http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{project}/state-machine", api.handleGet},
		{http.MethodPut, "/api/v1/projects/{project}/state-machine", api.handlePut},
		{http.MethodGet, "/api/v1/projects/{project}/state-machine/versions", api.handleListVersions},
		{http.MethodGet, "/api/v1/projects/{project}/state-machine/versions/{version}", api.handleGetVersion},
		{http.MethodGet, "/api/v1/projects/{project}/state-machine/validation", api.handleValidation},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func (a *machineAPI) writeMachine(w http.ResponseWriter, m *service.CurrentMachine) {
	writeJSON(w, http.StatusOK, toMachineResource(m))
}

// handleGet — T6.35.1: current machine (the family's MAX-version).
func (a *machineAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	m, err := a.svc.Get(r.Context(), r.PathValue("project"))
	if err != nil {
		WriteError(w, r, machineServiceError(err))
		return
	}
	a.writeMachine(w, m)
}

// handlePut — T6.35.2/T6.35.4/T6.35.5: publish a new version; invalid
// definitions 400, stale expected_version 409.
func (a *machineAPI) handlePut(w http.ResponseWriter, r *http.Request) {
	var req machineEditRequest
	if err := decodeStrictJSON(r, "invalid_state_machine", allowedMachineFields,
		[]string{"initial_state", "states", "transitions"}, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := &service.MachineEditInput{InitialStateKey: req.InitialState}
	for _, st := range req.States {
		in.States = append(in.States, service.MachineStateDef{Key: st.Key, Kind: st.Kind})
	}
	for _, tr := range req.Transitions {
		in.Transitions = append(in.Transitions, service.MachineTransitionDef{
			From: tr.From, Outcome: tr.Outcome, To: tr.To,
		})
	}
	m, err := a.svc.Edit(r.Context(), r.PathValue("project"), in, req.ExpectedVersion)
	if err != nil {
		WriteError(w, r, machineServiceError(err))
		return
	}
	a.writeMachine(w, m)
}

// handleListVersions — T6.35.3: the family's stored versions, ascending.
func (a *machineAPI) handleListVersions(w http.ResponseWriter, r *http.Request) {
	list, err := a.svc.ListVersions(r.Context(), r.PathValue("project"))
	if err != nil {
		WriteError(w, r, machineServiceError(err))
		return
	}
	out := make([]machineVersionResource, 0, len(list))
	for _, v := range list {
		out = append(out, machineVersionResource{
			Version: v.Version, MachineID: v.MachineID, Enabled: v.Enabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": out})
}

// handleGetVersion — T6.35.3: one stored version's definition.
// handleValidation — T6.38.1–T6.38.4: GET /validation returns the
// P6-16 report for the project's current machine: {valid, errors[]}
// with structured issue codes, all issues at once, deterministic
// ordering, and no mutation. Unknown project → 404.
func (a *machineAPI) handleValidation(w http.ResponseWriter, r *http.Request) {
	rep, err := a.svc.Validate(r.Context(), r.PathValue("project"))
	if err != nil {
		WriteError(w, r, machineServiceError(err))
		return
	}
	out := machineValidationResource{Valid: rep.Valid, Errors: make([]machineValidationIssue, 0, len(rep.Errors))}
	for _, e := range rep.Errors {
		out.Errors = append(out.Errors, machineValidationIssue{
			Code: e.Code, StateKey: e.StateKey, Detail: e.Detail,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *machineAPI) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil {
		WriteError(w, r, NewValidationError("version must be a positive integer"))
		return
	}
	if version < 1 {
		WriteError(w, r, NewValidationError("version must be a positive integer"))
		return
	}
	m, err := a.svc.GetVersion(r.Context(), r.PathValue("project"), version)
	if err != nil {
		WriteError(w, r, machineServiceError(err))
		return
	}
	a.writeMachine(w, m)
}
