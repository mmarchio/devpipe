package dbrepo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"overseer/internal/compression"
	"overseer/internal/persistence"
	"overseer/internal/prompt"
)

// ---------------------------------------------------------------------------
// Canonical prompt components + preservation manifest (P9-12/P9-13).

// CanonicalPromptComponentRepository implements
// persistence.CanonicalPromptComponentRepository on PostgreSQL.
type CanonicalPromptComponentRepository struct{ exec executor }

var _ persistence.CanonicalPromptComponentRepository = (*CanonicalPromptComponentRepository)(nil)

// NewCanonicalPromptComponentRepository binds the repository to a pool.
func NewCanonicalPromptComponentRepository(pool *pgxpool.Pool) *CanonicalPromptComponentRepository {
	return &CanonicalPromptComponentRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the transaction.
func (r *CanonicalPromptComponentRepository) WithTx(tx persistence.Transaction) persistence.CanonicalPromptComponentRepository {
	return &CanonicalPromptComponentRepository{exec: txExecutor{tx}}
}

// CreateComponents stores the components (one row each, by ordinal).
func (r *CanonicalPromptComponentRepository) CreateComponents(ctx context.Context, canonicalID uuid.UUID, comps []compression.CompressionComponent) error {
	for _, c := range comps {
		var src *uuid.UUID
		if c.SourceID != uuid.Nil {
			id := c.SourceID
			src = &id
		}
		if _, err := r.exec.Exec(ctx,
			`INSERT INTO canonical_prompt_components (canonical_prompt_id, ordinal, component_type, source, policy, content, source_id, source_version, source_hash)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			canonicalID, c.Ordinal, string(c.Type), string(c.Source), string(c.Policy), c.Content, src, c.SourceVersion, c.SourceHash); err != nil {
			return TranslateDBError("create canonical prompt component", err)
		}
	}
	return nil
}

// ListComponents returns the components in ordinal order.
func (r *CanonicalPromptComponentRepository) ListComponents(ctx context.Context, canonicalID uuid.UUID) ([]compression.CompressionComponent, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT ordinal, component_type, source, policy, content, source_id, source_version, source_hash
		 FROM canonical_prompt_components WHERE canonical_prompt_id = $1 ORDER BY ordinal ASC`, canonicalID)
	if err != nil {
		return nil, TranslateDBError("list canonical prompt components", err)
	}
	defer rows.Close()
	out := []compression.CompressionComponent{}
	for rows.Next() {
		var c compression.CompressionComponent
		var typ, src, pol string
		var sid *uuid.UUID
		if err := rows.Scan(&c.Ordinal, &typ, &src, &pol, &c.Content, &sid, &c.SourceVersion, &c.SourceHash); err != nil {
			return nil, TranslateDBError("scan canonical prompt component", err)
		}
		c.Type, c.Source, c.Policy = prompt.PromptComponentType(typ), prompt.PromptSource(src), prompt.CompressionPolicy(pol)
		if sid != nil {
			c.SourceID = *sid
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list canonical prompt components", err)
	}
	return out, nil
}

// CreateManifest stores the preservation manifest.
func (r *CanonicalPromptComponentRepository) CreateManifest(ctx context.Context, canonicalID uuid.UUID, m compression.Manifest) error {
	for i, it := range m {
		if _, err := r.exec.Exec(ctx,
			`INSERT INTO canonical_prompt_preservation_items (canonical_prompt_id, ordinal, kind, item_id, text) VALUES ($1, $2, $3, $4, $5)`,
			canonicalID, i, string(it.Kind), it.ID, it.Text); err != nil {
			return TranslateDBError("create preservation item", err)
		}
	}
	return nil
}

// ListManifest returns the manifest in ordinal order.
func (r *CanonicalPromptComponentRepository) ListManifest(ctx context.Context, canonicalID uuid.UUID) (compression.Manifest, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT ordinal, kind, item_id, text FROM canonical_prompt_preservation_items WHERE canonical_prompt_id = $1 ORDER BY ordinal ASC`, canonicalID)
	if err != nil {
		return nil, TranslateDBError("list preservation items", err)
	}
	defer rows.Close()
	out := compression.Manifest{}
	for rows.Next() {
		var it compression.ManifestItem
		var kind string
		if err := rows.Scan(&it.Ordinal, &kind, &it.ID, &it.Text); err != nil {
			return nil, TranslateDBError("scan preservation item", err)
		}
		it.Kind = compression.ItemKind(kind)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError("list preservation items", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Project compression settings (P9-05).

// CompressionSettingsRepository implements
// persistence.CompressionSettingsRepository.
type CompressionSettingsRepository struct{ exec executor }

var _ persistence.CompressionSettingsRepository = (*CompressionSettingsRepository)(nil)

// NewCompressionSettingsRepository binds the repository to a pool.
func NewCompressionSettingsRepository(pool *pgxpool.Pool) *CompressionSettingsRepository {
	return &CompressionSettingsRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the transaction.
func (r *CompressionSettingsRepository) WithTx(tx persistence.Transaction) persistence.CompressionSettingsRepository {
	return &CompressionSettingsRepository{exec: txExecutor{tx}}
}

// Get returns the stored settings; ErrNotFound when absent.
func (r *CompressionSettingsRepository) Get(ctx context.Context, projectID uuid.UUID) (*persistence.CompressionSettingsRecord, error) {
	rec := &persistence.CompressionSettingsRecord{ProjectID: projectID}
	var mode, vmode string
	var timeoutMS, baseMS, maxMS int64
	s := &rec.Settings
	err := r.exec.QueryRow(ctx,
		`SELECT mode, target_ratio, target_tokens, minimum_savings, max_attempts, validation_mode, timeout_ms, backoff_base_ms, backoff_max_ms, created_at, updated_at
		 FROM project_compression_settings WHERE project_id = $1`, projectID).
		Scan(&mode, &s.TargetRatio, &s.TargetTokens, &s.MinimumSavings, &s.MaxAttempts, &vmode, &timeoutMS, &baseMS, &maxMS, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return nil, TranslateDBError("get compression settings", err)
	}
	s.Mode, s.ValidationMode = compression.Mode(mode), compression.ValidationMode(vmode)
	s.Timeout = time.Duration(timeoutMS) * time.Millisecond
	s.BackoffBase = time.Duration(baseMS) * time.Millisecond
	s.BackoffMax = time.Duration(maxMS) * time.Millisecond
	return rec, nil
}

// Upsert stores the compression-specific fields.
func (r *CompressionSettingsRepository) Upsert(ctx context.Context, rec *persistence.CompressionSettingsRecord) error {
	s := rec.Settings
	now := rec.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO project_compression_settings (project_id, mode, target_ratio, target_tokens, minimum_savings, max_attempts, validation_mode, timeout_ms, backoff_base_ms, backoff_max_ms, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)
		 ON CONFLICT (project_id) DO UPDATE SET mode = EXCLUDED.mode, target_ratio = EXCLUDED.target_ratio,
		   target_tokens = EXCLUDED.target_tokens, minimum_savings = EXCLUDED.minimum_savings,
		   max_attempts = EXCLUDED.max_attempts, validation_mode = EXCLUDED.validation_mode,
		   timeout_ms = EXCLUDED.timeout_ms, backoff_base_ms = EXCLUDED.backoff_base_ms,
		   backoff_max_ms = EXCLUDED.backoff_max_ms, updated_at = EXCLUDED.updated_at`,
		rec.ProjectID, string(s.Mode), s.TargetRatio, s.TargetTokens, s.MinimumSavings, s.MaxAttempts, string(s.ValidationMode),
		s.Timeout.Milliseconds(), s.BackoffBase.Milliseconds(), s.BackoffMax.Milliseconds(), now.UTC()); err != nil {
		return TranslateDBError("upsert compression settings", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Compression instructions (P9-18).

// CompressionInstructionRepository implements
// persistence.CompressionInstructionRepository.
type CompressionInstructionRepository struct{ exec executor }

var _ persistence.CompressionInstructionRepository = (*CompressionInstructionRepository)(nil)

// NewCompressionInstructionRepository binds the repository to a pool.
func NewCompressionInstructionRepository(pool *pgxpool.Pool) *CompressionInstructionRepository {
	return &CompressionInstructionRepository{exec: poolAdapter{pool}}
}

// Append stores content as the next version; concurrent appenders racing on
// the same version retry on the primary-key conflict (bounded).
func (r *CompressionInstructionRepository) Append(ctx context.Context, content string) (compression.Instruction, error) {
	if _, err := compression.NewInstruction(1, content); err != nil {
		return compression.Instruction{}, err
	}
	hash := compression.InstructionHash(content)
	var lastErr error
	for i := 0; i < 5; i++ {
		var v int
		err := r.exec.QueryRow(ctx,
			`INSERT INTO compression_instructions (version, content, hash, created_at)
			 SELECT COALESCE(MAX(version), 0) + 1, $1, $2, $3 FROM compression_instructions RETURNING version`,
			content, hash, time.Now().UTC()).Scan(&v)
		if err == nil {
			return compression.Instruction{Version: v, Content: content, Hash: hash}, nil
		}
		lastErr = TranslateDBError("append compression instruction", err)
		if !errors.Is(lastErr, persistence.ErrConflict) {
			return compression.Instruction{}, lastErr
		}
	}
	return compression.Instruction{}, lastErr
}

// Get returns one version.
func (r *CompressionInstructionRepository) Get(ctx context.Context, version int) (compression.Instruction, error) {
	var in compression.Instruction
	err := r.exec.QueryRow(ctx, `SELECT version, content, hash FROM compression_instructions WHERE version = $1`, version).
		Scan(&in.Version, &in.Content, &in.Hash)
	if err != nil {
		return compression.Instruction{}, TranslateDBError("get compression instruction", err)
	}
	return in, nil
}

// GetCurrent returns the highest version.
func (r *CompressionInstructionRepository) GetCurrent(ctx context.Context) (compression.Instruction, error) {
	var in compression.Instruction
	err := r.exec.QueryRow(ctx, `SELECT version, content, hash FROM compression_instructions ORDER BY version DESC LIMIT 1`).
		Scan(&in.Version, &in.Content, &in.Hash)
	if err != nil {
		return compression.Instruction{}, TranslateDBError("get current compression instruction", err)
	}
	return in, nil
}

// List returns every version ascending.
func (r *CompressionInstructionRepository) List(ctx context.Context) ([]compression.Instruction, error) {
	rows, err := r.exec.Query(ctx, `SELECT version, content, hash FROM compression_instructions ORDER BY version ASC`)
	if err != nil {
		return nil, TranslateDBError("list compression instructions", err)
	}
	defer rows.Close()
	var out []compression.Instruction
	for rows.Next() {
		var in compression.Instruction
		if err := rows.Scan(&in.Version, &in.Content, &in.Hash); err != nil {
			return nil, TranslateDBError("scan compression instruction", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// EnsureDefault seeds the built-in v1 instruction (idempotent).
func (r *CompressionInstructionRepository) EnsureDefault(ctx context.Context) (compression.Instruction, error) {
	d := compression.DefaultInstruction()
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO compression_instructions (version, content, hash, created_at) VALUES ($1, $2, $3, $4) ON CONFLICT (version) DO NOTHING`,
		d.Version, d.Content, d.Hash, time.Now().UTC()); err != nil {
		return compression.Instruction{}, TranslateDBError("seed compression instruction", err)
	}
	return r.GetCurrent(ctx)
}

// ---------------------------------------------------------------------------
// Runs, attempts, compressed prompts, worker prompts (P9-46..P9-48).

// CompressionRepository implements persistence.CompressionRepository.
type CompressionRepository struct{ exec executor }

var _ persistence.CompressionRepository = (*CompressionRepository)(nil)

// NewCompressionRepository binds the repository to a pool.
func NewCompressionRepository(pool *pgxpool.Pool) *CompressionRepository {
	return &CompressionRepository{exec: poolAdapter{pool}}
}

// WithTx returns a copy bound to the transaction.
func (r *CompressionRepository) WithTx(tx persistence.Transaction) persistence.CompressionRepository {
	return &CompressionRepository{exec: txExecutor{tx}}
}

const runColumns = `r.id, r.project_id, r.canonical_prompt_id, r.request_identity, r.config_hash, r.instruction_version, r.provider, r.model, r.mode,
	r.target_tokens, r.canonical_tokens, r.outcome, r.reason, r.dropped, r.attempts, r.created_at, w.id`

// CreateRun inserts a PENDING run.
func (r *CompressionRepository) CreateRun(ctx context.Context, run *compression.Run) error {
	dropped, err := json.Marshal(nonNilDropped(run.Dropped))
	if err != nil {
		return &persistence.InfrastructureError{Op: "encode dropped components", Cause: err}
	}
	var instr *int
	if run.InstructionVersion > 0 {
		v := run.InstructionVersion
		instr = &v
	}
	outcome := run.Outcome
	if outcome == "" {
		outcome = "PENDING"
	}
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO compression_runs (id, project_id, canonical_prompt_id, request_identity, config_hash, instruction_version, provider, model, mode,
		   target_tokens, canonical_tokens, outcome, reason, dropped, attempts, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		run.ID, run.ProjectID, run.CanonicalPromptID, run.RequestIdentity, run.ConfigHash, instr, run.Provider, run.Model, string(run.Mode),
		run.TargetTokens, run.CanonicalTokens, string(outcome), run.Reason, dropped, run.Attempts, run.CreatedAt.UTC()); err != nil {
		return TranslateDBError("create compression run", err)
	}
	return nil
}

// FinishRun finalizes a PENDING run exactly once.
func (r *CompressionRepository) FinishRun(ctx context.Context, runID uuid.UUID, outcome compression.RunOutcome, reason string, attempts int, finishedAt time.Time) error {
	tag, err := r.exec.Exec(ctx,
		`UPDATE compression_runs SET outcome = $2, reason = $3, attempts = $4, finished_at = $5 WHERE id = $1 AND outcome = 'PENDING'`,
		runID, string(outcome), reason, attempts, finishedAt.UTC())
	if err != nil {
		return TranslateDBError("finish compression run", err)
	}
	if tag.RowsAffected() == 0 {
		return persistence.ErrConflict
	}
	return nil
}

func scanRun(row interface{ Scan(dest ...any) error }) (*compression.Run, error) {
	var run compression.Run
	var instr *int
	var mode, outcome string
	var dropped []byte
	var wp *uuid.UUID
	if err := row.Scan(&run.ID, &run.ProjectID, &run.CanonicalPromptID, &run.RequestIdentity, &run.ConfigHash, &instr, &run.Provider, &run.Model, &mode,
		&run.TargetTokens, &run.CanonicalTokens, &outcome, &run.Reason, &dropped, &run.Attempts, &run.CreatedAt, &wp); err != nil {
		return nil, err
	}
	if instr != nil {
		run.InstructionVersion = *instr
	}
	run.Mode, run.Outcome, run.WorkerPromptID = compression.Mode(mode), compression.RunOutcome(outcome), wp
	if len(dropped) > 0 {
		_ = json.Unmarshal(dropped, &run.Dropped)
	}
	return &run, nil
}

func (r *CompressionRepository) listRuns(ctx context.Context, op, where string, arg any) ([]*compression.Run, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+runColumns+` FROM compression_runs r LEFT JOIN worker_prompts w ON w.run_id = r.id WHERE `+where, arg)
	if err != nil {
		return nil, TranslateDBError(op, err)
	}
	defer rows.Close()
	out := []*compression.Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, TranslateDBError(op, err)
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError(op, err)
	}
	return out, nil
}

// GetRun returns one run.
func (r *CompressionRepository) GetRun(ctx context.Context, runID uuid.UUID) (*compression.Run, error) {
	runs, err := r.listRuns(ctx, "get compression run", `r.id = $1`, runID)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, persistence.ErrNotFound
	}
	return runs[0], nil
}

// ListRunsByCanonical returns the canonical prompt's runs, oldest first.
func (r *CompressionRepository) ListRunsByCanonical(ctx context.Context, canonicalID uuid.UUID) ([]*compression.Run, error) {
	return r.listRuns(ctx, "list compression runs", `r.canonical_prompt_id = $1 ORDER BY r.created_at ASC, r.id ASC`, canonicalID)
}

// ListRunsByProject returns the project's runs, oldest first.
func (r *CompressionRepository) ListRunsByProject(ctx context.Context, projectID uuid.UUID) ([]*compression.Run, error) {
	return r.listRuns(ctx, "list compression runs", `r.project_id = $1 ORDER BY r.created_at ASC, r.id ASC`, projectID)
}

// ListRunsByIdentity returns the runs with the request identity, newest
// first.
func (r *CompressionRepository) ListRunsByIdentity(ctx context.Context, identity string) ([]*compression.Run, error) {
	return r.listRuns(ctx, "list compression runs", `r.request_identity = $1 ORDER BY r.created_at DESC, r.id DESC`, identity)
}

// CreateAttempt inserts an immutable attempt row.
func (r *CompressionRepository) CreateAttempt(ctx context.Context, canonicalID uuid.UUID, a *compression.Attempt) error {
	var validation []byte
	if a.Validation != nil {
		b, err := json.Marshal(a.Validation)
		if err != nil {
			return &persistence.InfrastructureError{Op: "encode validation result", Cause: err}
		}
		validation = b
	}
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO compression_attempts (id, run_id, canonical_prompt_id, number, status, provider, model, error_kind, error_message, validation,
		   input_tokens, output_tokens, finish_reason, provider_request_id, duration_ms, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		a.ID, a.RunID, canonicalID, a.Number, string(a.Status), a.Provider, a.Model, string(a.ErrorKind), a.ErrorMessage, validation,
		a.InputTokens, a.OutputTokens, a.FinishReason, a.ProviderRequestID, a.Duration.Milliseconds(), a.CreatedAt.UTC()); err != nil {
		return TranslateDBError("create compression attempt", err)
	}
	return nil
}

const attemptColumns = `a.id, a.run_id, a.number, a.status, a.provider, a.model, a.error_kind, a.error_message, a.validation,
	a.input_tokens, a.output_tokens, a.finish_reason, a.provider_request_id, a.duration_ms, a.created_at, c.id, c.content`

func (r *CompressionRepository) listAttempts(ctx context.Context, op, join, where string, arg any) ([]compression.Attempt, error) {
	rows, err := r.exec.Query(ctx,
		`SELECT `+attemptColumns+` FROM compression_attempts a `+join+` LEFT JOIN compressed_prompts c ON c.attempt_id = a.id WHERE `+where, arg)
	if err != nil {
		return nil, TranslateDBError(op, err)
	}
	defer rows.Close()
	out := []compression.Attempt{}
	for rows.Next() {
		var a compression.Attempt
		var status, kind string
		var validation []byte
		var ms int64
		var cid *uuid.UUID
		var content *string
		if err := rows.Scan(&a.ID, &a.RunID, &a.Number, &status, &a.Provider, &a.Model, &kind, &a.ErrorMessage, &validation,
			&a.InputTokens, &a.OutputTokens, &a.FinishReason, &a.ProviderRequestID, &ms, &a.CreatedAt, &cid, &content); err != nil {
			return nil, TranslateDBError(op, err)
		}
		a.Status, a.ErrorKind, a.Duration = compression.AttemptStatus(status), compression.ErrorKind(kind), time.Duration(ms)*time.Millisecond
		a.CompressedPromptID = cid
		if content != nil {
			a.Output = *content
		}
		if len(validation) > 0 {
			var v compression.ValidationResult
			if err := json.Unmarshal(validation, &v); err == nil {
				a.Validation = &v
			}
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, TranslateDBError(op, err)
	}
	return out, nil
}

// ListAttemptsByRun returns a run's attempts by number.
func (r *CompressionRepository) ListAttemptsByRun(ctx context.Context, runID uuid.UUID) ([]compression.Attempt, error) {
	return r.listAttempts(ctx, "list compression attempts", "", `a.run_id = $1 ORDER BY a.number ASC`, runID)
}

// ListAttemptsByCanonical returns every attempt of the canonical prompt.
func (r *CompressionRepository) ListAttemptsByCanonical(ctx context.Context, canonicalID uuid.UUID) ([]compression.Attempt, error) {
	return r.listAttempts(ctx, "list compression attempts", "JOIN compression_runs r ON r.id = a.run_id",
		`a.canonical_prompt_id = $1 ORDER BY r.created_at ASC, r.id ASC, a.number ASC`, canonicalID)
}

// ListAttemptsByProject returns every attempt of the project.
func (r *CompressionRepository) ListAttemptsByProject(ctx context.Context, projectID uuid.UUID) ([]compression.Attempt, error) {
	return r.listAttempts(ctx, "list compression attempts", "JOIN compression_runs r ON r.id = a.run_id",
		`r.project_id = $1 ORDER BY r.created_at ASC, r.id ASC, a.number ASC`, projectID)
}

const compressedColumns = `id, canonical_prompt_id, attempt_id, instruction_version, provider, model, attempt, content, hash, canonical_tokens, compressed_tokens, validation_status, created_at`

// CreateCompressedPrompt inserts an immutable compressed prompt.
func (r *CompressionRepository) CreateCompressedPrompt(ctx context.Context, cp *compression.CompressedPrompt) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO compressed_prompts (`+compressedColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		cp.ID, cp.CanonicalPromptID, cp.AttemptID, cp.CompressionInstructionVersion, cp.Provider, cp.Model, cp.Attempt,
		cp.Content, cp.Hash, cp.CanonicalTokens, cp.CompressedTokens, string(cp.ValidationStatus), cp.CreatedAt.UTC()); err != nil {
		return TranslateDBError("create compressed prompt", err)
	}
	return nil
}

func scanCompressed(row interface{ Scan(dest ...any) error }) (*compression.CompressedPrompt, error) {
	var cp compression.CompressedPrompt
	var status string
	if err := row.Scan(&cp.ID, &cp.CanonicalPromptID, &cp.AttemptID, &cp.CompressionInstructionVersion, &cp.Provider, &cp.Model, &cp.Attempt,
		&cp.Content, &cp.Hash, &cp.CanonicalTokens, &cp.CompressedTokens, &status, &cp.CreatedAt); err != nil {
		return nil, err
	}
	cp.ValidationStatus = compression.ValidationStatus(status)
	return &cp, nil
}

// GetCompressedPrompt returns one compressed prompt.
func (r *CompressionRepository) GetCompressedPrompt(ctx context.Context, id uuid.UUID) (*compression.CompressedPrompt, error) {
	cp, err := scanCompressed(r.exec.QueryRow(ctx, `SELECT `+compressedColumns+` FROM compressed_prompts WHERE id = $1`, id))
	if err != nil {
		return nil, TranslateDBError("get compressed prompt", err)
	}
	return cp, nil
}

// ListCompressedByCanonical returns the canonical prompt's compressed
// prompts in creation order.
func (r *CompressionRepository) ListCompressedByCanonical(ctx context.Context, canonicalID uuid.UUID) ([]*compression.CompressedPrompt, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+compressedColumns+` FROM compressed_prompts WHERE canonical_prompt_id = $1 ORDER BY created_at ASC, attempt ASC, id ASC`, canonicalID)
	if err != nil {
		return nil, TranslateDBError("list compressed prompts", err)
	}
	defer rows.Close()
	out := []*compression.CompressedPrompt{}
	for rows.Next() {
		cp, err := scanCompressed(rows)
		if err != nil {
			return nil, TranslateDBError("list compressed prompts", err)
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

const workerColumns = `id, canonical_prompt_id, compressed_prompt_id, run_id, source, content, hash, estimated_tokens, created_at`

// CreateWorkerPrompt inserts an immutable worker prompt.
func (r *CompressionRepository) CreateWorkerPrompt(ctx context.Context, projectID uuid.UUID, wp *compression.WorkerPrompt) error {
	if _, err := r.exec.Exec(ctx,
		`INSERT INTO worker_prompts (project_id, `+workerColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		projectID, wp.ID, nullUUID(wp.CanonicalPromptID), wp.CompressedPromptID, nullUUID(wp.RunID), string(wp.Source), wp.Content, wp.Hash, wp.EstimatedTokens, wp.CreatedAt.UTC()); err != nil {
		return TranslateDBError("create worker prompt", err)
	}
	return nil
}

// nullUUID stores uuid.Nil as NULL (a command prompt has no canonical
// prompt or compression run).
func nullUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

func scanWorker(row interface{ Scan(dest ...any) error }) (*compression.WorkerPrompt, error) {
	var wp compression.WorkerPrompt
	var src string
	var canonical, run *uuid.UUID
	if err := row.Scan(&wp.ID, &canonical, &wp.CompressedPromptID, &run, &src, &wp.Content, &wp.Hash, &wp.EstimatedTokens, &wp.CreatedAt); err != nil {
		return nil, err
	}
	if canonical != nil {
		wp.CanonicalPromptID = *canonical
	}
	if run != nil {
		wp.RunID = *run
	}
	wp.Source = compression.WorkerPromptSource(src)
	return &wp, nil
}

// GetWorkerPrompt returns one worker prompt.
func (r *CompressionRepository) GetWorkerPrompt(ctx context.Context, id uuid.UUID) (*compression.WorkerPrompt, error) {
	wp, err := scanWorker(r.exec.QueryRow(ctx, `SELECT `+workerColumns+` FROM worker_prompts WHERE id = $1`, id))
	if err != nil {
		return nil, TranslateDBError("get worker prompt", err)
	}
	return wp, nil
}

// GetProjectWorkerPrompt returns one worker prompt of the project.
func (r *CompressionRepository) GetProjectWorkerPrompt(ctx context.Context, projectID, id uuid.UUID) (*compression.WorkerPrompt, error) {
	wp, err := scanWorker(r.exec.QueryRow(ctx, `SELECT `+workerColumns+` FROM worker_prompts WHERE id = $1 AND project_id = $2`, id, projectID))
	if err != nil {
		return nil, TranslateDBError("get worker prompt", err)
	}
	return wp, nil
}

// GetWorkerPromptByRun returns the run's worker prompt.
func (r *CompressionRepository) GetWorkerPromptByRun(ctx context.Context, runID uuid.UUID) (*compression.WorkerPrompt, error) {
	wp, err := scanWorker(r.exec.QueryRow(ctx, `SELECT `+workerColumns+` FROM worker_prompts WHERE run_id = $1`, runID))
	if err != nil {
		return nil, TranslateDBError("get worker prompt", err)
	}
	return wp, nil
}

// ListWorkerPromptsByCanonical returns the canonical prompt's worker
// prompts in creation order.
func (r *CompressionRepository) ListWorkerPromptsByCanonical(ctx context.Context, canonicalID uuid.UUID) ([]*compression.WorkerPrompt, error) {
	rows, err := r.exec.Query(ctx, `SELECT `+workerColumns+` FROM worker_prompts WHERE canonical_prompt_id = $1 ORDER BY created_at ASC, id ASC`, canonicalID)
	if err != nil {
		return nil, TranslateDBError("list worker prompts", err)
	}
	defer rows.Close()
	out := []*compression.WorkerPrompt{}
	for rows.Next() {
		wp, err := scanWorker(rows)
		if err != nil {
			return nil, TranslateDBError("list worker prompts", err)
		}
		out = append(out, wp)
	}
	return out, rows.Err()
}

func nonNilDropped(d []compression.DroppedComponent) []compression.DroppedComponent {
	if d == nil {
		return []compression.DroppedComponent{}
	}
	return d
}
