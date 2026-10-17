package execution

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CanonicalizeWorkingDirectory returns the stable absolute representation
// of path (WBS 15.16): absolute, cleaned and symlink-resolved. A path that
// does not exist is returned absolute + cleaned with the stat error.
func CanonicalizeWorkingDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("execution: working directory not configured")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs, err
	}
	return filepath.Clean(real), nil
}

// CheckWorkingDirectory resolves, canonicalizes and validates the configured
// working directory against the allowed roots (WBS 15.15–15.17). It returns
// the canonical path (empty when unresolvable) and the readiness errors.
// Empty roots = unrestricted.
func CheckWorkingDirectory(path string, allowedRoots []string) (string, []ReadinessError) {
	bad := func(msg string) []ReadinessError {
		return []ReadinessError{{Category: CategoryWorkingDir, Code: CodeWorkdirInvalid, Resource: path, Message: msg}}
	}
	if strings.TrimSpace(path) == "" {
		return "", bad("working directory is not configured")
	}
	canon, err := CanonicalizeWorkingDirectory(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", bad("working directory does not exist")
		}
		return "", bad("working directory cannot be resolved")
	}
	info, err := os.Stat(canon)
	if err != nil {
		return "", bad("working directory is not accessible")
	}
	if !info.IsDir() {
		return "", bad("working directory is not a directory")
	}
	f, err := os.Open(canon)
	if err != nil {
		return "", bad("working directory is not accessible")
	}
	_, rerr := f.Readdirnames(1)
	_ = f.Close()
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return "", bad("working directory is not readable")
	}
	if !WithinAllowedRoots(canon, allowedRoots) {
		return "", bad("working directory is outside the allowed roots")
	}
	return canon, nil
}

// WithinAllowedRoots reports whether canonical lies inside one of roots
// (each canonicalized; path-boundary match, never a string prefix). Empty
// roots = unrestricted. An unresolvable root never matches.
func WithinAllowedRoots(canonical string, roots []string) bool {
	if len(roots) == 0 {
		return true
	}
	for _, r := range roots {
		cr, err := CanonicalizeWorkingDirectory(r)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(cr, canonical)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)) {
			return true
		}
	}
	return false
}
