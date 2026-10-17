// Phase 9: the prompt-compression REST surface.
//
//	GET  /api/v1/projects/{project}/settings/compression           (WBS 9.2)
//	PUT  /api/v1/projects/{project}/settings/compression           (WBS 9.2)
//	POST /api/v1/projects/{project}/prompts/{prompt}/worker-prompt  (WBS 9.35)
//	POST /api/v1/projects/{project}/prompts/{prompt}/compression-preview   (WBS 9.45)
//	GET  /api/v1/projects/{project}/prompts/{prompt}/compression-attempts  (WBS 9.46)
//	GET  /api/v1/projects/{project}/prompts/{prompt}/compression-attempts/{attempt}/diff (WBS 9.48)
//	GET  /api/v1/projects/{project}/compression-metrics             (WBS 9.47)
//	GET  /api/v1/projects/{project}/compression-runs                (console Compressor tab)
//	GET  /api/v1/projects/{project}/compression-attempts            (console Compressor tab)
//	GET  /api/v1/projects/{project}/worker-prompts/{worker}/lineage (WBS 9.34)
//
// Read endpoints are read-only (other methods are a 405 via the mux); the
// preview persists nothing. No response ever carries a credential.
package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"overseer/internal/compression"
	"overseer/internal/readmodel"
	"overseer/internal/service"
)

type compressionAPI struct {
	svc *service.CompressionService
}

// RegisterCompressionRoutes exposes the Phase 9 endpoints.
func RegisterCompressionRoutes(r *Router, svc *service.CompressionService) error {
	api := &compressionAPI{svc: svc}
	routes := []struct {
		method, pattern string
		h               http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/projects/{project}/settings/compression", api.getSettings},
		{http.MethodPut, "/api/v1/projects/{project}/settings/compression", api.putSettings},
		{http.MethodPost, "/api/v1/projects/{project}/prompts/{prompt}/worker-prompt", api.prepare},
		{http.MethodPost, "/api/v1/projects/{project}/prompts/{prompt}/compression-preview", api.preview},
		{http.MethodGet, "/api/v1/projects/{project}/prompts/{prompt}/compression-attempts", api.attempts},
		{http.MethodGet, "/api/v1/projects/{project}/prompts/{prompt}/compression-attempts/{attempt}/diff", api.diff},
		{http.MethodGet, "/api/v1/projects/{project}/compression-metrics", api.metrics},
		{http.MethodGet, "/api/v1/projects/{project}/compression-runs", api.projectRuns},
		{http.MethodGet, "/api/v1/projects/{project}/compression-attempts", api.projectAttempts},
		{http.MethodGet, "/api/v1/projects/{project}/worker-prompts/{worker}/lineage", api.lineage},
	}
	for _, rt := range routes {
		if err := r.RegisterAPI(rt.method, rt.pattern, rt.h); err != nil {
			return err
		}
	}
	return nil
}

func compressionServiceError(err error) error {
	var se *compression.SettingsError
	switch {
	case errors.As(err, &se):
		return NewValidationFields("invalid_compression_settings", "Compression settings are invalid.", se.Fields)
	case errors.Is(err, service.ErrInvalidInput):
		return NewValidationCode("invalid_compression_request", "Compression request is invalid.")
	case errors.Is(err, service.ErrNotFound):
		return NewNotFoundError("resource")
	case errors.Is(err, compression.ErrCompressionTargetImpossible):
		return NewConflictCode("compression_target_impossible", "Exact-preserve content alone exceeds the compression target.")
	case errors.Is(err, compression.ErrCanonicalExceedsContext):
		return NewConflictCode("canonical_exceeds_context", "Compression is disabled and the canonical prompt exceeds the worker context.")
	case errors.Is(err, compression.ErrPromptPreparationFailed):
		return NewConflictCode("prompt_preparation_failed", "Compression failed and the canonical prompt does not fit the worker context.")
	case errors.Is(err, service.ErrConflict):
		return NewConflictError("compression state conflict")
	default:
		return NewInternalError("compression request failed")
	}
}

func pathUUIDs(r *http.Request, names ...string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(names))
	for _, n := range names {
		id, err := uuid.Parse(r.PathValue(n))
		if err != nil {
			return nil, NewNotFoundError("resource")
		}
		out = append(out, id)
	}
	return out, nil
}

// --- settings -------------------------------------------------------------

type compressionSettingsDTO struct {
	Mode            string  `json:"mode"`
	Provider        string  `json:"provider"`
	Model           string  `json:"model"`
	ContextWindow   int     `json:"context_window"`
	TargetRatio     float64 `json:"target_ratio"`
	TargetTokens    int     `json:"target_tokens"`
	EffectiveTarget int     `json:"effective_target_tokens"`
	MinimumSavings  float64 `json:"minimum_savings"`
	MaxAttempts     int     `json:"max_attempts"`
	ValidationMode  string  `json:"validation_mode"`
	TimeoutMS       int64   `json:"timeout_ms"`
	BackoffBaseMS   int64   `json:"backoff_base_ms"`
	BackoffMaxMS    int64   `json:"backoff_max_ms"`
	Enabled         bool    `json:"enabled"`
	Stored          bool    `json:"stored"`
}

func toSettingsDTO(s compression.Settings, stored bool) compressionSettingsDTO {
	return compressionSettingsDTO{
		Mode: string(s.Mode), Provider: s.Provider, Model: s.Model, ContextWindow: s.ContextWindow,
		TargetRatio: s.TargetRatio, TargetTokens: s.TargetTokens, EffectiveTarget: s.EffectiveTarget(),
		MinimumSavings: s.MinimumSavings, MaxAttempts: s.MaxAttempts, ValidationMode: string(s.ValidationMode),
		TimeoutMS: s.Timeout.Milliseconds(), BackoffBaseMS: s.BackoffBase.Milliseconds(), BackoffMaxMS: s.BackoffMax.Milliseconds(),
		Enabled: s.Enabled(), Stored: stored,
	}
}

func (a *compressionAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	st, stored, err := a.svc.EffectiveSettings(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toSettingsDTO(st, stored))
}

type compressionSettingsInput struct {
	Mode           string  `json:"mode"`
	TargetRatio    float64 `json:"target_ratio"`
	TargetTokens   int     `json:"target_tokens"`
	MinimumSavings float64 `json:"minimum_savings"`
	MaxAttempts    int     `json:"max_attempts"`
	ValidationMode string  `json:"validation_mode"`
	TimeoutMS      int64   `json:"timeout_ms"`
	BackoffBaseMS  int64   `json:"backoff_base_ms"`
	BackoffMaxMS   int64   `json:"backoff_max_ms"`
}

var compressionSettingsFields = map[string]bool{
	"mode": true, "target_ratio": true, "target_tokens": true, "minimum_savings": true, "max_attempts": true,
	"validation_mode": true, "timeout_ms": true, "backoff_base_ms": true, "backoff_max_ms": true,
}

func (a *compressionAPI) putSettings(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	def := compression.DefaultSettings()
	in := compressionSettingsInput{
		Mode: string(def.Mode), MinimumSavings: def.MinimumSavings, MaxAttempts: def.MaxAttempts,
		ValidationMode: string(def.ValidationMode), TimeoutMS: def.Timeout.Milliseconds(),
		BackoffBaseMS: def.BackoffBase.Milliseconds(), BackoffMaxMS: def.BackoffMax.Milliseconds(),
	}
	if err := decodeStrictJSON(r, "invalid_compression_settings", compressionSettingsFields, []string{"mode"}, &in); err != nil {
		WriteError(w, r, err)
		return
	}
	st := compression.Settings{
		Mode: compression.Mode(in.Mode), TargetRatio: in.TargetRatio, TargetTokens: in.TargetTokens,
		MinimumSavings: in.MinimumSavings, MaxAttempts: in.MaxAttempts, ValidationMode: compression.ValidationMode(in.ValidationMode),
		Timeout:     time.Duration(in.TimeoutMS) * time.Millisecond,
		BackoffBase: time.Duration(in.BackoffBaseMS) * time.Millisecond,
		BackoffMax:  time.Duration(in.BackoffMaxMS) * time.Millisecond,
	}
	out, err := a.svc.UpdateCompressionSettings(r.Context(), ids[0], st)
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, toSettingsDTO(out, true))
}

// --- prepare / preview ------------------------------------------------------

type workerPromptDTO struct {
	ID                 string  `json:"id"`
	CanonicalPromptID  string  `json:"canonical_prompt_id"`
	CompressedPromptID *string `json:"compressed_prompt_id"`
	RunID              string  `json:"run_id"`
	Source             string  `json:"source"`
	Content            string  `json:"content"`
	Hash               string  `json:"hash"`
	EstimatedTokens    int     `json:"estimated_tokens"`
	CreatedAt          string  `json:"created_at"`
}

func toWorkerDTO(wp *compression.WorkerPrompt) *workerPromptDTO {
	if wp == nil {
		return nil
	}
	return &workerPromptDTO{
		ID: wp.ID.String(), CanonicalPromptID: wp.CanonicalPromptID.String(), CompressedPromptID: uuidPtr(wp.CompressedPromptID),
		RunID: wp.RunID.String(), Source: string(wp.Source), Content: wp.Content, Hash: wp.Hash,
		EstimatedTokens: wp.EstimatedTokens, CreatedAt: wp.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

type runDTO struct {
	ID                 string                         `json:"id"`
	CanonicalPromptID  string                         `json:"canonical_prompt_id"`
	RequestIdentity    string                         `json:"request_identity"`
	ConfigHash         string                         `json:"config_hash"`
	InstructionVersion int                            `json:"instruction_version"`
	Provider           string                         `json:"provider"`
	Model              string                         `json:"model"`
	Mode               string                         `json:"mode"`
	TargetTokens       int                            `json:"target_tokens"`
	CanonicalTokens    int                            `json:"canonical_tokens"`
	Outcome            string                         `json:"outcome"`
	Reason             string                         `json:"reason"`
	Attempts           int                            `json:"attempts"`
	Dropped            []compression.DroppedComponent `json:"dropped"`
	WorkerPromptID     *string                        `json:"worker_prompt_id"`
	CreatedAt          string                         `json:"created_at"`
}

func toRunDTO(r *compression.Run) *runDTO {
	if r == nil {
		return nil
	}
	d := r.Dropped
	if d == nil {
		d = []compression.DroppedComponent{}
	}
	return &runDTO{
		ID: r.ID.String(), CanonicalPromptID: r.CanonicalPromptID.String(), RequestIdentity: r.RequestIdentity,
		ConfigHash: r.ConfigHash, InstructionVersion: r.InstructionVersion, Provider: r.Provider, Model: r.Model,
		Mode: string(r.Mode), TargetTokens: r.TargetTokens, CanonicalTokens: r.CanonicalTokens,
		Outcome: string(r.Outcome), Reason: r.Reason, Attempts: r.Attempts, Dropped: d,
		WorkerPromptID: uuidPtr(r.WorkerPromptID), CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

func (a *compressionAPI) prepare(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "prompt")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.svc.PrepareWorkerPrompt(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run":           toRunDTO(res.Run),
		"worker_prompt": toWorkerDTO(res.WorkerPrompt),
		"attempts":      toAttemptDTOs(res.Attempts, res.Run),
		"reused":        res.Reused,
	})
}

type previewDTO struct {
	CanonicalPromptID string                         `json:"canonical_prompt_id"`
	Decision          string                         `json:"decision"`
	Reason            string                         `json:"reason"`
	CanonicalTokens   int                            `json:"canonical_tokens"`
	CompressedTokens  int                            `json:"compressed_tokens"`
	TargetTokens      int                            `json:"target_tokens"`
	TokensSaved       int                            `json:"tokens_saved"`
	Reduction         float64                        `json:"reduction_percent"`
	ValidationStatus  string                         `json:"validation_status"`
	Items             []compression.ItemResult       `json:"preserved_items"`
	Failures          []compression.Failure          `json:"failures"`
	Attempts          int                            `json:"attempts"`
	Dropped           []compression.DroppedComponent `json:"dropped"`
	Preview           string                         `json:"compressed_preview"`
	Source            string                         `json:"source"`
}

func (a *compressionAPI) preview(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "prompt")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := a.svc.PreviewCompression(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	out := previewDTO{
		CanonicalPromptID: p.CanonicalPromptID.String(), Decision: string(p.Decision), Reason: p.Reason,
		CanonicalTokens: p.CanonicalTokens, CompressedTokens: p.CompressedTokens, TargetTokens: p.TargetTokens,
		TokensSaved: p.Metrics.TokensSaved, Reduction: p.Metrics.PercentSaved,
		ValidationStatus: string(p.ValidationStatus), Items: p.Items, Failures: p.Failures, Attempts: p.Attempts,
		Dropped: p.Dropped, Preview: p.Preview, Source: string(p.Source),
	}
	if out.Items == nil {
		out.Items = []compression.ItemResult{}
	}
	if out.Failures == nil {
		out.Failures = []compression.Failure{}
	}
	if out.Dropped == nil {
		out.Dropped = []compression.DroppedComponent{}
	}
	writeJSON(w, http.StatusOK, out)
}

// --- attempts / diff -------------------------------------------------------

type attemptDTO struct {
	ID                 string                        `json:"id"`
	RunID              string                        `json:"run_id"`
	Attempt            int                           `json:"attempt"`
	Status             string                        `json:"status"`
	Accepted           bool                          `json:"accepted"`
	Provider           string                        `json:"provider"`
	Model              string                        `json:"model"`
	ErrorKind          string                        `json:"error_kind,omitempty"`
	ErrorMessage       string                        `json:"error_message,omitempty"`
	CanonicalTokens    int                           `json:"canonical_tokens"`
	CompressedTokens   int                           `json:"compressed_tokens"`
	InputTokens        int                           `json:"input_tokens"`
	OutputTokens       int                           `json:"output_tokens"`
	DurationMS         int64                         `json:"duration_ms"`
	CompressedPromptID *string                       `json:"compressed_prompt_id"`
	Validation         *compression.ValidationResult `json:"validation"`
	RunOutcome         string                        `json:"run_outcome"`
	CreatedAt          string                        `json:"created_at"`
}

func toAttemptDTOs(atts []compression.Attempt, run *compression.Run) []attemptDTO {
	out := make([]attemptDTO, 0, len(atts))
	for _, at := range atts {
		out = append(out, toAttemptDTO(at, run))
	}
	return out
}

func toAttemptDTO(at compression.Attempt, run *compression.Run) attemptDTO {
	d := attemptDTO{
		ID: at.ID.String(), RunID: at.RunID.String(), Attempt: at.Number, Status: string(at.Status), Accepted: at.Accepted(),
		Provider: at.Provider, Model: at.Model, ErrorKind: string(at.ErrorKind), ErrorMessage: at.ErrorMessage,
		InputTokens: at.InputTokens, OutputTokens: at.OutputTokens, DurationMS: at.Duration.Milliseconds(),
		CompressedPromptID: uuidPtr(at.CompressedPromptID), Validation: at.Validation,
		CreatedAt: at.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if at.Validation != nil {
		d.CanonicalTokens, d.CompressedTokens = at.Validation.Canonical, at.Validation.Output
	}
	if run != nil {
		d.RunOutcome = string(run.Outcome)
		if d.CanonicalTokens == 0 {
			d.CanonicalTokens = run.CanonicalTokens
		}
	}
	return d
}

func (a *compressionAPI) attempts(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "prompt")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	items, err := a.svc.ListAttempts(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	runs, err := a.svc.ListRuns(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	out := make([]attemptDTO, 0, len(items))
	for _, it := range items {
		out = append(out, toAttemptDTO(it.Attempt, it.Run))
	}
	runDTOs := make([]*runDTO, 0, len(runs))
	for _, rn := range runs {
		runDTOs = append(runDTOs, toRunDTO(rn))
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempts": out, "count": len(out), "runs": runDTOs})
}

func (a *compressionAPI) diff(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "prompt", "attempt")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	d, err := a.svc.AttemptDiff(r.Context(), ids[0], ids[1], ids[2])
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	lines := d.Lines
	if lines == nil {
		lines = []compression.DiffLine{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "removed": d.Removed, "added": d.Added, "equal": d.Equal})
}

// --- metrics / lineage ------------------------------------------------------

func (a *compressionAPI) metrics(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	m, err := a.svc.ProjectMetrics(r.Context(), ids[0])
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runs":                      m.Runs,
		"prompts_compressed":        m.PromptsCompressed,
		"prompts_bypassed":          m.PromptsBypassed,
		"prompts_fallback":          m.PromptsFallback,
		"prompts_failed":            m.PromptsFailed,
		"average_canonical_tokens":  m.AverageCanonicalTok,
		"average_compressed_tokens": m.AverageCompressedTok,
		"average_reduction_percent": m.AverageReduction,
		"attempts":                  m.Attempts,
		"validation_failures":       m.ValidationFailures,
		"validation_failure_rate":   m.ValidationFailureRate,
		"retry_rate":                m.RetryRate,
		"fallback_rate":             m.FallbackRate,
	})
}

func (a *compressionAPI) lineage(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project", "worker")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	l, err := a.svc.Lineage(r.Context(), ids[0], ids[1])
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	var compressedID, acceptedID *string
	if l.CompressedPrompt != nil {
		s := l.CompressedPrompt.ID.String()
		compressedID = &s
	}
	if l.AcceptedAttempt != nil {
		s := l.AcceptedAttempt.ID.String()
		acceptedID = &s
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"worker_prompt":        toWorkerDTO(l.WorkerPrompt),
		"compressed_prompt_id": compressedID,
		"accepted_attempt_id":  acceptedID,
		"run":                  toRunDTO(l.Run),
		"attempts":             toAttemptDTOs(l.Attempts, l.Run),
		"canonical_prompt_id":  l.Canonical.ID.String(),
		"canonical_hash":       l.Canonical.Hash,
		"context_snapshot_id":  l.ContextSnapshotID.String(),
	})
}

// projectRuns pages the project's preparation runs, newest first.
func (a *compressionAPI) projectRuns(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit", 50)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset = max(offset, 0)
	runs, total, err := a.svc.ProjectRuns(r.Context(), ids[0], limit, offset)
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	out := make([]*runDTO, 0, len(runs))
	for _, rn := range runs {
		out = append(out, toRunDTO(rn))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out, "page": readmodel.Page{Total: total, Limit: limit, Offset: offset}})
}

// projectAttempts lists the project's latest compression attempts.
func (a *compressionAPI) projectAttempts(w http.ResponseWriter, r *http.Request) {
	ids, err := pathUUIDs(r, "project")
	if err != nil {
		WriteError(w, r, err)
		return
	}
	limit, err := queryInt(r, "limit", 50)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	atts, err := a.svc.ProjectAttempts(r.Context(), ids[0], limit)
	if err != nil {
		WriteError(w, r, compressionServiceError(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempts": toAttemptDTOs(atts, nil)})
}
