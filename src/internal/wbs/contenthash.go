package wbs

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashContent returns a deterministic content hash for WBS version content
// (WBS 4.11). The hash is a pure function of the content string: identical
// content always yields the identical hash, and a collision-resistant digest
// (SHA-256) means any content change changes the hash. The empty string is a
// valid input with a well-defined hash. The result is stable across runs and
// processes (no time, random, or environment input).
func HashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
