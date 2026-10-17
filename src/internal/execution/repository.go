package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// RepositorySnapshot is observational repository metadata (WBS 15.19/15.80).
type RepositorySnapshot struct {
	WorkingDirectory string  `json:"working_directory"`
	IsGitRepository  bool    `json:"is_git_repository"`
	HeadCommit       *string `json:"head_commit"`
	Branch           *string `json:"branch"`
	Dirty            bool    `json:"dirty"`
	// Fingerprint is SHA-256 over HEAD + the porcelain status bytes
	// (WBS 15.80 "working-tree fingerprint where practical").
	Fingerprint string `json:"fingerprint"`
}

// Hash is a deterministic identity of the snapshot.
func (s RepositorySnapshot) Hash() string {
	h := sha256.New()
	str := func(p *string) string {
		if p == nil {
			return "\x00nil"
		}
		return *p
	}
	fmt.Fprintf(h, "wd=%q\ngit=%t\nhead=%q\nbranch=%q\ndirty=%t\nfp=%q\n",
		s.WorkingDirectory, s.IsGitRepository, str(s.HeadCommit), str(s.Branch), s.Dirty, s.Fingerprint)
	return hex.EncodeToString(h.Sum(nil))
}

// RepositoryInspector reads repository metadata without mutating the
// repository (WBS 15.21). Implementations live outside the domain
// (internal/repoinspect).
type RepositoryInspector interface {
	Inspect(ctx context.Context, workingDirectory string) (RepositorySnapshot, error)
}

// EvaluateRepositoryPolicy applies the git-required and dirty policies
// (WBS 15.18/15.20).
func EvaluateRepositoryPolicy(snap RepositorySnapshot, st Settings) (errs, warns []ReadinessError) {
	if !snap.IsGitRepository {
		if st.RequireGitRepository {
			errs = append(errs, ReadinessError{Category: CategoryRepository, Code: CodeRepositoryInvalid,
				Resource: snap.WorkingDirectory, Message: "working directory is not a git repository"})
		}
		return errs, warns
	}
	if snap.Dirty {
		e := ReadinessError{Category: CategoryRepository, Code: CodeRepositoryInvalid,
			Resource: snap.WorkingDirectory, Message: "repository working tree is dirty"}
		switch st.DirtyPolicy {
		case DirtyReject:
			errs = append(errs, e)
		case DirtyWarn:
			warns = append(warns, e)
		}
	}
	return errs, warns
}

// RepositoryIdentity identifies the repository an execution works in
// (WBS 18.49, PLAN Decision 16): the work-tree top level, the root commit
// (an unrelated repository has a different one) and the branch. A
// directory that is not a git work tree has IsGit false and only its path.
type RepositoryIdentity struct {
	Toplevel   string `json:"toplevel"`
	RootCommit string `json:"root_commit"`
	Branch     string `json:"branch"`
	IsGit      bool   `json:"is_git"`
	// Head is the observed HEAD commit — compared with the expected HEAD
	// (WBS 18.50), not part of the recorded identity.
	Head string `json:"head,omitempty"`
}

// RepositoryIdentifier reads a working directory's identity. A missing
// directory is an error wrapping fs.ErrNotExist.
type RepositoryIdentifier interface {
	Identify(ctx context.Context, workingDirectory string) (RepositoryIdentity, error)
	// Writable proves the working directory (and its git directory) can
	// be written.
	Writable(ctx context.Context, workingDirectory string) error
}

// IdentityMismatch compares the recorded identity with the current one
// (WBS 18.49 / 18.51). It returns "" when they match, else a reason; the
// branch reason is reported separately (a branch switch is a HEAD change,
// not a different repository).
func IdentityMismatch(want, got RepositoryIdentity) (identity, branch string) {
	switch {
	case want.IsGit && !got.IsGit:
		return "the working directory is no longer a git repository", ""
	case want.IsGit && want.RootCommit != got.RootCommit:
		return fmt.Sprintf("repository root commit changed (%s → %s): an unrelated repository", short(want.RootCommit), short(got.RootCommit)), ""
	case want.Toplevel != got.Toplevel:
		return fmt.Sprintf("repository top level moved (%s → %s)", want.Toplevel, got.Toplevel), ""
	case want.IsGit && want.Branch != got.Branch:
		return "", fmt.Sprintf("branch switched (%q → %q)", want.Branch, got.Branch)
	}
	return "", ""
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "<none>"
	}
	return s
}
