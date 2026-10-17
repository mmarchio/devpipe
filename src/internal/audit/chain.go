package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Tamper-evident audit chaining (Phase 18, WBS 18.105–18.109): every event
// belongs to a chain (one per project, plus "system" for project-less
// events) and stores event_hash = sha256(prev_hash ‖ canonical(event)).

// SystemChain is the chain of project-less events.
const SystemChain = "system"

// ChainKey returns the chain an event belongs to.
func ChainKey(projectID *uuid.UUID) string {
	if projectID == nil {
		return SystemChain
	}
	return "project:" + projectID.String()
}

// Genesis is the deterministic previous-hash of a chain's first event
// (WBS 18.106).
func Genesis(chainKey string) string {
	sum := sha256.Sum256([]byte("overseer-audit-genesis-v1:" + chainKey))
	return hex.EncodeToString(sum[:])
}

// StoredPrecision is the timestamp precision PostgreSQL keeps; events are
// truncated to it before hashing so a stored row re-hashes identically.
const StoredPrecision = time.Microsecond

type canonicalEvent struct {
	Chain        string         `json:"chain"`
	ChainSeq     int64          `json:"chain_seq"`
	ID           string         `json:"id"`
	At           string         `json:"at"`
	ProjectID    string         `json:"project_id"`
	ProjectName  string         `json:"project_name"`
	ExecutionID  string         `json:"execution_id"`
	PhaseID      string         `json:"phase_id"`
	WorkUnitID   string         `json:"work_unit_id"`
	ActorID      string         `json:"actor_id"`
	ActorName    string         `json:"actor_name"`
	ActorType    string         `json:"actor_type"`
	Action       string         `json:"action"`
	ResourceKind string         `json:"resource_kind"`
	ResourceID   string         `json:"resource_id"`
	Result       string         `json:"result"`
	HTTPStatus   int            `json:"http_status"`
	Reason       string         `json:"reason"`
	RequestID    string         `json:"request_id"`
	Method       string         `json:"method"`
	Path         string         `json:"path"`
	Metadata     map[string]any `json:"metadata"`
}

func idStr(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

// Canonical is the deterministic serialization hashed into the chain
// (WBS 18.107): fixed field order, UTC microsecond timestamp, metadata
// re-encoded through JSON so map keys are sorted and numbers normalized
// exactly as they come back from JSONB.
func Canonical(chainKey string, chainSeq int64, e Event) ([]byte, error) {
	md := map[string]any{}
	if len(e.Metadata) > 0 {
		raw, err := json.Marshal(e.Metadata)
		if err != nil {
			return nil, fmt.Errorf("audit: metadata not serializable: %w", err)
		}
		if err := json.Unmarshal(raw, &md); err != nil {
			return nil, err
		}
	}
	c := canonicalEvent{Chain: chainKey, ChainSeq: chainSeq, ID: e.ID.String(), At: e.At.UTC().Truncate(StoredPrecision).Format(time.RFC3339Nano),
		ProjectID: idStr(e.ProjectID), ProjectName: e.ProjectName, ExecutionID: idStr(e.ExecutionID), PhaseID: idStr(e.PhaseID),
		WorkUnitID: idStr(e.WorkUnitID), ActorID: e.Actor.ID, ActorName: e.Actor.Name, ActorType: string(e.Actor.Type),
		Action: string(e.Action), ResourceKind: e.ResourceKind, ResourceID: e.ResourceID, Result: string(e.Result),
		HTTPStatus: e.HTTPStatus, Reason: e.Reason, RequestID: e.RequestID, Method: e.Method, Path: e.Path, Metadata: md}
	return json.Marshal(c)
}

// EventHash links canonical to its predecessor (WBS 18.105).
func EventHash(prevHash string, canonical []byte) string {
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write([]byte{0})
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))
}

// ChainLink computes the stored chain fields of an event.
func ChainLink(chainKey string, chainSeq int64, prevHash string, e Event) (string, error) {
	c, err := Canonical(chainKey, chainSeq, e)
	if err != nil {
		return "", err
	}
	return EventHash(prevHash, c), nil
}

// ChainRow is one stored event as the verifier reads it.
type ChainRow struct {
	ChainSeq  int64
	PrevHash  string
	EventHash string
	Event     Event
}

// Verification statuses (WBS 18.110).
type VerifyStatus string

const (
	ChainValid      VerifyStatus = "VALID"
	ChainInvalid    VerifyStatus = "INVALID"
	ChainIncomplete VerifyStatus = "INCOMPLETE"
)

// Gap is a missing chain_seq range [From, To].
type Gap struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// ChainResult is the verification of one chain.
type ChainResult struct {
	Chain         string       `json:"chain"`
	Status        VerifyStatus `json:"status"`
	Events        int          `json:"events"`
	FirstFailing  int64        `json:"first_failing_seq,omitempty"`
	FailureReason string       `json:"failure_reason,omitempty"`
	Gaps          []Gap        `json:"gaps,omitempty"`
}

// VerifyChain checks rows (sorted by chain_seq) of one chain: sequence
// continuity from 1 (gaps → INCOMPLETE, WBS 18.109) and every link and
// hash (mismatch → INVALID at the first failing seq, WBS 18.108). After a
// gap the next row's stored predecessor is taken as given so later
// tampering is still located.
func VerifyChain(chainKey string, rows []ChainRow) ChainResult {
	res := ChainResult{Chain: chainKey, Status: ChainValid, Events: len(rows)}
	prev := Genesis(chainKey)
	expect := int64(1)
	fail := func(seq int64, why string) {
		if res.Status != ChainInvalid {
			res.Status, res.FirstFailing, res.FailureReason = ChainInvalid, seq, why
		}
	}
	for _, r := range rows {
		if r.ChainSeq < expect {
			fail(r.ChainSeq, "duplicate or out-of-order chain sequence")
			continue
		}
		if r.ChainSeq > expect {
			res.Gaps = append(res.Gaps, Gap{From: expect, To: r.ChainSeq - 1})
			if res.Status == ChainValid {
				res.Status, res.FirstFailing, res.FailureReason = ChainIncomplete, expect, "missing events"
			}
			prev = r.PrevHash
		}
		if r.PrevHash != prev {
			fail(r.ChainSeq, "previous-hash link broken")
		}
		h, err := ChainLink(chainKey, r.ChainSeq, r.PrevHash, r.Event)
		if err != nil || h != r.EventHash {
			fail(r.ChainSeq, "event content does not match its hash")
		}
		prev = r.EventHash
		expect = r.ChainSeq + 1
	}
	return res
}

// Overall folds chain results: INVALID beats INCOMPLETE beats VALID.
func Overall(results []ChainResult) VerifyStatus {
	st := ChainValid
	for _, r := range results {
		switch r.Status {
		case ChainInvalid:
			return ChainInvalid
		case ChainIncomplete:
			st = ChainIncomplete
		}
	}
	return st
}

// Failures returns the results that are not VALID.
func Failures(results []ChainResult) []ChainResult {
	var out []ChainResult
	for _, r := range results {
		if r.Status != ChainValid {
			out = append(out, r)
		}
	}
	return out
}

// Fingerprint identifies a verification failure (the failing chains, their
// status, first failing seq, reason and gaps) so an acknowledgement covers
// exactly that failure: a new or different one needs its own (WBS 18.110,
// decision U9). It is "" when every chain is VALID.
func Fingerprint(results []ChainResult) string {
	f := Failures(results)
	if len(f) == 0 {
		return ""
	}
	sort.Slice(f, func(i, j int) bool { return f[i].Chain < f[j].Chain })
	type key struct {
		Chain  string       `json:"chain"`
		Status VerifyStatus `json:"status"`
		First  int64        `json:"first"`
		Reason string       `json:"reason"`
		Gaps   []Gap        `json:"gaps"`
	}
	ks := make([]key, len(f))
	for i, r := range f {
		ks[i] = key{r.Chain, r.Status, r.FirstFailing, r.FailureReason, r.Gaps}
	}
	b, _ := json.Marshal(ks)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FailureAck is an operator's acknowledgement of one verification failure.
type FailureAck struct {
	ID          uuid.UUID     `json:"id"`
	Fingerprint string        `json:"fingerprint"`
	Status      VerifyStatus  `json:"status"`
	Failure     []ChainResult `json:"failure"`
	Reason      string        `json:"reason"`
	By          string        `json:"acknowledged_by"`
	ByName      string        `json:"acknowledged_by_name"`
	RequestID   string        `json:"request_id"`
	At          time.Time     `json:"acknowledged_at"`
}
