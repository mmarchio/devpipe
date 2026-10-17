package prompt

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

// CanonicalPromptHashInput is the hash-input set for the canonical prompt
// hash (WBS 8.27). Every field is a pinned, semantic input — the hash never
// sees "latest" values:
//
//   - Content: the final rendered canonical prompt, byte-for-byte. A
//     continuation-status change rides here (the continuation component is
//     part of the render), covering T8.27.5.
//   - TemplateVersion: the pinned template version.
//   - SnapshotHash: the context snapshot's content hash — it stands in for
//     the state files the snapshot pins, covering T8.27.2 (a state-file edit
//     moves the snapshot hash, which moves this).
//   - ResponseProtocolVersion: the pinned response-protocol version.
//   - RoleVersion: the pinned role version — a role-instruction change
//     advances it, covering T8.27.4.
//   - StateMachineVersion + StateKey: the pinned identity of the state the
//     prompt was composed for.
type CanonicalPromptHashInput struct {
	Content                 string
	TemplateVersion         int
	SnapshotHash            string
	ResponseProtocolVersion int
	RoleVersion             int
	StateMachineVersion     int
	StateKey                string
}

// CanonicalPromptHash computes the deterministic canonical prompt hash
// (WBS 8.27): the hex SHA-256 of a length-prefixed serialization of the
// hash-input set. Same inputs → same hash; a change to any single input
// moves the hash. The serialization is per-field length-prefixed (a field
// tag + an 8-byte big-endian length + the raw bytes) so field boundaries are
// unambiguous and no input can cancel another's contribution. The whole
// digest is a single sha256 over the frame — the same SHA-256 primitive as
// P8-02's ContentHash — so this and ContentHash share one hash domain.
func CanonicalPromptHash(in CanonicalPromptHashInput) (string, error) {
	if in.Content == "" {
		return "", fmt.Errorf("prompt: canonical prompt hash requires exact content")
	}
	buf := make([]byte, 0, 256)
	buf = frameField(buf, "content", []byte(in.Content))
	buf = frameField(buf, "template_version", intBytes(in.TemplateVersion))
	buf = frameField(buf, "snapshot_hash", []byte(in.SnapshotHash))
	buf = frameField(buf, "response_protocol_version", intBytes(in.ResponseProtocolVersion))
	buf = frameField(buf, "role_version", intBytes(in.RoleVersion))
	buf = frameField(buf, "state_machine_version", intBytes(in.StateMachineVersion))
	buf = frameField(buf, "state_key", []byte(in.StateKey))
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// frameField appends a length-prefixed field: an 8-byte big-endian length of
// the tag+payload, then the tag, then the payload. The length covers the tag
// and the payload together, so frames are self-delimiting and the stream
// parses unambiguously (no field boundary is ambiguous, and "a","bc" cannot
// collide with "ab","c").
func frameField(dst []byte, tag string, payload []byte) []byte {
	n := len(tag) + len(payload)
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(n))
	dst = append(dst, lenBuf[:]...)
	dst = append(dst, tag...)
	dst = append(dst, payload...)
	return dst
}

// intBytes renders a version as an 8-byte big-endian value framed by the
// field tag in frameField, so integer inputs are framed exactly like string
// inputs.
func intBytes(v int) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}
