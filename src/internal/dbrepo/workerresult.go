package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/persistence"
	"overseer/internal/workerresult"
)

// ---------------------------------------------------------------------------
// Settings (P13-29).

// ResultProtocolSettingsRepository implements persistence.ResultProtocolSettingsRepository.
type ResultProtocolSettingsRepository struct{ exec executor }

var _ persistence.ResultProtocolSettingsRepository = (*ResultProtocolSettingsRepository)(nil)

// NewResultProtocolSettingsRepository binds the repository to a pool.
func NewResultProtocolSettingsRepository(pool *pgxpool.Pool) *ResultProtocolSettingsRepository {
	return &ResultProtocolSettingsRepository{exec: poolAdapter{pool}}
}

// Get returns stored settings or defaults.
func (r *ResultProtocolSettingsRepository) Get(ctx context.Context, projectID uuid.UUID) (workerresult.Settings, bool, error) {
	var s workerresult.Settings
	var mode, repair string
	l := &s.Limits
	err := r.exec.QueryRow(ctx, `SELECT protocol_major, protocol_minor, parse_mode, repair_mode, max_response_bytes, max_summary_bytes,
		max_item_bytes, max_next_action_bytes, max_completed_items, max_remaining_items, max_blocking_items, max_artifact_items, max_acceptance_items
		FROM project_result_protocol_settings WHERE project_id = $1`, projectID).
		Scan(&s.ProtocolVersion.Major, &s.ProtocolVersion.Minor, &mode, &repair, &l.MaxResponseBytes, &l.MaxSummaryBytes, &l.MaxItemBytes,
			&l.MaxNextActionByte, &l.MaxCompleted, &l.MaxRemaining, &l.MaxBlocking, &l.MaxArtifacts, &l.MaxAcceptance)
	if err != nil {
		if errors.Is(TranslateDBError("get result protocol settings", err), persistence.ErrNotFound) {
			return workerresult.DefaultSettings(), false, nil
		}
		return workerresult.Settings{}, false, TranslateDBError("get result protocol settings", err)
	}
	s.ParseMode, s.RepairMode = workerresult.ParseMode(mode), workerresult.RepairMode(repair)
	return s, true, nil
}

// Upsert stores settings.
func (r *ResultProtocolSettingsRepository) Upsert(ctx context.Context, projectID uuid.UUID, s workerresult.Settings) error {
	l := s.Limits
	_, err := r.exec.Exec(ctx, `INSERT INTO project_result_protocol_settings (project_id, protocol_major, protocol_minor, parse_mode, repair_mode,
		max_response_bytes, max_summary_bytes, max_item_bytes, max_next_action_bytes, max_completed_items, max_remaining_items, max_blocking_items,
		max_artifact_items, max_acceptance_items, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, now())
		ON CONFLICT (project_id) DO UPDATE SET protocol_major = EXCLUDED.protocol_major, protocol_minor = EXCLUDED.protocol_minor,
		  parse_mode = EXCLUDED.parse_mode, repair_mode = EXCLUDED.repair_mode, max_response_bytes = EXCLUDED.max_response_bytes,
		  max_summary_bytes = EXCLUDED.max_summary_bytes, max_item_bytes = EXCLUDED.max_item_bytes, max_next_action_bytes = EXCLUDED.max_next_action_bytes,
		  max_completed_items = EXCLUDED.max_completed_items, max_remaining_items = EXCLUDED.max_remaining_items,
		  max_blocking_items = EXCLUDED.max_blocking_items, max_artifact_items = EXCLUDED.max_artifact_items,
		  max_acceptance_items = EXCLUDED.max_acceptance_items, updated_at = now()`,
		projectID, s.ProtocolVersion.Major, s.ProtocolVersion.Minor, string(s.ParseMode), string(s.RepairMode), l.MaxResponseBytes, l.MaxSummaryBytes,
		l.MaxItemBytes, l.MaxNextActionByte, l.MaxCompleted, l.MaxRemaining, l.MaxBlocking, l.MaxArtifacts, l.MaxAcceptance)
	if err != nil {
		return TranslateDBError("upsert result protocol settings", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Results (P13-30).

// WorkerResultRepository implements persistence.WorkerResultRepository.
type WorkerResultRepository struct {
	exec executor
	pool *pgxpool.Pool
}

var _ persistence.WorkerResultRepository = (*WorkerResultRepository)(nil)

// NewWorkerResultRepository binds the repository to a pool.
func NewWorkerResultRepository(pool *pgxpool.Pool) *WorkerResultRepository {
	return &WorkerResultRepository{exec: poolAdapter{pool}, pool: pool}
}

var itemKinds = []struct {
	kind string
	get  func(*workerresult.WorkerResult) *[]string
}{
	{"COMPLETED", func(r *workerresult.WorkerResult) *[]string { return &r.Completed }},
	{"REMAINING", func(r *workerresult.WorkerResult) *[]string { return &r.Remaining }},
	{"BLOCKING", func(r *workerresult.WorkerResult) *[]string { return &r.Blocking }},
}

// CreateAccepted stores an accepted result and its ordered children.
func (r *WorkerResultRepository) CreateAccepted(ctx context.Context, id uuid.UUID, res *workerresult.WorkerResult, at time.Time) (*persistence.StoredWorkerResult, bool, error) {
	m, p := res.Metadata, res.Provenance
	var run, passed *bool
	var tsum *string
	if t := res.Tests; t != nil {
		rr := t.ReportedRun
		run, passed, tsum = &rr, t.ReportedPassed, &t.Summary
	}
	created := false
	err := Run(ctx, r.pool, func(tx persistence.Transaction) error {
		tag, err := tx.Exec(ctx, `INSERT INTO worker_results (id, dispatch_id, project_id, session_id, request_id, phase_id, work_unit_id, state_key, role_key,
			protocol_major, protocol_minor, status, summary, next_action, test_reported_run, test_reported_passed, test_summary, raw_response_hash,
			normalized_hash, parsed_hash, parse_mode, repair_mode, extraction, repaired, repair_method, parser_version, parsed_at, created_at, plan)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29)
			ON CONFLICT (dispatch_id) DO NOTHING`,
			id, m.DispatchID, m.ProjectID, m.SessionID, m.RequestID, m.PhaseID, m.WorkUnitID, m.StateKey, m.RoleKey,
			res.ProtocolVersion.Major, res.ProtocolVersion.Minor, string(res.Status), res.Summary, res.NextAction, run, passed, tsum,
			m.RawResponseHash, res.NormalizedHash, p.ParsedHash, string(p.Mode), string(p.RepairMode), string(p.Extraction), p.Repaired,
			p.RepairMethod, p.ParserVersion, m.ParsedAt.UTC(), at.UTC(), planText(res.Plan))
		if err != nil {
			return TranslateDBError("insert worker result", err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		created = true
		for _, k := range itemKinds {
			for i, text := range *k.get(res) {
				if _, err := tx.Exec(ctx, `INSERT INTO worker_result_items (result_id, kind, ordinal, text) VALUES ($1, $2, $3, $4)`, id, k.kind, i, text); err != nil {
					return TranslateDBError("insert worker result item", err)
				}
			}
		}
		for i, a := range res.Artifacts {
			if _, err := tx.Exec(ctx, `INSERT INTO worker_result_artifacts (result_id, ordinal, path, action, previous_path) VALUES ($1, $2, $3, $4, $5)`,
				id, i, a.Path, string(a.Action), a.PreviousPath); err != nil {
				return TranslateDBError("insert worker result artifact", err)
			}
		}
		for i, c := range res.AcceptanceClaims {
			if _, err := tx.Exec(ctx, `INSERT INTO worker_result_acceptance_claims (result_id, ordinal, criterion_id, status, evidence, recognized)
				VALUES ($1, $2, $3, $4, $5, $6)`, id, i, c.ID, string(c.Status), c.Evidence, c.Recognized); err != nil {
				return TranslateDBError("insert worker result claim", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	out, err := r.GetByDispatch(ctx, m.ProjectID, m.DispatchID)
	return out, created, err
}

const resultCols = `id, dispatch_id, project_id, session_id, request_id, phase_id, work_unit_id, state_key, role_key, protocol_major, protocol_minor,
	status, summary, next_action, test_reported_run, test_reported_passed, test_summary, raw_response_hash, normalized_hash, parsed_hash,
	parse_mode, repair_mode, extraction, repaired, repair_method, parser_version, parsed_at, created_at, plan`

func scanResult(row interface{ Scan(dest ...any) error }) (*persistence.StoredWorkerResult, error) {
	var s persistence.StoredWorkerResult
	r := &s.Result
	m, p := &r.Metadata, &r.Provenance
	var status, mode, repair, extraction string
	var run, passed *bool
	var tsum, plan *string
	if err := row.Scan(&s.ID, &m.DispatchID, &m.ProjectID, &m.SessionID, &m.RequestID, &m.PhaseID, &m.WorkUnitID, &m.StateKey, &m.RoleKey,
		&r.ProtocolVersion.Major, &r.ProtocolVersion.Minor, &status, &r.Summary, &r.NextAction, &run, &passed, &tsum, &m.RawResponseHash,
		&r.NormalizedHash, &p.ParsedHash, &mode, &repair, &extraction, &p.Repaired, &p.RepairMethod, &p.ParserVersion, &m.ParsedAt, &s.CreatedAt, &plan); err != nil {
		return nil, err
	}
	if plan != nil {
		r.Plan = []byte(*plan)
	}
	r.Status, p.Mode, p.RepairMode, p.Extraction = workerresult.WorkerResultStatus(status), workerresult.ParseMode(mode),
		workerresult.RepairMode(repair), workerresult.ExtractionKind(extraction)
	p.SourceHash = m.RawResponseHash
	if run != nil {
		r.Tests = &workerresult.WorkerTestClaim{ReportedRun: *run, ReportedPassed: passed}
		if tsum != nil {
			r.Tests.Summary = *tsum
		}
	}
	return &s, nil
}

// children loads ordered items, artifacts and claims.
func (r *WorkerResultRepository) children(ctx context.Context, s *persistence.StoredWorkerResult) error {
	res := &s.Result
	res.Completed, res.Remaining, res.Blocking = []string{}, []string{}, []string{}
	rows, err := r.exec.Query(ctx, `SELECT kind, text FROM worker_result_items WHERE result_id = $1 ORDER BY kind, ordinal`, s.ID)
	if err != nil {
		return TranslateDBError("load worker result items", err)
	}
	for rows.Next() {
		var kind, text string
		if err := rows.Scan(&kind, &text); err != nil {
			rows.Close()
			return TranslateDBError("load worker result items", err)
		}
		for _, k := range itemKinds {
			if k.kind == kind {
				*k.get(res) = append(*k.get(res), text)
			}
		}
	}
	rows.Close()
	rows, err = r.exec.Query(ctx, `SELECT path, action, previous_path FROM worker_result_artifacts WHERE result_id = $1 ORDER BY ordinal`, s.ID)
	if err != nil {
		return TranslateDBError("load worker result artifacts", err)
	}
	for rows.Next() {
		var a workerresult.ArtifactClaim
		var act string
		if err := rows.Scan(&a.Path, &act, &a.PreviousPath); err != nil {
			rows.Close()
			return TranslateDBError("load worker result artifacts", err)
		}
		a.Action = workerresult.ArtifactAction(act)
		res.Artifacts = append(res.Artifacts, a)
	}
	rows.Close()
	rows, err = r.exec.Query(ctx, `SELECT criterion_id, status, evidence, recognized FROM worker_result_acceptance_claims WHERE result_id = $1 ORDER BY ordinal`, s.ID)
	if err != nil {
		return TranslateDBError("load worker result claims", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c workerresult.AcceptanceClaim
		var st string
		if err := rows.Scan(&c.ID, &st, &c.Evidence, &c.Recognized); err != nil {
			return TranslateDBError("load worker result claims", err)
		}
		c.Status = workerresult.AcceptanceClaimStatus(st)
		res.AcceptanceClaims = append(res.AcceptanceClaims, c)
	}
	return rows.Err()
}

func (r *WorkerResultRepository) one(ctx context.Context, op, where string, args ...any) (*persistence.StoredWorkerResult, error) {
	s, err := scanResult(r.exec.QueryRow(ctx, `SELECT `+resultCols+` FROM worker_results WHERE `+where, args...))
	if err != nil {
		return nil, TranslateDBError(op, err)
	}
	if err := r.children(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Get returns a project's result.
func (r *WorkerResultRepository) Get(ctx context.Context, projectID, id uuid.UUID) (*persistence.StoredWorkerResult, error) {
	return r.one(ctx, "get worker result", "id = $1 AND project_id = $2", id, projectID)
}

// GetByDispatch returns the accepted result of a dispatch.
func (r *WorkerResultRepository) GetByDispatch(ctx context.Context, projectID, dispatchID uuid.UUID) (*persistence.StoredWorkerResult, error) {
	return r.one(ctx, "get worker result by dispatch", "dispatch_id = $1 AND project_id = $2", dispatchID, projectID)
}

// List pages a project's results (newest first).
func (r *WorkerResultRepository) List(ctx context.Context, f persistence.WorkerResultFilter) ([]persistence.StoredWorkerResult, int, error) {
	where := " WHERE project_id = $1"
	args := []any{f.ProjectID}
	add := func(col string, v any) {
		args = append(args, v)
		where += " AND " + col + " = $" + itoa(len(args))
	}
	if f.PhaseID != nil {
		add("phase_id", *f.PhaseID)
	}
	if f.WorkUnitID != nil {
		add("work_unit_id", *f.WorkUnitID)
	}
	if f.StateKey != "" {
		add("state_key", f.StateKey)
	}
	if f.RoleKey != "" {
		add("role_key", f.RoleKey)
	}
	if f.Status != "" {
		add("status", string(f.Status))
	}
	if f.SessionID != nil {
		add("session_id", *f.SessionID)
	}
	var total int
	if err := r.exec.QueryRow(ctx, `SELECT count(*) FROM worker_results`+where, args...).Scan(&total); err != nil {
		return nil, 0, TranslateDBError("count worker results", err)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args = append(args, limit, f.Offset)
	rows, err := r.exec.Query(ctx, `SELECT `+resultCols+` FROM worker_results`+where+` ORDER BY created_at DESC, id LIMIT $`+itoa(len(args)-1)+
		` OFFSET $`+itoa(len(args)), args...)
	if err != nil {
		return nil, 0, TranslateDBError("list worker results", err)
	}
	var out []persistence.StoredWorkerResult
	for rows.Next() {
		s, err := scanResult(rows)
		if err != nil {
			rows.Close()
			return nil, 0, TranslateDBError("list worker results", err)
		}
		out = append(out, *s)
	}
	rows.Close()
	for i := range out {
		if err := r.children(ctx, &out[i]); err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}

// ---------------------------------------------------------------------------
// Parse attempts (P13-31 / P13-32).

// ParseAttemptRepository implements persistence.ParseAttemptRepository.
type ParseAttemptRepository struct{ exec executor }

var _ persistence.ParseAttemptRepository = (*ParseAttemptRepository)(nil)

// NewParseAttemptRepository binds the repository to a pool.
func NewParseAttemptRepository(pool *pgxpool.Pool) *ParseAttemptRepository {
	return &ParseAttemptRepository{exec: poolAdapter{pool}}
}

type valErrJSON struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Create stores an attempt.
func (r *ParseAttemptRepository) Create(ctx context.Context, a *persistence.ParseAttemptRecord) error {
	ve := make([]valErrJSON, 0, len(a.ValidationErrors))
	for _, e := range a.ValidationErrors {
		ve = append(ve, valErrJSON{e.Field, e.Code, e.Message})
	}
	b, _ := json.Marshal(ve)
	_, err := r.exec.Exec(ctx, `INSERT INTO worker_result_parse_attempts (id, dispatch_id, project_id, raw_response_hash, outcome, error_code,
		validation_errors, result_status, normalized_hash, result_id, differs_from_accepted, parse_mode, repair_mode, repaired, repair_method,
		parser_version, reparse, role_key, state_key, worker_provider, worker_model, started_at, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`,
		a.ID, a.DispatchID, a.ProjectID, a.RawResponseHash, string(a.Outcome), string(a.ErrorCode), b, string(a.ResultStatus), a.NormalizedHash,
		a.ResultID, a.DiffersFromAccepted, string(a.ParseMode), string(a.RepairMode), a.Repaired, a.RepairMethod, a.ParserVersion, a.Reparse,
		a.RoleKey, a.StateKey, a.WorkerProvider, a.WorkerModel, a.StartedAt.UTC(), a.CompletedAt.UTC())
	if err != nil {
		return TranslateDBError("create parse attempt", err)
	}
	return nil
}

const attemptCols = `id, dispatch_id, project_id, raw_response_hash, outcome, error_code, validation_errors, result_status, normalized_hash,
	result_id, differs_from_accepted, parse_mode, repair_mode, repaired, repair_method, parser_version, reparse, role_key, state_key,
	worker_provider, worker_model, started_at, completed_at`

func scanAttempt(row interface{ Scan(dest ...any) error }) (*persistence.ParseAttemptRecord, error) {
	var a persistence.ParseAttemptRecord
	var outcome, code, status, mode, repair string
	var ve []byte
	if err := row.Scan(&a.ID, &a.DispatchID, &a.ProjectID, &a.RawResponseHash, &outcome, &code, &ve, &status, &a.NormalizedHash, &a.ResultID,
		&a.DiffersFromAccepted, &mode, &repair, &a.Repaired, &a.RepairMethod, &a.ParserVersion, &a.Reparse, &a.RoleKey, &a.StateKey,
		&a.WorkerProvider, &a.WorkerModel, &a.StartedAt, &a.CompletedAt); err != nil {
		return nil, err
	}
	a.Outcome, a.ErrorCode, a.ResultStatus = workerresult.AttemptOutcome(outcome), workerresult.WorkerResultErrorCode(code), workerresult.WorkerResultStatus(status)
	a.ParseMode, a.RepairMode = workerresult.ParseMode(mode), workerresult.RepairMode(repair)
	var js []valErrJSON
	_ = json.Unmarshal(ve, &js)
	for _, e := range js {
		a.ValidationErrors = append(a.ValidationErrors, workerresult.ValidationError{Field: e.Field, Code: e.Code, Message: e.Message})
	}
	return &a, nil
}

// Get returns a project's attempt.
func (r *ParseAttemptRepository) Get(ctx context.Context, projectID, id uuid.UUID) (*persistence.ParseAttemptRecord, error) {
	a, err := scanAttempt(r.exec.QueryRow(ctx, `SELECT `+attemptCols+` FROM worker_result_parse_attempts WHERE id = $1 AND project_id = $2`, id, projectID))
	if err != nil {
		return nil, TranslateDBError("get parse attempt", err)
	}
	return a, nil
}

// List pages a project's attempts (newest first).
func (r *ParseAttemptRepository) List(ctx context.Context, f persistence.ParseAttemptFilter) ([]persistence.ParseAttemptRecord, int, error) {
	where := " WHERE project_id = $1"
	args := []any{f.ProjectID}
	if f.DispatchID != nil {
		args = append(args, *f.DispatchID)
		where += " AND dispatch_id = $" + itoa(len(args))
	}
	if f.Outcome != "" {
		args = append(args, string(f.Outcome))
		where += " AND outcome = $" + itoa(len(args))
	}
	var total int
	if err := r.exec.QueryRow(ctx, `SELECT count(*) FROM worker_result_parse_attempts`+where, args...).Scan(&total); err != nil {
		return nil, 0, TranslateDBError("count parse attempts", err)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 50
	}
	args = append(args, limit, f.Offset)
	rows, err := r.exec.Query(ctx, `SELECT `+attemptCols+` FROM worker_result_parse_attempts`+where+` ORDER BY started_at DESC, id LIMIT $`+
		itoa(len(args)-1)+` OFFSET $`+itoa(len(args)), args...)
	if err != nil {
		return nil, 0, TranslateDBError("list parse attempts", err)
	}
	defer rows.Close()
	var out []persistence.ParseAttemptRecord
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, 0, TranslateDBError("list parse attempts", err)
		}
		out = append(out, *a)
	}
	return out, total, rows.Err()
}

// ListUnparsed returns COMPLETED dispatches with a stored response and no
// parse attempt (WBS 13.88), oldest first.
func (r *ParseAttemptRepository) ListUnparsed(ctx context.Context, limit int) ([]persistence.UnparsedDispatch, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := r.exec.Query(ctx, `SELECT d.id, d.project_id FROM worker_dispatches d
		JOIN worker_responses rs ON rs.dispatch_id = d.id
		WHERE d.status = 'COMPLETED' AND NOT EXISTS (SELECT 1 FROM worker_result_parse_attempts a WHERE a.dispatch_id = d.id)
		ORDER BY d.completed_at, d.id LIMIT $1`, limit)
	if err != nil {
		return nil, TranslateDBError("list unparsed dispatches", err)
	}
	defer rows.Close()
	var out []persistence.UnparsedDispatch
	for rows.Next() {
		var u persistence.UnparsedDispatch
		if err := rows.Scan(&u.DispatchID, &u.ProjectID); err != nil {
			return nil, TranslateDBError("list unparsed dispatches", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// planText stores the P16-80 plan bytes verbatim (TEXT, not JSONB, so the
// normalized hash recomputes byte-identically).
func planText(b []byte) *string {
	if b == nil {
		return nil
	}
	s := string(b)
	return &s
}
