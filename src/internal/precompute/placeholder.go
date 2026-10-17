package precompute

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/google/uuid"

	"overseer/internal/compression"
)

// ACTIVE_TASK placeholder (PLAN Decision 5, user requirement): a future
// unit projected without an ACTIVE_TASK gets a placeholder value instead of
// a guessed one. The token is deterministic per unit and carries the
// compression package's placeholder marker, so every dispatch guard can
// recognise it.

// PlaceholderToken is the ACTIVE_TASK placeholder of one unit (U11.P.1).
func PlaceholderToken(unit uuid.UUID) string {
	return compression.PlaceholderMarker + "ACTIVE_TASK:" + unit.String() + compression.PlaceholderEnd
}

// PlaceholderHash is the placeholder pin's hash.
func PlaceholderHash(unit uuid.UUID) string {
	s := sha256.Sum256([]byte(PlaceholderToken(unit)))
	return hex.EncodeToString(s[:])
}

// PlaceholderPin is the projected ACTIVE_TASK pin: version 1 (the version
// a first derivation creates; composition requires a captured version)
// with the token's hash — the hash, never the version, distinguishes it
// from any real ACTIVE_TASK.
func PlaceholderPin(unit uuid.UUID) FilePin {
	return FilePin{TypeKey: "ACTIVE_TASK", Version: 1, Hash: PlaceholderHash(unit), Placeholder: true}
}

// ContainsPlaceholder reports whether text carries any placeholder marker.
func ContainsPlaceholder(text string) bool { return compression.ContainsPlaceholder(text) }

// Placeholder substitution errors.
var (
	ErrPlaceholderMissing   = errors.New("precompute: placeholder slot missing")
	ErrPlaceholderDuplicate = errors.New("precompute: placeholder slot not unique")
	ErrPlaceholderSpoofed   = errors.New("precompute: real content contains a placeholder marker")
)

// ReplacePlaceholder substitutes the real ACTIVE_TASK value for the unit's
// placeholder (U11.P.2/U11.P.3/U11.P.4): exactly one slot must exist, the
// real value must not itself carry a marker, and no marker may remain.
func ReplacePlaceholder(text string, unit uuid.UUID, real string) (string, error) {
	if compression.ContainsPlaceholder(real) {
		return "", ErrPlaceholderSpoofed
	}
	tok := PlaceholderToken(unit)
	switch n := strings.Count(text, tok); {
	case n == 0:
		return "", ErrPlaceholderMissing
	case n > 1:
		return "", ErrPlaceholderDuplicate
	}
	out := strings.Replace(text, tok, real, 1)
	if compression.ContainsPlaceholder(out) {
		return "", ErrPlaceholderDuplicate
	}
	return out, nil
}
