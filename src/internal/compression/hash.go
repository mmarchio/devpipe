package compression

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"strconv"
)

// frameField appends a self-delimiting length-prefixed field (8-byte
// big-endian length of tag+payload, tag, payload) — the same framing as the
// Phase 8 canonical prompt hash and cache key, so field boundaries are
// unambiguous and no input can cancel another's contribution.
func frameField(dst []byte, tag string, payload []byte) []byte {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(tag)+len(payload)))
	dst = append(dst, lenBuf[:]...)
	dst = append(dst, tag...)
	return append(dst, payload...)
}

func intField(v int) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

func floatField(v float64) []byte {
	return []byte(strconv.FormatFloat(v, 'g', -1, 64))
}

func digest(buf []byte) string {
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// CompressedPromptHash is the deterministic hash over compressed content
// (WBS 9.14), in its own hash domain (tag "compressed_content") so it is
// separate from the canonical prompt hash.
func CompressedPromptHash(content string) string {
	return digest(frameField(nil, "compressed_content", []byte(content)))
}

// WorkerPromptHash hashes the exact content sent to the worker plus its
// source (WBS 9.33): same content + source → same hash; a canonical and a
// compressed worker prompt are always distinguishable (T9.33.3).
func WorkerPromptHash(content string, source WorkerPromptSource) string {
	buf := frameField(nil, "worker_content", []byte(content))
	buf = frameField(buf, "worker_source", []byte(source))
	return digest(buf)
}

// ConfigHashInput is the semantic compression configuration (WBS 9.44).
type ConfigHashInput struct {
	Provider           string
	Model              string
	InstructionVersion int
	InstructionHash    string
	TargetTokens       int
	// ContextWindow is policy input: the NEVER-mode rejection and the
	// canonical-fallback decision depend on it, so a reused result must
	// have been prepared against the same window.
	ContextWindow  int
	Mode           Mode
	MinimumSavings float64
	MaxAttempts    int
	ValidationMode ValidationMode
}

// ConfigHash hashes the semantic compression configuration (WBS 9.44) — a
// Phase 10 cache-key input. Timeouts/backoff are operational, not semantic,
// and are excluded.
func ConfigHash(in ConfigHashInput) string {
	buf := frameField(nil, "provider", []byte(in.Provider))
	buf = frameField(buf, "model", []byte(in.Model))
	buf = frameField(buf, "instruction_version", intField(in.InstructionVersion))
	buf = frameField(buf, "instruction_hash", []byte(in.InstructionHash))
	buf = frameField(buf, "target_tokens", intField(in.TargetTokens))
	buf = frameField(buf, "context_window", intField(in.ContextWindow))
	buf = frameField(buf, "mode", []byte(in.Mode))
	buf = frameField(buf, "minimum_savings", floatField(in.MinimumSavings))
	buf = frameField(buf, "max_attempts", intField(in.MaxAttempts))
	buf = frameField(buf, "validation_mode", []byte(in.ValidationMode))
	return digest(buf)
}

// ConfigHashFor derives the ConfigHashInput from settings + instruction.
func ConfigHashFor(s Settings, in Instruction) string {
	return ConfigHash(ConfigHashInput{
		Provider: s.Provider, Model: s.Model,
		InstructionVersion: in.Version, InstructionHash: in.Hash,
		TargetTokens: s.EffectiveTarget(), ContextWindow: s.ContextWindow, Mode: s.Mode,
		MinimumSavings: s.MinimumSavings, MaxAttempts: s.MaxAttempts,
		ValidationMode: s.ValidationMode,
	})
}

// RequestIdentity is the deterministic compression request identity (WBS
// 9.43): the canonical prompt hash + the configuration hash. Model output
// is excluded (it is not assumed deterministic).
func RequestIdentity(canonicalHash, configHash string) string {
	buf := frameField(nil, "canonical_hash", []byte(canonicalHash))
	buf = frameField(buf, "config_hash", []byte(configHash))
	return digest(buf)
}
