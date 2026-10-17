package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/persistence"
)

// ConfigVersionHeader carries the project configuration version on
// guarded settings routes (WBS 17.140).
const ConfigVersionHeader = "X-Config-Version"

// ConfigVersioner reads / bumps the project configuration version.
type ConfigVersioner interface {
	GetConfigVersion(ctx context.Context, projectID uuid.UUID) (int, error)
	BumpConfigVersion(ctx context.Context, projectID uuid.UUID, expected *int) (int, error)
	RevertConfigVersion(ctx context.Context, projectID uuid.UUID, to int) error
}

// guardedSettings are the sub-configuration routes protected by the
// configuration version (P17-46).
var guardedSettings = []string{"compression", "continuation", "engine", "execution", "precompute", "prompt-cache",
	"result-protocol", "planning", "worker"}

func configGuarded(pattern string) bool {
	for _, s := range guardedSettings {
		if strings.HasSuffix(pattern, "/settings/"+s) {
			return true
		}
	}
	return false
}

// SetConfigGuard installs the configuration-version guard for settings
// routes registered afterwards.
func (r *Router) SetConfigGuard(cv ConfigVersioner) { r.configGuard = cv }

// configGuardHandler: GET publishes the current version in
// X-Config-Version; PUT accepts an optional "version" member (removed
// before the resource handler sees the body), bumps the version with a
// compare-and-set (409 when stale) and reverts the bump when the write
// fails. Every successful write bumps the version, so a writer that echoes
// it always detects a concurrent change.
func configGuardHandler(cv ConfigVersioner, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pid, err := uuid.Parse(r.PathValue("project"))
		if err != nil {
			h.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if v, err := cv.GetConfigVersion(r.Context(), pid); err == nil {
				w.Header().Set(ConfigVersionHeader, strconv.Itoa(v))
			}
			h.ServeHTTP(w, r)
			return
		}
		var expected *int
		if r.Body != nil && r.Body != http.NoBody {
			raw, _ := io.ReadAll(r.Body)
			var obj map[string]json.RawMessage
			if json.Unmarshal(raw, &obj) == nil && obj != nil {
				if v, ok := obj["version"]; ok {
					var n int
					if err := json.Unmarshal(v, &n); err != nil || n <= 0 {
						WriteError(w, r, NewValidationFields(CodeInvalidInput, "Version is invalid.", map[string]string{"version": "must be a positive integer"}))
						return
					}
					expected = &n
					delete(obj, "version")
					raw, _ = json.Marshal(obj)
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			r.ContentLength = int64(len(raw))
		}
		if hv := r.Header.Get(ConfigVersionHeader); expected == nil && hv != "" {
			if n, err := strconv.Atoi(hv); err == nil && n > 0 {
				expected = &n
			}
		}
		newV, err := cv.BumpConfigVersion(r.Context(), pid, expected)
		switch {
		case errors.Is(err, persistence.ErrConflict):
			WriteError(w, r, NewConflictCode("stale_configuration", "The configuration changed since it was loaded; reload before saving."))
			return
		case errors.Is(err, persistence.ErrNotFound):
			WriteError(w, r, NewNotFoundError("project"))
			return
		case err != nil:
			WriteError(w, r, NewInternalError("configuration version update failed"))
			return
		}
		w.Header().Set(ConfigVersionHeader, strconv.Itoa(newV))
		sw := &auditStatus{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		if sw.status >= 300 {
			_ = cv.RevertConfigVersion(context.WithoutCancel(r.Context()), pid, newV-1)
		}
	})
}
