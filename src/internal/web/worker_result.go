// Phase 13: the worker result REST surface.
//
//	GET  /api/v1/projects/{project}/worker/results                      (WBS 13.67, filters + pagination)
//	GET  /api/v1/projects/{project}/worker/results/{result}
//	GET  /api/v1/projects/{project}/worker/results/current              (WBS 13.70 read model)
//	GET  /api/v1/projects/{project}/worker/results/metrics              (WBS 13.62/13.63)
//	GET  /api/v1/projects/{project}/worker/results/readiness            (WBS 13.97)
//	GET  /api/v1/projects/{project}/worker/parse-attempts               (WBS 13.68)
//	GET  /api/v1/projects/{project}/worker/parse-attempts/{attempt}
//	POST /api/v1/projects/{project}/worker/dispatches/{dispatch}/reparse (WBS 13.69, stored response only)
//	GET  /api/v1/projects/{project}/settings/result-protocol            (WBS 13.96)
//	PUT  /api/v1/projects/{project}/settings/result-protocol
//	POST /api/v1/projects/{project}/settings/result-protocol/instructions (ensure the protocol instructions, WBS 13.7)
package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/service"
	"overseer/internal/workerresult"
)

type resultAPI struct {
	svc *service.ResultService
}

// RegisterWorkerResultRoutes exposes the Phase 13 endpoints.
func RegisterWorkerResultRoutes(r *Router, svc *service.ResultService) error {
	api := &resultAPI{svc: svc}
	base := "/api/v1/projects/{project}"
	routes := []struct {
		method, pattern string
		h               http.HandlerFunc
	}{
		{http.MethodGet, base + "/worker/results", api.list},
		{http.MethodGet, base + "/worker/results/current", api.current},
		{http.MethodGet, base + "/worker/results/metrics", api.metrics},
		{http.MethodGet, base + "/worker/results/readiness", api.readiness},
		{http.MethodGet, base + "/worker/results/{result}", api.get},
		{http.MethodGet, base + "/worker/parse-attempts", api.attempts},
		{http.MethodGet, base + "/worker/parse-attempts/{attempt}", api.attempt},
		{http.MethodPost, base + "/worker/dispatches/{dispatch}/reparse", api.reparse},
		{http.MethodGet, base + "/settings/result-protocol", api.getSettings},
		{http.MethodPut, base + "/settings/result-protocol", api.putSettings},
		{http.MethodPost, base + "/settings/result-protocol/instructions", api.ensure},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func resultError(err error) error {
	var se *workerresult.SettingsError
	switch {
	case errors.As(err, &se):
		return NewValidationFields("invalid_result_protocol_settings", "Result protocol settings are invalid.", se.Fields)
	case errors.Is(err, service.ErrDispatchNotCompleted):
		return NewConflictCode("dispatch_not_completed", "The dispatch has no completed worker response.")
	case errors.Is(err, workerresult.ErrBindingMismatch):
		return NewConflictCode("result_binding_mismatch", "The stored response does not belong to the dispatch.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("resource")
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_request", "Request is invalid.")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("worker result conflict")
	}
	return NewInternalError("worker result request failed")
}

func strs(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func toValidationErrors(es []workerresult.ValidationError) []map[string]string {
	out := make([]map[string]string, 0, len(es))
	for _, e := range es {
		out = append(out, map[string]string{"field": e.Field, "code": e.Code, "message": e.Message})
	}
	return out
}

// toResultDTO renders an accepted result. Worker-authored content is
// labelled as a worker report; next_action is advisory; claims are claims.
func toResultDTO(s *persistence.StoredWorkerResult) map[string]any {
	r := s.Result
	m, p := r.Metadata, r.Provenance
	var tests any
	if t := r.Tests; t != nil {
		var passed any
		if t.ReportedPassed != nil {
			passed = *t.ReportedPassed
		}
		tests = map[string]any{"reported_run": t.ReportedRun, "reported_passed": passed, "summary": t.Summary, "source": "worker-reported"}
	}
	arts := make([]map[string]string, 0, len(r.Artifacts))
	for _, a := range r.Artifacts {
		arts = append(arts, map[string]string{"path": a.Path, "action": string(a.Action), "previous_path": a.PreviousPath})
	}
	claims := make([]map[string]any, 0, len(r.AcceptanceClaims))
	for _, c := range r.AcceptanceClaims {
		claims = append(claims, map[string]any{"id": c.ID, "status": c.Status, "evidence": c.Evidence, "recognized": c.Recognized, "source": "worker-reported"})
	}
	var method any
	if p.RepairMethod != nil {
		method = *p.RepairMethod
	}
	v := workerresult.BuildView(r, "")
	out := map[string]any{
		"id": s.ID.String(), "dispatch_id": m.DispatchID.String(), "request_id": m.RequestID.String(), "session_id": m.SessionID.String(),
		"phase_id": uuidPtr(m.PhaseID), "work_unit_id": uuidPtr(m.WorkUnitID), "state": m.StateKey, "role": m.RoleKey,
		"protocol_version": r.ProtocolVersion.String(), "status": r.Status, "summary": r.Summary,
		"completed": strs(r.Completed), "remaining": strs(r.Remaining), "blocking": strs(r.Blocking),
		"next_action": map[string]string{"text": r.NextAction, "label": "worker-reported, advisory"},
		"tests":       tests, "artifacts": arts, "acceptance_claims": claims,
		"raw_response_hash": m.RawResponseHash, "normalized_hash": r.NormalizedHash,
		"provenance": map[string]any{"parse_mode": p.Mode, "repair_mode": p.RepairMode, "extraction": p.Extraction, "repaired": p.Repaired,
			"repair_method": method, "parser_version": p.ParserVersion, "parsed_hash": p.ParsedHash},
		"worker_report": v.StatusLabel, "validation": v.ValidationLabel,
		"parsed_at": rfc(m.ParsedAt), "created_at": rfc(s.CreatedAt),
	}
	switch {
	case r.IsComplete():
		c, _ := r.Completion()
		out["completion_report"] = map[string]any{"summary": c.Summary, "completed": strs(c.Completed), "test_claim": tests, "artifacts": arts, "acceptance_claims": claims}
	case r.IsIncomplete():
		c, _ := r.Continuation()
		out["continuation_status"] = map[string]any{"summary": c.Summary, "completed": strs(c.Completed), "remaining": strs(c.Remaining),
			"blocking": strs(c.Blocking), "next_action": c.NextAction}
	case r.IsBlocked():
		b, _ := r.Blocked()
		out["blocked_status"] = map[string]any{"summary": b.Summary, "blocking": strs(b.Blocking), "remaining": strs(b.Remaining), "next_action": b.NextAction}
	case r.IsFailed():
		f, _ := r.Failure()
		out["failure_status"] = map[string]any{"summary": f.Summary, "blocking": strs(f.Blocking), "next_action": f.NextAction}
	}
	return out
}

func toParseAttemptDTO(a *persistence.ParseAttemptRecord) map[string]any {
	var method any
	if a.RepairMethod != nil {
		method = *a.RepairMethod
	}
	return map[string]any{
		"id": a.ID.String(), "dispatch_id": a.DispatchID.String(), "outcome": a.Outcome, "error_code": a.ErrorCode,
		"validation_errors": toValidationErrors(a.ValidationErrors), "result_status": a.ResultStatus, "normalized_hash": a.NormalizedHash,
		"result_id": uuidPtr(a.ResultID), "differs_from_accepted": a.DiffersFromAccepted, "raw_response_hash": a.RawResponseHash,
		"parse_mode": a.ParseMode, "repair_mode": a.RepairMode, "repaired": a.Repaired, "repair_method": method, "parser_version": a.ParserVersion,
		"reparse": a.Reparse, "role": a.RoleKey, "state": a.StateKey, "worker_provider": a.WorkerProvider, "worker_model": a.WorkerModel,
		"started_at": rfc(a.StartedAt), "completed_at": rfc(a.CompletedAt),
	}
}

func paging(r *http.Request) (int, int, error) {
	vals := [2]int{}
	for i, name := range []string{"limit", "offset"} {
		if v := r.URL.Query().Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return 0, 0, NewValidationCode("invalid_filter", name+" must be a non-negative integer")
			}
			vals[i] = n
		}
	}
	return vals[0], vals[1], nil
}

func (a *resultAPI) list(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q := r.URL.Query()
	f := persistence.WorkerResultFilter{ProjectID: ids[0], StateKey: q.Get("state"), RoleKey: q.Get("role"), Status: workerresult.WorkerResultStatus(q.Get("status"))}
	if f.Status != "" && !f.Status.Valid() {
		WriteError(w, r, NewValidationCode("invalid_filter", "unknown result status"))
		return
	}
	for name, dst := range map[string]**uuid.UUID{"phase": &f.PhaseID, "work_unit": &f.WorkUnitID, "session": &f.SessionID} {
		if *dst, err = optUUID(r, name); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	if f.Limit, f.Offset, err = paging(r); err != nil {
		WriteError(w, r, err)
		return
	}
	rs, total, err := a.svc.Results(r.Context(), f)
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	out := make([]map[string]any, 0, len(rs))
	for i := range rs {
		out = append(out, toResultDTO(&rs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out, "count": len(out), "total": total, "offset": f.Offset})
}

func (a *resultAPI) get(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "result")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.svc.Result(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	writeJSON(w, http.StatusOK, toResultDTO(res))
}

func (a *resultAPI) current(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, res, err := a.svc.CurrentView(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	if v == nil {
		writeJSON(w, http.StatusOK, map[string]any{"result": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result_id": res.ID.String(), "work_unit_id": uuidPtr(res.Result.Metadata.WorkUnitID),
		"session_id": res.Result.Metadata.SessionID.String(), "worker_report": v.StatusLabel, "status": v.Status, "validation": v.ValidationLabel,
		"role": v.Role, "state": v.State, "summary": v.Summary, "completed": v.Completed, "remaining": v.Remaining, "blocking": v.Blocking,
		"blocking_label": v.BlockingLabel, "next_action": v.NextAction, "next_action_label": v.NextActionLabel, "test_claim_label": v.TestClaimLabel})
}

func complianceDTO(c workerresult.Compliance) map[string]any {
	return map[string]any{"attempts": c.Attempts, "parsed": c.Parsed, "missing": c.Missing, "malformed": c.Malformed, "ambiguous": c.Ambiguous,
		"schema_invalid": c.SchemaInvalid, "semantic_invalid": c.SemanticInvalid, "unsupported_version": c.UnsupportedVersion,
		"duplicates": c.Duplicates, "repair_attempts": c.RepairAttempts, "repair_successes": c.RepairSuccesses,
		"worker_reported_failed": c.ReportedFailed, "compliance_rate": c.ComplianceRate}
}

func groupDTO(m map[string]workerresult.Compliance) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = complianceDTO(v)
	}
	return out
}

func (a *resultAPI) metrics(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rep, err := a.svc.Compliance(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"overall": complianceDTO(rep.Overall), "by_role": groupDTO(rep.ByRole), "by_state": groupDTO(rep.ByState),
		"by_provider": groupDTO(rep.ByProvider), "by_model": groupDTO(rep.ByModel)})
}

func (a *resultAPI) readiness(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	err = a.svc.Readiness(r.Context(), ids[0])
	var re *workerresult.ReadinessError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ready": true, "problems": []string{}})
	case errors.As(err, &re):
		writeJSON(w, http.StatusOK, map[string]any{"ready": false, "problems": re.Problems})
	default:
		WriteError(w, r, resultError(err))
	}
}

func (a *resultAPI) attempts(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	f := persistence.ParseAttemptFilter{ProjectID: ids[0], Outcome: workerresult.AttemptOutcome(r.URL.Query().Get("outcome"))}
	switch f.Outcome {
	case "", workerresult.OutcomeAccepted, workerresult.OutcomeRejected, workerresult.OutcomeDuplicate:
	default:
		WriteError(w, r, NewValidationCode("invalid_filter", "unknown attempt outcome"))
		return
	}
	if f.DispatchID, err = optUUID(r, "dispatch"); err != nil {
		WriteError(w, r, err)
		return
	}
	if f.Limit, f.Offset, err = paging(r); err != nil {
		WriteError(w, r, err)
		return
	}
	as, total, err := a.svc.Attempts(r.Context(), f)
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	out := make([]map[string]any, 0, len(as))
	for i := range as {
		out = append(out, toParseAttemptDTO(&as[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempts": out, "count": len(out), "total": total, "offset": f.Offset})
}

func (a *resultAPI) attempt(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "attempt")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	at, err := a.svc.Attempt(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	writeJSON(w, http.StatusOK, toParseAttemptDTO(at))
}

func (a *resultAPI) reparse(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "dispatch")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.svc.Reparse(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	body := map[string]any{"attempt": toParseAttemptDTO(&out.Attempt), "result": nil, "error": nil}
	if out.Result != nil {
		body["result"] = toResultDTO(out.Result)
	}
	if out.Err != nil {
		body["error"] = map[string]any{"code": out.Err.Code, "errors": toValidationErrors(out.Err.Errors)}
	}
	writeJSON(w, http.StatusOK, body)
}

type resultSettingsDTO struct {
	ProtocolVersion     string `json:"protocol_version"`
	ParseMode           string `json:"parse_mode"`
	RepairMode          string `json:"repair_mode"`
	MaxResponseBytes    int    `json:"max_response_bytes"`
	MaxSummaryBytes     int    `json:"max_summary_bytes"`
	MaxItemBytes        int    `json:"max_item_bytes"`
	MaxNextActionBytes  int    `json:"max_next_action_bytes"`
	MaxCompletedItems   int    `json:"max_completed_items"`
	MaxRemainingItems   int    `json:"max_remaining_items"`
	MaxBlockingItems    int    `json:"max_blocking_items"`
	MaxArtifactItems    int    `json:"max_artifact_items"`
	MaxAcceptanceClaims int    `json:"max_acceptance_claims"`
	Stored              bool   `json:"stored"`
}

func toResultSettingsDTO(s workerresult.Settings, stored bool) resultSettingsDTO {
	l := s.Limits
	return resultSettingsDTO{ProtocolVersion: s.ProtocolVersion.String(), ParseMode: string(s.ParseMode), RepairMode: string(s.RepairMode),
		MaxResponseBytes: l.MaxResponseBytes, MaxSummaryBytes: l.MaxSummaryBytes, MaxItemBytes: l.MaxItemBytes, MaxNextActionBytes: l.MaxNextActionByte,
		MaxCompletedItems: l.MaxCompleted, MaxRemainingItems: l.MaxRemaining, MaxBlockingItems: l.MaxBlocking, MaxArtifactItems: l.MaxArtifacts,
		MaxAcceptanceClaims: l.MaxAcceptance, Stored: stored}
}

var resultSettingsFields = map[string]bool{"protocol_version": true, "parse_mode": true, "repair_mode": true, "max_response_bytes": true,
	"max_summary_bytes": true, "max_item_bytes": true, "max_next_action_bytes": true, "max_completed_items": true, "max_remaining_items": true,
	"max_blocking_items": true, "max_artifact_items": true, "max_acceptance_claims": true}

func (a *resultAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, stored, err := a.svc.ProtocolSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	writeJSON(w, http.StatusOK, toResultSettingsDTO(s, stored))
}

func (a *resultAPI) putSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	cur, _, err := a.svc.ProtocolSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	in := toResultSettingsDTO(cur, true)
	if err := decodeStrictJSON(r, "invalid_result_protocol_settings", resultSettingsFields, nil, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := workerresult.ParseProtocolVersion(in.ProtocolVersion)
	if err != nil {
		WriteError(w, r, NewValidationFields("invalid_result_protocol_settings", "Result protocol settings are invalid.",
			map[string]string{"protocol_version": "must be MAJOR.MINOR"}))
		return
	}
	st := workerresult.Settings{ProtocolVersion: v, ParseMode: workerresult.ParseMode(in.ParseMode), RepairMode: workerresult.RepairMode(in.RepairMode),
		Limits: workerresult.Limits{MaxResponseBytes: in.MaxResponseBytes, MaxSummaryBytes: in.MaxSummaryBytes, MaxItemBytes: in.MaxItemBytes,
			MaxNextActionByte: in.MaxNextActionBytes, MaxCompleted: in.MaxCompletedItems, MaxRemaining: in.MaxRemainingItems,
			MaxBlocking: in.MaxBlockingItems, MaxArtifacts: in.MaxArtifactItems, MaxAcceptance: in.MaxAcceptanceClaims}}
	out, err := a.svc.UpdateProtocolSettings(r.Context(), ids[0], st)
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	writeJSON(w, http.StatusOK, toResultSettingsDTO(out, true))
}

func (a *resultAPI) ensure(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ref, err := a.svc.EnsureProtocol(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, resultError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"protocol_id": ref.ProtocolID.String(), "version": ref.Version, "created": ref.Created, "appended": ref.Appended})
}
