package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"overseer/internal/persistence"
	"overseer/internal/prompt"
	"overseer/internal/wbs"
	"overseer/internal/worker"
	"overseer/internal/workerresult"
)

// Phase 13 result service errors.
var (
	// ErrDispatchNotCompleted: only a COMPLETED dispatch has a raw response
	// to parse.
	ErrDispatchNotCompleted = errors.New("service: dispatch has no completed response")
)

// WorkUnitReader loads work units (acceptance criterion ids).
type WorkUnitReader interface {
	Get(ctx context.Context, id uuid.UUID) (*wbs.WorkUnit, error)
}

// ResultDeps wires the result service.
type ResultDeps struct {
	Settings   persistence.ResultProtocolSettingsRepository
	Results    persistence.WorkerResultRepository
	Attempts   persistence.ParseAttemptRepository
	Dispatches persistence.WorkerDispatchRepository
	Responses  persistence.WorkerResponseRepository
	Sessions   persistence.WorkerSessionRepository
	Units      WorkUnitReader
	Protocols  persistence.ResponseProtocolRepository
	Lookup     persistence.ResponseProtocolLookup
	Events     persistence.WorkerEventRepository
	Projects   persistence.ProjectRepository

	Logger *slog.Logger
	Now    func() time.Time
	// PersistRetries bounds retries of a transient persistence failure
	// (never a different parse, WBS 13.86).
	PersistRetries int
}

// ResultService is the Phase 13 application service: it parses stored raw
// worker responses into WorkerResults and records every attempt. It never
// sends a worker request, transitions state, or changes work units,
// ACTIVE_TASK, acceptance records or tests.
type ResultService struct {
	d ResultDeps

	observer ParseOutcomeObserver
}

// ParseOutcomeObserver is told about every parse outcome of a dispatch
// (Phase 14 consumption hook, P14-56): result is the accepted result, or
// nil when the response was rejected.
type ParseOutcomeObserver interface {
	OnParseOutcome(ctx context.Context, projectID, dispatchID uuid.UUID, result *persistence.StoredWorkerResult)
}

// WithOutcomeObserver registers the parse outcome observer.
func (s *ResultService) WithOutcomeObserver(o ParseOutcomeObserver) *ResultService {
	s.observer = o
	return s
}

// NewResultService builds the service.
func NewResultService(d ResultDeps) *ResultService {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.PersistRetries <= 0 {
		d.PersistRetries = 3
	}
	return &ResultService{d: d}
}

func (s *ResultService) project(ctx context.Context, projectID uuid.UUID) error {
	if _, err := s.d.Projects.Get(ctx, projectID.String()); err != nil {
		return translate(err)
	}
	return nil
}

// --- settings (13.96) -------------------------------------------------------

// ProtocolSettings returns the project's response-protocol settings.
func (s *ResultService) ProtocolSettings(ctx context.Context, projectID uuid.UUID) (workerresult.Settings, bool, error) {
	if err := s.project(ctx, projectID); err != nil {
		return workerresult.Settings{}, false, err
	}
	st, stored, err := s.d.Settings.Get(ctx, projectID)
	if err != nil {
		return workerresult.Settings{}, false, translate(err)
	}
	return st, stored, nil
}

// UpdateProtocolSettings validates and stores the settings.
func (s *ResultService) UpdateProtocolSettings(ctx context.Context, projectID uuid.UUID, st workerresult.Settings) (workerresult.Settings, error) {
	if err := s.project(ctx, projectID); err != nil {
		return workerresult.Settings{}, err
	}
	if err := st.Validate(); err != nil {
		return workerresult.Settings{}, err
	}
	if err := s.d.Settings.Upsert(ctx, projectID, st); err != nil {
		return workerresult.Settings{}, translate(err)
	}
	return st, nil
}

// --- instructions (13.7 / P13-33) ------------------------------------------------

// ProtocolRef identifies the response protocol version carrying the
// instructions.
type ProtocolRef struct {
	ProtocolID uuid.UUID
	Version    int
	Created    bool
	Appended   bool
}

// EnsureProtocol makes the project's IMPLEMENTATION_RESPONSE protocol carry
// the Phase 13 instructions for the configured protocol version (PLAN
// Decision 8, revised): created project-scoped when absent, otherwise a new
// version is appended when its current content differs. State pins are not
// changed (they reference a version explicitly).
func (s *ResultService) EnsureProtocol(ctx context.Context, projectID uuid.UUID) (ProtocolRef, error) {
	st, _, err := s.ProtocolSettings(ctx, projectID)
	if err != nil {
		return ProtocolRef{}, err
	}
	text := workerresult.Instructions(st.ProtocolVersion)
	pid := projectID
	rec, err := s.d.Lookup.FindByKind(ctx, &pid, string(prompt.KindImplementationResponse))
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return ProtocolRef{}, translate(err)
	}
	if rec == nil || rec.ProjectID == nil {
		now := s.d.Now()
		n := &persistence.ResponseProtocolRecord{ID: uuid.New(), ProjectID: &pid, Name: workerresult.InstructionName,
			Kind: string(prompt.KindImplementationResponse), Enabled: true, CreatedAt: now, UpdatedAt: now}
		if err := s.d.Protocols.Create(ctx, n, text); err != nil {
			return ProtocolRef{}, translate(err)
		}
		return ProtocolRef{ProtocolID: n.ID, Version: 1, Created: true}, nil
	}
	cur, err := s.d.Protocols.GetCurrentVersion(ctx, rec.ID)
	if err != nil {
		return ProtocolRef{}, translate(err)
	}
	if cur != nil && cur.Content == text {
		return ProtocolRef{ProtocolID: rec.ID, Version: cur.Version}, nil
	}
	v, err := s.d.Protocols.AppendVersion(ctx, rec.ID, text)
	if err != nil {
		return ProtocolRef{}, translate(err)
	}
	return ProtocolRef{ProtocolID: rec.ID, Version: v.Version, Appended: true}, nil
}

// instructionResolvable reports whether the protocol visible to the
// project currently carries the configured instructions.
func (s *ResultService) instructionResolvable(ctx context.Context, projectID uuid.UUID, st workerresult.Settings) bool {
	pid := projectID
	rec, err := s.d.Lookup.FindByKind(ctx, &pid, string(prompt.KindImplementationResponse))
	if err != nil || rec == nil {
		return false
	}
	cur, err := s.d.Protocols.GetCurrentVersion(ctx, rec.ID)
	return err == nil && cur != nil && cur.Content == workerresult.Instructions(st.ProtocolVersion)
}

// Readiness is the protocol readiness check (WBS 13.97): every problem
// together.
func (s *ResultService) Readiness(ctx context.Context, projectID uuid.UUID) error {
	st, _, err := s.ProtocolSettings(ctx, projectID)
	if err != nil {
		return err
	}
	return workerresult.ValidateReadiness(st, s.instructionResolvable(ctx, projectID, st), true)
}

// --- parsing (13.43-13.51, 13.86, 13.87) -------------------------------------------

// ParseOutcome is the result of one parse attempt.
type ParseOutcome struct {
	Attempt persistence.ParseAttemptRecord
	// Result is the accepted result of the dispatch (new or existing).
	Result *persistence.StoredWorkerResult
	// Err is the protocol failure (never a worker-reported failed).
	Err *workerresult.ParseError
	// Created reports that this attempt stored the accepted result.
	Created bool
}

// ParseDispatch parses the dispatch's stored raw response (never the
// worker). A protocol failure is recorded as a REJECTED attempt and returned
// in the outcome (error nil). A persistence failure returns an error and
// leaves no attempt, so recovery or reparse replays it later.
func (s *ResultService) ParseDispatch(ctx context.Context, projectID, dispatchID uuid.UUID) (*ParseOutcome, error) {
	return s.parse(ctx, projectID, dispatchID, false)
}

// Reparse re-parses the stored raw response with the current parser (WBS
// 13.47/13.69). An existing accepted result is never overwritten.
func (s *ResultService) Reparse(ctx context.Context, projectID, dispatchID uuid.UUID) (*ParseOutcome, error) {
	return s.parse(ctx, projectID, dispatchID, true)
}

func (s *ResultService) retry(ctx context.Context, op func() error) error {
	var err error
	for i := 0; i < s.d.PersistRetries; i++ {
		if err = op(); err == nil {
			return nil
		}
		var ie *persistence.InfrastructureError
		if !errors.As(err, &ie) || ctx.Err() != nil {
			return err
		}
		time.Sleep(time.Duration(i+1) * 20 * time.Millisecond)
	}
	return err
}

func (s *ResultService) parse(ctx context.Context, projectID, dispatchID uuid.UUID, reparse bool) (*ParseOutcome, error) {
	started := s.d.Now()
	d, err := s.d.Dispatches.Get(ctx, projectID, dispatchID)
	if err != nil {
		return nil, translate(err)
	}
	if d.Status != worker.DispatchCompleted {
		return nil, ErrDispatchNotCompleted
	}
	raw, err := s.d.Responses.GetByDispatch(ctx, projectID, dispatchID)
	if err != nil {
		if errors.Is(err, persistence.ErrNotFound) {
			return nil, ErrDispatchNotCompleted
		}
		return nil, translate(err)
	}
	st, _, err := s.d.Settings.Get(ctx, projectID)
	if err != nil {
		return nil, translate(err)
	}
	var known []string
	if d.WorkUnitID != nil && s.d.Units != nil {
		if u, err := s.d.Units.Get(ctx, *d.WorkUnitID); err == nil && u.Specification != nil {
			known = workerresult.AcceptanceIDs(u.Specification.AcceptanceCriteria)
		}
	}
	provider := ""
	if sess, err := s.d.Sessions.Get(ctx, projectID, d.SessionID); err == nil {
		provider = string(sess.Provider)
	}
	ids := &dispatchIDs{project: projectID, session: d.SessionID, dispatch: d.ID}
	s.event(ctx, ids, worker.EventResultParseStarted, map[string]any{"reparse": reparse, "parser_version": workerresult.ParserVersion,
		"raw_response_hash": raw.ContentHash})

	parser := workerresult.DeterministicParser{Settings: st, AcceptanceIDs: known, Now: s.d.Now}
	res, perr := parser.Parse(ctx, *raw, *d)
	att := persistence.ParseAttemptRecord{ID: uuid.New(), DispatchID: d.ID, ProjectID: projectID, RawResponseHash: raw.ContentHash,
		ParseMode: st.ParseMode, RepairMode: st.RepairMode, ParserVersion: workerresult.ParserVersion, Reparse: reparse, RoleKey: d.RoleKey,
		StateKey: d.StateKey, WorkerProvider: provider, WorkerModel: d.WorkerModel, StartedAt: started}
	existing, err := s.d.Results.GetByDispatch(ctx, projectID, d.ID)
	if err != nil && !errors.Is(err, persistence.ErrNotFound) {
		return nil, translate(err)
	}
	out := &ParseOutcome{}
	if perr != nil {
		pe, ok := workerresult.AsParseError(perr)
		if !ok {
			return nil, perr // binding / integrity — never a protocol outcome
		}
		att.Outcome, att.ErrorCode, att.ValidationErrors = workerresult.OutcomeRejected, pe.Code, pe.Errors
		att.Repaired, att.RepairMethod = pe.Provenance.Repaired, pe.Provenance.RepairMethod
		if existing != nil {
			att.ResultID, att.DiffersFromAccepted = &existing.ID, true
			out.Result = existing
		}
		out.Err = pe
	} else {
		att.ResultStatus, att.NormalizedHash = res.Status, res.NormalizedHash
		att.Repaired, att.RepairMethod = res.Provenance.Repaired, res.Provenance.RepairMethod
		if res.Provenance.Repaired {
			s.event(ctx, ids, worker.EventResultRepaired, map[string]any{"repair_method": *res.Provenance.RepairMethod})
		}
		if existing != nil {
			att.Outcome, att.ResultID = workerresult.OutcomeDuplicate, &existing.ID
			att.DiffersFromAccepted = existing.Result.NormalizedHash != res.NormalizedHash
			out.Result = existing
		} else {
			var stored *persistence.StoredWorkerResult
			var created bool
			if err := s.retry(ctx, func() error {
				var e error
				stored, created, e = s.d.Results.CreateAccepted(ctx, uuid.New(), &res, s.d.Now())
				return e
			}); err != nil {
				s.d.Logger.Error("worker result not persisted", "dispatch_id", d.ID.String(), "error", err.Error())
				return nil, translate(err)
			}
			out.Result, out.Created = stored, created
			att.ResultID = &stored.ID
			if created {
				att.Outcome = workerresult.OutcomeAccepted
			} else {
				att.Outcome = workerresult.OutcomeDuplicate
				att.DiffersFromAccepted = stored.Result.NormalizedHash != res.NormalizedHash
			}
		}
	}
	att.CompletedAt = s.d.Now()
	if err := s.retry(ctx, func() error { return s.d.Attempts.Create(ctx, &att) }); err != nil {
		return nil, translate(err)
	}
	out.Attempt = att
	s.recordOutcome(ctx, ids, d, out)
	if s.observer != nil {
		s.observer.OnParseOutcome(context.WithoutCancel(ctx), projectID, dispatchID, out.Result)
	}
	return out, nil
}

type dispatchIDs struct{ project, session, dispatch uuid.UUID }

func (s *ResultService) event(ctx context.Context, ids *dispatchIDs, typ worker.EventType, payload map[string]any) {
	if s.d.Events == nil {
		return
	}
	sid, did := ids.session, ids.dispatch
	e := &persistence.WorkerEventRecord{ID: uuid.New(), ProjectID: ids.project, SessionID: &sid, DispatchID: &did, Type: typ,
		Payload: sanitizePayload(payload), CreatedAt: s.d.Now()}
	if err := s.d.Events.Append(context.WithoutCancel(ctx), e); err != nil {
		s.d.Logger.Warn("worker result event not recorded", "type", string(typ), "error", err.Error())
	}
}

// recordOutcome emits audit events (classification only, WBS 13.64) and
// the structured log (WBS 13.65): no result text, no raw content.
func (s *ResultService) recordOutcome(ctx context.Context, ids *dispatchIDs, d *worker.WorkerDispatch, out *ParseOutcome) {
	a := out.Attempt
	args := []any{"component", "worker_result", "project_id", d.ProjectID.String(), "dispatch_id", d.ID.String(), "session_id", d.SessionID.String(),
		"state", d.StateKey, "role", d.RoleKey, "outcome", string(a.Outcome), "parser_version", a.ParserVersion, "repaired", a.Repaired,
		"reparse", a.Reparse, "parse_duration_ms", a.CompletedAt.Sub(a.StartedAt).Milliseconds()}
	if d.PhaseID != nil {
		args = append(args, "phase_id", d.PhaseID.String())
	}
	if d.WorkUnitID != nil {
		args = append(args, "work_unit_id", d.WorkUnitID.String())
	}
	if out.Err != nil {
		fields := make([]string, 0, len(out.Err.Errors))
		for _, e := range out.Err.Errors {
			fields = append(fields, e.Field+":"+e.Code)
		}
		s.event(ctx, ids, worker.EventResultRejected, map[string]any{"error_code": string(out.Err.Code), "errors": fields, "reparse": a.Reparse})
		s.d.Logger.Warn("worker result rejected", append(args, "error_code", string(out.Err.Code))...)
		return
	}
	r := out.Result.Result
	s.event(ctx, ids, worker.EventResultParsed, map[string]any{"status": string(a.ResultStatus), "protocol_version": r.ProtocolVersion.String(),
		"normalized_hash": a.NormalizedHash, "outcome": string(a.Outcome), "differs_from_accepted": a.DiffersFromAccepted})
	if out.Created {
		s.event(ctx, ids, worker.EventResultPersisted, map[string]any{"result_id": out.Result.ID.String(), "normalized_hash": r.NormalizedHash})
	}
	s.d.Logger.Info("worker result parsed", append(args, "result_status", string(a.ResultStatus), "protocol_version", r.ProtocolVersion.String())...)
}

// OnDispatchCompleted is the Phase 12 completion hook (P13-36): the
// response is parsed synchronously; failures are logged, never surfaced to
// the dispatch.
func (s *ResultService) OnDispatchCompleted(ctx context.Context, projectID, dispatchID uuid.UUID) {
	if _, err := s.ParseDispatch(context.WithoutCancel(ctx), projectID, dispatchID); err != nil {
		s.d.Logger.Warn("worker result parse deferred to recovery", "dispatch_id", dispatchID.String(), "error", err.Error())
	}
}

// RecoverUnparsed parses every COMPLETED dispatch that has no parse attempt
// (WBS 13.88). Accepted and rejected dispatches are left untouched (WBS
// 13.89/13.90): reinterpretation only happens through explicit reparse.
func (s *ResultService) RecoverUnparsed(ctx context.Context) (int, error) {
	todo, err := s.d.Attempts.ListUnparsed(ctx, 0)
	if err != nil {
		return 0, translate(err)
	}
	n := 0
	for _, u := range todo {
		if _, err := s.ParseDispatch(ctx, u.ProjectID, u.DispatchID); err != nil {
			s.d.Logger.Warn("worker result recovery failed", "dispatch_id", u.DispatchID.String(), "error", err.Error())
			continue
		}
		n++
	}
	return n, nil
}

// --- queries (13.66 / P13-38) ------------------------------------------------------

// Result returns one accepted result.
func (s *ResultService) Result(ctx context.Context, projectID, id uuid.UUID) (*persistence.StoredWorkerResult, error) {
	r, err := s.d.Results.Get(ctx, projectID, id)
	if err != nil {
		return nil, translate(err)
	}
	return r, nil
}

// ResultByDispatch returns the accepted result of a dispatch.
func (s *ResultService) ResultByDispatch(ctx context.Context, projectID, dispatchID uuid.UUID) (*persistence.StoredWorkerResult, error) {
	r, err := s.d.Results.GetByDispatch(ctx, projectID, dispatchID)
	if err != nil {
		return nil, translate(err)
	}
	return r, nil
}

// Results lists accepted results (ListByProject / Phase / WorkUnit /
// Status through the filter).
func (s *ResultService) Results(ctx context.Context, f persistence.WorkerResultFilter) ([]persistence.StoredWorkerResult, int, error) {
	if err := s.project(ctx, f.ProjectID); err != nil {
		return nil, 0, err
	}
	out, total, err := s.d.Results.List(ctx, f)
	if err != nil {
		return nil, 0, translate(err)
	}
	return out, total, nil
}

// Attempt returns one parse attempt.
func (s *ResultService) Attempt(ctx context.Context, projectID, id uuid.UUID) (*persistence.ParseAttemptRecord, error) {
	a, err := s.d.Attempts.Get(ctx, projectID, id)
	if err != nil {
		return nil, translate(err)
	}
	return a, nil
}

// Attempts lists parse attempts.
func (s *ResultService) Attempts(ctx context.Context, f persistence.ParseAttemptFilter) ([]persistence.ParseAttemptRecord, int, error) {
	if err := s.project(ctx, f.ProjectID); err != nil {
		return nil, 0, err
	}
	out, total, err := s.d.Attempts.List(ctx, f)
	if err != nil {
		return nil, 0, translate(err)
	}
	return out, total, nil
}

// --- metrics (13.62 / 13.63) --------------------------------------------------------------

// ComplianceReport is overall and grouped protocol compliance.
type ComplianceReport struct {
	Overall    workerresult.Compliance
	ByRole     map[string]workerresult.Compliance
	ByState    map[string]workerresult.Compliance
	ByProvider map[string]workerresult.Compliance
	ByModel    map[string]workerresult.Compliance
}

// Compliance aggregates the project's parse attempts using the binding
// snapshot recorded from the dispatch (never worker content).
func (s *ResultService) Compliance(ctx context.Context, projectID uuid.UUID) (ComplianceReport, error) {
	if err := s.project(ctx, projectID); err != nil {
		return ComplianceReport{}, err
	}
	var recs []workerresult.AttemptRecord
	for off := 0; ; off += 1000 {
		as, total, err := s.d.Attempts.List(ctx, persistence.ParseAttemptFilter{ProjectID: projectID, Limit: 1000, Offset: off})
		if err != nil {
			return ComplianceReport{}, translate(err)
		}
		for _, a := range as {
			recs = append(recs, workerresult.AttemptRecord{Outcome: a.Outcome, ErrorCode: a.ErrorCode, ResultStatus: a.ResultStatus, Repaired: a.Repaired,
				Reparse: a.Reparse, ProjectID: a.ProjectID.String(), Role: a.RoleKey, State: a.StateKey, Provider: a.WorkerProvider, Model: a.WorkerModel})
		}
		if off+1000 >= total {
			break
		}
	}
	return ComplianceReport{Overall: workerresult.Aggregate(recs), ByRole: workerresult.AggregateBy(recs, workerresult.ByRole),
		ByState: workerresult.AggregateBy(recs, workerresult.ByState), ByProvider: workerresult.AggregateBy(recs, workerresult.ByProvider),
		ByModel: workerresult.AggregateBy(recs, workerresult.ByModel)}, nil
}

// --- read model (13.70-13.72) --------------------------------------------------------------

// View renders a result for the UI. No deterministic validation exists yet
// (later phase), so validation is always reported NOT YET RUN.
func (s *ResultService) View(r workerresult.WorkerResult) workerresult.ResultView {
	return workerresult.BuildView(r, "")
}

// CurrentView is the read model of the project's most recent accepted
// result (nil when none).
func (s *ResultService) CurrentView(ctx context.Context, projectID uuid.UUID) (*workerresult.ResultView, *persistence.StoredWorkerResult, error) {
	rs, _, err := s.Results(ctx, persistence.WorkerResultFilter{ProjectID: projectID, Limit: 1})
	if err != nil || len(rs) == 0 {
		return nil, nil, err
	}
	v := s.View(rs[0].Result)
	return &v, &rs[0], nil
}
