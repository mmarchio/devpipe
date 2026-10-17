package worker

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Working-directory validation errors (WBS 12.25).
var (
	ErrDirectoryEmpty      = errors.New("worker: working directory is empty")
	ErrDirectoryRelative   = errors.New("worker: working directory must be absolute")
	ErrDirectoryDisallowed = errors.New("worker: working directory outside the allowed roots")
	ErrDirectoryInvalid    = errors.New("worker: working directory does not exist or is not a directory")
)

// ValidateDirectoryRequest checks a requested directory before it is sent
// to the worker. allowedRoots (when non-empty) is the sandbox; check, when
// non-nil, verifies the path locally (only meaningful for a worker running
// on this host). Shell output is never the only check.
func ValidateDirectoryRequest(path string, allowedRoots []string, check func(string) bool) error {
	if strings.TrimSpace(path) == "" {
		return ErrDirectoryEmpty
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: %q", ErrDirectoryRelative, path)
	}
	if len(allowedRoots) > 0 {
		clean := filepath.Clean(path)
		ok := false
		for _, root := range allowedRoots {
			r := filepath.Clean(root)
			if clean == r || strings.HasPrefix(clean, r+string(filepath.Separator)) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%w: %q", ErrDirectoryDisallowed, path)
		}
	}
	if check != nil && !check(path) {
		return fmt.Errorf("%w: %q", ErrDirectoryInvalid, path)
	}
	return nil
}
