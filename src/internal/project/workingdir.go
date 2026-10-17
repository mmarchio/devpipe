package project

import "os"

// DirectoryStatus captures the validation state of a configured working
// directory (WBS 3.4). The three states are distinct:
//
//	Configured: a path was provided (non-empty).
//	Valid: the path exists on disk and is a directory.
//	Accessible: the path is valid and os.Stat reports its attributes
//	(permissions sufficient for the validating process to inspect it).
type DirectoryStatus struct {
	Path       string
	Configured bool
	Valid      bool
	Accessible bool
}

// ValidateWorkingDirectory reports the status of the configured working
// directory without changing the process working directory and without
// trimming, resolving, or otherwise rewriting the path (WBS 3.4).
//
// It is a pure, side-effect-free check: it stat()s the path and returns the
// three-state report. A missing path yields a zero-value report (nothing
// configured); a path that is a regular file is configured but not valid.
func ValidateWorkingDirectory(path string) DirectoryStatus {
	if path == "" {
		return DirectoryStatus{}
	}
	rep := DirectoryStatus{Path: path, Configured: true}
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		rep.Valid = true
		rep.Accessible = true
	}
	return rep
}
