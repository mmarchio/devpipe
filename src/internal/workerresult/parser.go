package workerresult

import (
	"context"
	"time"

	"overseer/internal/worker"
)

// ParserVersion identifies this parser implementation, independent of the
// worker protocol version (WBS 13.48).
const ParserVersion = "1.0.0"

// WorkerResultParser turns a stored raw response into a WorkerResult
// (WBS 13.8): extraction, syntax, schema, semantics, normalization and
// provenance from the dispatch.
type WorkerResultParser interface {
	Parse(ctx context.Context, response worker.WorkerResponseRecord, dispatch worker.WorkerDispatch) (WorkerResult, error)
}

// DeterministicParser is the protocol parser: no LLM, no network, no
// filesystem. The same bytes and settings always give the same outcome.
type DeterministicParser struct {
	Settings Settings
	// AcceptanceIDs are the dispatched unit's criterion ids (PLAN
	// Decision 5).
	AcceptanceIDs []string
	Now           func() time.Time
}

var _ WorkerResultParser = DeterministicParser{}

// Parse implements WorkerResultParser.
func (p DeterministicParser) Parse(ctx context.Context, response worker.WorkerResponseRecord, d worker.WorkerDispatch) (WorkerResult, error) {
	if err := ctx.Err(); err != nil {
		return WorkerResult{}, err
	}
	// WBS 13.49: the response must belong to this dispatch and project.
	if response.DispatchID != d.ID || response.ProjectID != d.ProjectID {
		return WorkerResult{}, ErrBindingMismatch
	}
	s := p.Settings
	raw := []byte(response.Content)
	if response.ContentHash != worker.ContentHash(response.Content) {
		prov := ResultParseProvenance{Mode: s.ParseMode, RepairMode: s.RepairMode, SourceHash: sha(raw), ParserVersion: ParserVersion}
		return WorkerResult{}, newParseError(ResultMalformed, []ValidationError{{Field: "$", Code: "RAW_HASH_MISMATCH",
			Message: "stored response does not match its hash"}}, prov)
	}
	cand, prov, perr := Extract(raw, s)
	if perr != nil {
		return WorkerResult{}, perr
	}
	tree, sf := parseTree(cand)
	if sf != nil {
		return WorkerResult{}, newParseError(sf.code, []ValidationError{{Field: "$", Code: sf.vcode, Message: sf.message}}, prov)
	}
	d0, errs, unsupported := validateSchema(tree, s)
	if unsupported {
		return WorkerResult{}, newParseError(ResultUnsupportedVersion, errs, prov)
	}
	if len(errs) > 0 {
		return WorkerResult{}, newParseError(ResultSchemaInvalid, errs, prov)
	}
	if sem := validateSemantics(d0); len(sem) > 0 {
		return WorkerResult{}, newParseError(ResultSemanticInvalid, sem, prov)
	}
	recognize(d0.claims, p.AcceptanceIDs)
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now()
	}
	r := WorkerResult{ProtocolVersion: d0.version, Status: d0.status, Summary: d0.summary, Completed: d0.completed, Remaining: d0.remaining,
		Blocking: d0.blocking, NextAction: d0.nextAction, Tests: d0.tests, Artifacts: d0.artifacts, AcceptanceClaims: d0.claims, Plan: d0.plan,
		Metadata: MetadataFor(d, response.ContentHash, now), Provenance: prov}
	r.NormalizedHash = NormalizedHash(r)
	return r, nil
}

// MetadataFor derives result metadata from the authoritative dispatch
// (WBS 13.5/13.49-13.51); nothing comes from worker content.
func MetadataFor(d worker.WorkerDispatch, rawHash string, at time.Time) WorkerResultMetadata {
	return WorkerResultMetadata{DispatchID: d.ID, RequestID: d.RequestID, SessionID: d.SessionID, ProjectID: d.ProjectID,
		PhaseID: d.PhaseID, WorkUnitID: d.WorkUnitID, StateKey: d.StateKey, RoleKey: d.RoleKey, RawResponseHash: rawHash, ParsedAt: at}
}

// Revalidate re-checks an accepted result's content invariants (fuzz /
// persistence round-trip guard): status valid, semantics hold, hash
// matches.
func Revalidate(r WorkerResult) bool {
	if !r.Status.Valid() || r.Summary == "" || r.ProtocolVersion.Compatible() != nil {
		return false
	}
	d := &draft{status: r.Status, summary: r.Summary, remaining: r.Remaining, blocking: r.Blocking, nextAction: r.NextAction}
	return len(validateSemantics(d)) == 0 && NormalizedHash(r) == r.NormalizedHash
}
