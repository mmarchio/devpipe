package statefile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsafePath means a configured state-file filename is not a safe
// managed relative name under the state directory (WBS 7.21): it is
// absolute, empty, escapes the directory via .. traversal, or — after
// symlink resolution — resolves outside the directory.
var ErrUnsafePath = fmt.Errorf("statefile: unsafe materialization path")

// SafeRelPath validates a configured state-file filename against the
// state directory (WBS 7.21) and returns the absolute target path.
//
// Policy (documented per the task): configured filenames are managed
// RELATIVE names —
//   - an empty name is invalid (T7.21.5);
//   - an absolute path is rejected (T7.21.3);
//   - a name whose cleaned form escapes the state directory via .. is
//     rejected (T7.21.2) — cleaning is done relative to the state
//     directory, so "a/../b" stays inside while "../x" escapes;
//   - a name containing a NUL byte is invalid (T7.21.5).
//
// Symlink policy (T7.21.4): the path is RESOLVED (symlinks followed) and
// containment is re-verified AFTER resolution — a component of the name
// that is a symlink pointing outside the state directory is rejected.
// Resolution stops at the deepest existing prefix (the file itself does
// not exist yet), and each resolved ancestor must remain inside the
// resolved state directory.
func SafeRelPath(stateDir, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty filename", ErrUnsafePath)
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("%w: absolute path %q", ErrUnsafePath, name)
	}
	if strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("%w: invalid byte in filename %q", ErrUnsafePath, name)
	}
	if filepath.Clean(name) == "." {
		return "", fmt.Errorf("%w: %q is not a file name", ErrUnsafePath, name)
	}
	base, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	// Clean relative to the state directory: a name whose cleaned form
	// starts with .. would escape (T7.21.2).
	if rel, err := filepath.Rel(base, filepath.Join(base, name)); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("%w: %q escapes the state directory", ErrUnsafePath, name)
	}
	target := filepath.Join(base, name)

	// Symlink containment (T7.21.4): resolve the deepest existing prefix
	// and require it to stay inside the resolved base. The state dir may
	// not exist yet (the materializer creates it), so resolve each side
	// independently and compare.
	resolvedBase, err := resolveExisting(base)
	if err != nil {
		return "", err
	}
	resolvedPrefix, err := resolveExisting(target)
	if err != nil {
		return "", err
	}
	if resolvedBase != "" && resolvedPrefix != "" {
		if rel, err := filepath.Rel(resolvedBase, resolvedPrefix); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return "", fmt.Errorf("%w: %q escapes the state directory after symlink resolution", ErrUnsafePath, name)
		}
	}
	return target, nil
}

// resolveExisting returns the symlink-resolved path of the deepest
// existing prefix of p (p itself need not exist). "" when nothing on the
// path exists (the caller then skips the containment check — there is
// nothing to resolve).
func resolveExisting(p string) (string, error) {
	candidate := p
	for {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", nil // reached the filesystem root without existing
		}
		candidate = parent
	}
}
