package prompt

import (
	"crypto/sha256"
	"encoding/hex"
)

// CacheKeyInput is the deterministic input set Phase 10 will use for cache
// keys (WBS 8.34). Every field is a CAPTURED/pinned value — the cache key
// never reads "latest" state:
//
//   - ProjectConfigurationVersion: the pinned project configuration
//     version.
//   - StateMachineVersion + StateKey: the pinned identity of the state the
//     prompt was composed for.
//   - RoleConfigurationHash: the role configuration hash (WBS 5.16) — a
//     role edit that moves the hash moves the cache key (T8.34.2).
//   - TemplateVersion + TemplateHash: the pinned template identity (a
//     template edit moves it, T8.34.4).
//   - ExecutionSnapshotHash: the context snapshot's content hash — it
//     stands in for the WBS/state files the snapshot pins, so a
//     WBS/state-file edit moves the cache key (T8.34.3).
//   - ResponseProtocolVersion + ResponseProtocolHash: the pinned
//     response-protocol identity.
//   - ContinuationStatusHash: the captured continuation status's hash;
//     empty string = no continuation status (a fresh prompt pins "").
type CacheKeyInput struct {
	ProjectConfigurationVersion int

	StateMachineVersion int
	StateKey            string

	RoleConfigurationHash string

	TemplateVersion int
	TemplateHash    string

	ExecutionSnapshotHash string

	ResponseProtocolVersion int
	ResponseProtocolHash    string

	ContinuationStatusHash string
}

// CacheKey computes the deterministic prompt cache key (WBS 8.34): the hex
// SHA-256 of a per-field length-prefixed serialization of the cache-input
// set — the same framing as CanonicalPromptHash, so both share one hash
// domain and field boundaries are unambiguous (no input can cancel
// another's contribution). Repeated computation over the same input is
// byte-identical (T8.34.1); a change to any single field moves the key
// (T8.34.2-T8.34.4). Full compressed-prompt caching is Phase 10 scope —
// this exposes the inputs only.
func CacheKey(in CacheKeyInput) (string, error) {
	buf := make([]byte, 0, 256)
	buf = frameField(buf, "project_configuration_version", intBytes(in.ProjectConfigurationVersion))
	buf = frameField(buf, "state_machine_version", intBytes(in.StateMachineVersion))
	buf = frameField(buf, "state_key", []byte(in.StateKey))
	buf = frameField(buf, "role_configuration_hash", []byte(in.RoleConfigurationHash))
	buf = frameField(buf, "template_version", intBytes(in.TemplateVersion))
	buf = frameField(buf, "template_hash", []byte(in.TemplateHash))
	buf = frameField(buf, "execution_snapshot_hash", []byte(in.ExecutionSnapshotHash))
	buf = frameField(buf, "response_protocol_version", intBytes(in.ResponseProtocolVersion))
	buf = frameField(buf, "response_protocol_hash", []byte(in.ResponseProtocolHash))
	buf = frameField(buf, "continuation_status_hash", []byte(in.ContinuationStatusHash))
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}
