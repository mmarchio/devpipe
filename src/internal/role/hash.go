package role

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// HashSystemPromptContent returns a deterministic content hash for a role's
// system prompt (WBS 5.6). The hash is a pure function of the content string:
// identical content always yields the identical hash, and a SHA-256 digest
// means any content change changes the hash. The empty string is a valid
// input with a well-defined hash. The result is stable across runs and
// processes (no time, random, or environment input) — it feeds the future
// prompt-cache key and the role configuration hash.
func HashSystemPromptContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// ValidateSystemPrompt enforces that a system prompt is non-empty (WBS 5.4,
// T5.4.5). Whitespace-only content is treated as empty: a system prompt that
// carries no instruction is invalid. A valid (non-empty) prompt returns nil.
func ValidateSystemPrompt(content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("role: system prompt must not be empty")
	}
	return nil
}
