package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Key schema versions (WBS 10.4). The version is framed into the hash AND
// prefixed onto the key, so a key built under one algorithm can never be
// found by a lookup under another.
const (
	SchemaV1 = 1
	// SchemaV2 keys the context snapshot by its runtime-independent
	// semantic hash (Phase 11, approved PLAN Decision 1) so a key projected
	// before a workflow transition can equal the actual key after it.
	SchemaV2 = 2
	// CurrentSchema is the key algorithm used for new entries.
	CurrentSchema = SchemaV2
)

// ErrUnknownSchema rejects an unsupported key schema version (T10.4.3).
var ErrUnknownSchema = errors.New("cache: unknown cache key schema version")

// knownSchemas lists the supported versions. v2 is accepted only to prove
// version separation (T10.4.2); it hashes the same fields under its own tag.
var knownSchemas = map[int]bool{1: true, 2: true}

func frame(dst []byte, tag string, payload []byte) []byte {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(tag)+len(payload)))
	dst = append(dst, n[:]...)
	dst = append(dst, tag...)
	return append(dst, payload...)
}

func intBytes(v int) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

func optional(p *string) []byte {
	if p == nil {
		return []byte{0}
	}
	return append([]byte{1}, *p...)
}

func sum(buf []byte) string {
	s := sha256.Sum256(buf)
	return hex.EncodeToString(s[:])
}

// Layer names one reusable stage of the layered cache (WBS 10.55).
type Layer string

const (
	// LayerCanonical: snapshot + role + state + template + protocol (+ the
	// live composition inputs) → canonical prompt.
	LayerCanonical Layer = "CANONICAL"
	// LayerCompression: canonical hash + compression config → accepted
	// compression (Phase 9 request identity).
	LayerCompression Layer = "COMPRESSION"
	// LayerWorker: the full semantic input → worker-ready prompt.
	LayerWorker Layer = "WORKER"
	// LayerFragment: a reusable static prompt fragment (WBS 10.45-10.48).
	LayerFragment Layer = "FRAGMENT"
)

// canonicalFields frames the composition inputs (Layer 1). The order is
// fixed by code, never by map iteration, so input ordering cannot affect
// the key (T10.3.2).
func canonicalFields(buf []byte, in PromptCacheInput) []byte {
	buf = frame(buf, "project_id", []byte(in.ProjectID.String()))
	buf = frame(buf, "context_snapshot_hash", []byte(in.ContextSnapshotHash))
	buf = frame(buf, "state_machine_id", []byte(in.StateMachineID.String()))
	buf = frame(buf, "state_machine_version", intBytes(in.StateMachineVersion))
	buf = frame(buf, "state_key", []byte(in.StateKey))
	buf = frame(buf, "role_hash", []byte(in.RoleHash))
	buf = frame(buf, "template_hash", []byte(in.TemplateHash))
	buf = frame(buf, "response_protocol_hash", []byte(in.ResponseProtocolHash))
	buf = frame(buf, "work_unit_spec_hash", []byte(in.WorkUnitSpecHash))
	buf = frame(buf, "state_instruction_hash", []byte(in.StateInstructionHash))
	buf = frame(buf, "phase_wbs_hash", []byte(in.PhaseWBSHash))
	buf = frame(buf, "planning_config_hash", []byte(in.PlanningConfigHash))
	buf = frame(buf, "continuation_status_hash", optional(in.ContinuationStatusHash))
	return buf
}

func checkSchema(version int) error {
	if !knownSchemas[version] {
		return fmt.Errorf("%w: %d", ErrUnknownSchema, version)
	}
	return nil
}

func prefix(version int, layer Layer) string {
	l := "prompt-cache"
	if layer == LayerCanonical {
		l = "canonical-cache"
	}
	return fmt.Sprintf("%s:v%d:", l, version)
}

// CacheKey is the worker-prompt (Layer 3) cache key (WBS 10.3/10.4):
// `prompt-cache:v<schema>:<sha256>` over the schema version and every
// semantic input — the composition inputs plus the project configuration,
// compression configuration and preparation policy.
func CacheKey(in PromptCacheInput, schema int) (string, error) {
	if err := checkSchema(schema); err != nil {
		return "", err
	}
	if _, err := NewPromptCacheInput(in); err != nil {
		return "", err
	}
	buf := frame(nil, "schema", intBytes(schema))
	buf = frame(buf, "layer", []byte(LayerWorker))
	buf = canonicalFields(buf, in)
	buf = frame(buf, "project_config_hash", []byte(in.ProjectConfigHash))
	buf = frame(buf, "compression_config_hash", []byte(in.CompressionConfigHash))
	buf = frame(buf, "preparation_policy", []byte(in.PreparationPolicy))
	return prefix(schema, LayerWorker) + sum(buf), nil
}

// CanonicalKey is the Layer 1 key (WBS 10.54/10.55): only the inputs that
// determine the canonical prompt, so a compression-configuration or
// worker-context change reuses the canonical prompt.
func CanonicalKey(in PromptCacheInput, schema int) (string, error) {
	if err := checkSchema(schema); err != nil {
		return "", err
	}
	if _, err := NewPromptCacheInput(in); err != nil {
		return "", err
	}
	buf := frame(nil, "schema", intBytes(schema))
	buf = frame(buf, "layer", []byte(LayerCanonical))
	buf = canonicalFields(buf, in)
	return prefix(schema, LayerCanonical) + sum(buf), nil
}

// SchemaOf returns the schema version encoded in a key's prefix (audit of
// historical entries, T10.4.4).
func SchemaOf(key string) (int, error) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || !strings.HasPrefix(parts[1], "v") {
		return 0, fmt.Errorf("%w: malformed key", ErrUnknownSchema)
	}
	var v int
	if _, err := fmt.Sscanf(parts[1], "v%d", &v); err != nil {
		return 0, fmt.Errorf("%w: malformed key", ErrUnknownSchema)
	}
	return v, nil
}
