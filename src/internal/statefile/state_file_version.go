package statefile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// StateFileVersion is a single immutable content version of a state file
// (WBS 7.5). Each version carries the exact content bytes it was created
// with, a deterministic SHA-256 content hash over those bytes (WBS 7.6), and
// an approximate token estimate stored as metadata (WBS 7.28). Versions are
// append-only: updates create a new version, never rewriting history.
type StateFileVersion struct {
	ID          uuid.UUID
	StateFileID uuid.UUID

	Version int
	Content string

	ContentHash string

	// TokenEstimate is approximate metadata (a bytes/4 heuristic), not an
	// authoritative tokenizer output (WBS 7.28).
	TokenEstimate int

	CreatedAt time.Time
}

// ContentHash returns the deterministic SHA-256 hash of the exact content
// bytes, hex-encoded (WBS 7.6). Identical bytes always produce the identical
// hash; a single changed byte changes the hash. These hashes later feed
// prompt-cache keys.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// TokenEstimate returns an approximate token count for content (WBS 7.28).
// It is a coarse bytes/4 heuristic, NOT an authoritative tokenizer — it exists
// so prompt composition can budget against context windows without depending
// on a specific model's tokenizer. Empty content estimates 0.
func TokenEstimate(content string) int {
	if content == "" {
		return 0
	}
	// Approximate: roughly four bytes per token.
	return len(content) / 4
}

// AppendVersion creates the next content version for a state file (WBS 7.5).
// The new version number is MAX(existing)+1 derived from the file's
// CurrentVersion, so the first content is v1 and each update advances. The
// ContentHash and TokenEstimate are computed from the exact content and
// stored on the version. now is injectable for deterministic tests.
func AppendVersion(f *StateFile, content string, now func() time.Time) (*StateFileVersion, error) {
	if f == nil {
		return nil, fmt.Errorf("statefile: version requires a state file")
	}
	// The next version is deterministic over the file's existing versions:
	// an empty file (CurrentVersion 0) yields v1; a file at vN yields vN+1.
	next := NextVersion(existingVersionsFrom(f))
	return &StateFileVersion{
		ID:            uuid.New(),
		StateFileID:   f.ID,
		Version:       next,
		Content:       content,
		ContentHash:   ContentHash(content),
		TokenEstimate: TokenEstimate(content),
		CreatedAt:     now().UTC(),
	}, nil
}

// existingVersionsFrom returns the ordered list of version numbers a state
// file currently holds: 1..CurrentVersion. A fresh file (CurrentVersion 0)
// holds none.
func existingVersionsFrom(f *StateFile) []int {
	versions := make([]int, 0, f.CurrentVersion)
	for v := 1; v <= f.CurrentVersion; v++ {
		versions = append(versions, v)
	}
	return versions
}

// NextVersion is the pure "next version" rule over an ordered set of existing
// version numbers (WBS 7.5): it returns MAX(existing)+1, so an empty set yields
// 1 (first content) and a populated set yields the next monotonically higher
// number. It is DB-free so P7-06's repository reuses it verbatim for the
// advisory-locked MAX+1 allocation.
func NextVersion(existing []int) int {
	max := 0
	for _, v := range existing {
		if v > max {
			max = v
		}
	}
	return max + 1
}

// ValidateVersionCandidate enforces the no-duplicate / no-backward guards
// (WBS 7.5.5 / 7.5.6) as a pure function over the set of already-committed
// version numbers. It rejects a candidate that is already present (a duplicate
// a concurrent writer would compute from the same MAX+1) or that is below the
// current maximum (a backward move). A candidate strictly greater than the max
// is accepted. P7-06's repository reuses this verbatim after its advisory lock.
func ValidateVersionCandidate(existing []int, candidate int) error {
	max := 0
	for _, v := range existing {
		if v == candidate {
			return fmt.Errorf("statefile: version %d already exists", candidate)
		}
		if v > max {
			max = v
		}
	}
	if candidate <= max {
		return fmt.Errorf("statefile: version %d would move backward from %d", candidate, max)
	}
	return nil
}
